# Probing egress from a sandbox actor

How to run arbitrary commands inside an actor and confirm at the gateway that the traffic went through it. The worked example is `ssh -T git@github.com` and `git clone` over SSH, chosen because SSH is not HTTP: it exercises the raw-TCP path through the egress gateway rather than anything HTTP-aware.

## Create the template and an actor

`demos/sandbox/ssh-egress-test.yaml.tmpl` defines a `ssh-egress-test` template in namespace `ate-ssh-egress-test`. It reuses the `demos/sandbox` image, whose `/process` endpoint runs an arbitrary argv, and sets `onPause: Data` / `onCommit: Data` with no DurableDir volumes so a pause writes no filesystem content to the snapshot bucket.

```sh
source .ate-dev-env.sh
sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" demos/sandbox/ssh-egress-test.yaml.tmpl \
  | ./hack/run-tool.sh ko apply -f -
kubectl wait --for=condition=Ready actortemplate/ssh-egress-test -n ate-ssh-egress-test --timeout=300s

./bin/kubectl-ate create atespace demo
./bin/kubectl-ate create actor ssh-probe -a demo --template ate-ssh-egress-test/ssh-egress-test
./bin/kubectl-ate resume actor ssh-probe -a demo
```

## Run commands in the actor

Port-forward the router, then POST to `/process`. The actor is selected by the `Host` header, which `resources.ActorDNSName` builds as `<name>.<atespace>.actors.resources.substrate.ate.dev`:

```sh
kubectl port-forward -n ate-system svc/atenet-router 8000:80 &

curl -sS -X POST http://localhost:8000/process \
  -H "Host: ssh-probe.demo.actors.resources.substrate.ate.dev" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-c","id"]}'
```

The response is `{"stdout","stderr","exitCode","error"}`. Check `exitCode`: a command that fails still returns HTTP 200. A long command can outlast the router's response timeout and return `504` while continuing to run in the actor, so poll for the result rather than retrying.

That response is a single JSON line with the output escaped as `\r\n` and `>`, which is unreadable for anything verbose. This helper decodes it, and builds the request with `jq` so the command needs no JSON quoting:

```sh
ate_run() {  # ate_run <actor>.<atespace> <shell command...>
  local host="$1"; shift
  jq -n --arg cmd "$*" '{command:["sh","-c",$cmd]}' \
    | curl -sS -X POST http://localhost:8000/process \
        -H "Host: ${host}.actors.resources.substrate.ate.dev" \
        -H "Content-Type: application/json" --data-binary @- \
    | jq -r '.stdout, .stderr, (if .exitCode != 0 then "[exit \(.exitCode)]" else empty end) | select(. != "")' \
    | tr -d '\r'
}

ate_run ssh-probe.demo 'id'
```

For a one-off, appending `| jq -r '.stdout, .stderr'` to a raw `curl` is enough.

The container is built on plain `alpine` and runs as root, so `git` and `ssh` are not present but are installable at runtime:

```sh
apk add --no-cache git openssh-client
```

## Probe the network path

No credential is needed. Reaching GitHub's authentication layer already proves the transport works, and `Permission denied (publickey)` means only that no key was offered:

```sh
ate_run ssh-probe.demo 'ssh -vT -o StrictHostKeyChecking=no git@github.com 2>&1' \
  | grep -vE 'load_hostkeys|no pubkey|identity file|no identity'
```

The `grep -v` drops the dozen lines OpenSSH emits per missing key file and per missing `known_hosts` path, which are noise in a keyless actor. Excerpted from the result (`[exit 255]`):

```
debug1: Connecting to github.com [140.82.112.4] port 22.
debug1: Connection established.
debug1: Local version string SSH-2.0-OpenSSH_10.3
debug1: Remote protocol version 2.0, remote software version feb815a
debug1: kex: algorithm: sntrup761x25519-sha512
debug1: Server host key: ssh-ed25519 SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU
debug1: Host 'github.com' is known and matches the ED25519 host key.
debug1: Authentications that can continue: publickey
debug1: No more authentication methods to try.
git@github.com: Permission denied (publickey).
```

The host key matches GitHub's published ed25519 fingerprint and the key exchange completed, so DNS, egress and the SSH transport are all working. Authentication is the only thing that failed, because the actor holds no key.

## Confirm the traffic crossed the gateway

The actor's own view cannot distinguish "the gateway tunneled this" from "the gateway was never in the path". Confirm at the gateway, whose access log flushes as soon as a tunnel is established (`manifests/ate-install/atenet-egress.yaml:83-93`):

```sh
kubectl logs -n ate-system deploy/atenet-egress -c envoy | grep '\[egress\]'
```

```
[egress] authority=140.82.112.4:22 peer_san=spiffe://substrate-actor.local/atespace/demo/actor/ssh-probe code=200 flags=- up_bytes=0 down_bytes=0
[egress] authority=140.82.112.4:22 peer_san=spiffe://substrate-actor.local/atespace/demo/actor/ssh-probe code=200 flags=DC up_bytes=3022 down_bytes=2749
```

`authority=...:22` is the terminated CONNECT, `code=200` means ext_proc authorized it, `peer_san` is the actor identity from the mTLS client cert, and the byte counts on the `DC` record show the tunnel carried bidirectional traffic.

## What this establishes

Actors reach the gateway over mTLS on `:443`, atunnel issues an HTTP `CONNECT` carrying the destination as `IP:port`, and Envoy terminates it and pipes raw TCP (`manifests/ate-install/atenet-egress.yaml:14-19`). The SSH result exercises parts of that path HTTP cannot:

- **SSH is server-speaks-first.** The client reads the server's banner before sending anything, so a tunnel assuming client-first framing, or routing by SNI or an HTTP `Host`, would deadlock.
- **The payload is neither HTTP nor TLS**, so it uses the raw-TCP path after the CONNECT upgrade.
- **Port 22 is not a web port.**

CONNECT tunnels TCP by construction, so this says nothing about UDP. "Non-HTTP" is not "non-TCP".

### The gateway authorizes who, not where

Arbitrary destinations and ports are reachable:

```
$ nc -vz -w 8 1.1.1.1 53          # succeeded
$ nc -vz -w 8 smtp.gmail.com 587  # succeeded
[egress] authority=1.1.1.1:53 peer_san=spiffe://.../actor/ssh-probe code=200 ...
[egress] authority=142.251.183.109:587 peer_san=spiffe://.../actor/ssh-probe code=200 ...
```

ext_proc re-verifies the actor certificate and asks the ate API whether that UID is a real, running actor; nothing in the default variant consults the destination. The gateway is an identity gate and an audit point, not a destination allowlist. The denial worth testing is therefore an invalid or absent actor identity, which ext_proc answers with a 403 and atunnel surfaces as `ConnectRejectedError` (`internal/atunnel/client.go:50-64`).

## Authenticating to GitHub from the actor

Only needed to clone a private repo or push; public repos clone over HTTPS with no credential.

> **Warning:** prefer a throwaway key registered as a read-only deploy key (`gh repo deploy-key add`, requires repo `admin`) over your personal key, so it can be revoked on its own. Any key in an actor is exposed to whatever the template snapshots.

Pass the key in the `envvars` field rather than interpolating it into the command string, so it stays out of shell history and process lists:

```sh
umask 077
jq -n --rawfile key /tmp/actor-key \
  '{command:["sh","-c","mkdir -p /root/.ssh && chmod 700 /root/.ssh && printf %s \"$ACTOR_SSH_KEY\" > /root/.ssh/id_ed25519 && chmod 600 /root/.ssh/id_ed25519 && ssh-keygen -lf /root/.ssh/id_ed25519"], envvars:{ACTOR_SSH_KEY:$key}}' > /tmp/payload.json
curl -sS -X POST http://localhost:8000/process \
  -H "Host: ssh-probe.demo.actors.resources.substrate.ate.dev" \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/payload.json
shred -u /tmp/payload.json
```

Compare the fingerprint it prints against `ssh-keygen -lf <key>.pub` to confirm the transfer. With a registered key, `ssh -T git@github.com` reports success and exits 1, which is GitHub refusing shell access rather than a failure:

```
Hi haiyanmeng! You've successfully authenticated, but GitHub does not provide shell access.
```

## Clean up

```sh
./bin/kubectl-ate delete actor ssh-probe -a demo
kubectl delete namespace ate-ssh-egress-test
```
