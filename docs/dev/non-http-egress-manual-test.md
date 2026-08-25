# Manual test: non-HTTP egress

Confirms that SSH — traffic that is neither HTTP nor TLS — reaches the internet
through the MITM egress gateway and lands on the passthrough filter chain.

Uses the sandbox demo, whose `/process` endpoint runs arbitrary argv inside the
actor, so no purpose-built fixture is needed.

## 1. Install

```sh
hack/install-ate-kind.sh --deploy-ate-system --experimental-use-sdsmint
hack/install-ate.sh --deploy-demo-sandbox
kubectl wait --for=condition=Ready actortemplate/sandbox-template -n ate-demo-sandbox --timeout=5m
```

`--experimental-use-sdsmint` selects `manifests/ate-install/atenet-egress-with-sdsmint.yaml`
instead of the shipped gateway. Without it there is no MITM listener and nothing
to test.

## 2. Create an actor

```sh
kubectl ate create atespace demo
kubectl ate create actor my-sandbox-1 -a demo --template ate-demo-sandbox/sandbox-template
kubectl ate resume actor my-sandbox-1 -a demo

kubectl port-forward -n ate-system svc/atenet-router 8000:80 &
```

Do not use `demos/sandbox/client`. It dials ateapi with no credentials and fails
with `Unauthenticated: missing bearer token`.

## 3. Helper

`/process` takes JSON and returns `{"stdout","stderr","exitCode"}` with the
output escaped. Build the request with `jq` so commands need no quoting:

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
```

A command that fails still returns HTTP 200, so check `exitCode`. A slow command
can outlast the router timeout and return 504 while still running in the actor.

## 4. Install ssh in the actor

The image is plain Alpine: no `ssh`, no `git`, no `curl`. Installing them needs
egress, and HTTPS egress does not work yet — the gateway mints a leaf off a CA
the actor does not trust, so `apk` fails with `TLS: server certificate not
trusted`. Alpine signs packages independently of the transport, so switch the
repos to HTTP:

```sh
ate_run my-sandbox-1.demo "sed -i 's|https|http|g' /etc/apk/repositories && apk add --no-cache openssh-client"
```

## 5. Run SSH

Copy in the key you already use for GitHub. Check first that it works and has no
passphrase — the actor has no agent and nothing to prompt at:

```sh
ssh -T -o BatchMode=yes -i ~/.ssh/id_ed25519 git@github.com     # -> Hi <user>!
ssh-keygen -y -P '' -f ~/.ssh/id_ed25519 >/dev/null && echo "no passphrase"
```

If it is passphrase-protected, strip it from a *copy*, never the original:

```sh
cp ~/.ssh/id_ed25519 /tmp/actor-key && ssh-keygen -p -N '' -f /tmp/actor-key
```

Pass it in `envvars`, not interpolated into the command string, so it stays out
of shell history and process lists:

```sh
umask 077
jq -n --rawfile key ~/.ssh/id_ed25519 '{
  command:["sh","-c","mkdir -p /root/.ssh && chmod 700 /root/.ssh && printf %s \"$ACTOR_SSH_KEY\" > /root/.ssh/id_ed25519 && chmod 600 /root/.ssh/id_ed25519 && ssh-keygen -lf /root/.ssh/id_ed25519"],
  envvars:{ACTOR_SSH_KEY:$key}}' > /tmp/payload.json

curl -sS -X POST http://localhost:8000/process \
  -H "Host: my-sandbox-1.demo.actors.resources.substrate.ate.dev" \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/payload.json | jq -r '.stdout, .stderr'

shred -u /tmp/payload.json
```

The fingerprint it prints must match `ssh-keygen -lf ~/.ssh/id_ed25519.pub`.
Then connect:

```sh
ate_run my-sandbox-1.demo 'ssh -T -o StrictHostKeyChecking=no git@github.com 2>&1'
```

```
Hi haiyanmeng! You've successfully authenticated, but GitHub does not provide shell access.
```

**Exit 1 is the success case** — GitHub is refusing a shell, not refusing you.

For the transport detail, add `-v`:

```sh
ate_run my-sandbox-1.demo 'ssh -vT -o StrictHostKeyChecking=no git@github.com 2>&1' \
  | grep -vE 'load_hostkeys|no pubkey|identity file|no identity'
```

The host key must be GitHub's published fingerprint
`SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU`, which confirms the session
was not intercepted — the passthrough chain is meant to be blind.

Without a key you get `Permission denied (publickey)` and exit 255 instead. That
also proves the transport works, since it comes after a completed key exchange.

> **Your personal key leaves the machine.** `sandbox-template` defaults to
> `Full` snapshot scope, so the key is copied into the GCS snapshot bucket on
> every pause and outlives the actor. Either delete the actor without pausing
> it, or use a template with `onPause: Data` / `onCommit: Data` and no DurableDir
> volumes — `demos/sandbox/ssh-egress-test.yaml.tmpl` is exactly that, and writes
> no filesystem content at all. For anything longer-lived, register a throwaway
> deploy key instead (`gh repo deploy-key add`) so it can be revoked on its own.

## 6. Check the gateway

The gateway logs two formats on one stream: the CONNECT leg as text, the three
MITM legs as JSON. Filter before parsing.

```sh
kubectl logs -n ate-system deploy/atenet-egress -c envoy --tail=400 \
  | jq -Rc 'fromjson? // empty | select(.leg)' | tail
```

SSH must appear as `passthrough`:

```json
{"leg":"passthrough","sni":null,"upstream":"140.82.113.3:22",
 "bytes_rcvd":3022,"bytes_sent":2749,"flags":"-"}
```

- `leg` is `passthrough`, not `mitm` or `cleartext`. Before the fix, non-HTTP
  traffic was parsed as HTTP, dropped, and logged nowhere at all.
- `upstream` is the IP:port from the CONNECT, so `original_dst_address` survived
  the internal hop. This chain has no Host or SNI to fall back on.
- Both byte counts are non-zero, so a real session crossed it.

The apk traffic from step 4 should show up as `leg: cleartext`, confirming the
new fallback chain did not swallow HTTP. And the CONNECT leg authorizes both:

```sh
kubectl logs -n ate-system deploy/atenet-egress -c envoy --tail=400 | grep '\[egress\]'
```

```
[egress] authority=140.82.113.3:22 peer_san=spiffe://substrate-actor.local/atespace/demo/actor/my-sandbox-1 code=200 flags=DC up_bytes=3022 down_bytes=2749
```

## 7. Clone a repo over SSH

`git` is not in the image either, and GitHub requires a key on port 22 even for
a public repo, so this reuses the one from step 5:

```sh
ate_run my-sandbox-1.demo 'apk add --no-cache git && git clone git@github.com:agent-substrate/substrate.git /tmp/sub1 && ls /tmp/sub1 | head'
```

The gateway shows the same `leg: passthrough` on `:22`, with byte counts the
size of the repo — a key changes authentication, not transport:

```json
{"leg":"passthrough","upstream":"140.82.114.3:22",
 "bytes_rcvd":251238,"bytes_sent":84006017,"duration_ms":7783,"flags":"-"}
```

This is the record that shows the chain carries sustained bulk transfer, not
just enough bytes to reach an auth prompt. A full clone of this repo takes about
8s; on a slower link one can outlast the router timeout and return 504 while
still running, so poll with `ls` rather than retrying, or use `--depth 1`.

## 8. Server-speaks-first

SSH sends its banner first, so it does not exercise the listener-filter timeout.
For that, use a protocol where the server greets first:

```sh
ate_run my-sandbox-1.demo 'sleep 6 | nc -w 8 smtp.gmail.com 25'
```

```
220 smtp.gmail.com ESMTP 5614622812f47-4b2d68769e1sm1723520b6e.8 - gsmtp
```

The `sleep` is required. `/process` gives the command no stdin, and busybox `nc`
closes the socket as soon as stdin hits EOF — without it `nc` exits 0 having
printed nothing, before the greeting arrives.

The gateway record proves the timeout path was taken:

```json
{"leg":"passthrough","upstream":"173.194.206.108:25","bytes_rcvd":0,"bytes_sent":74}
```

`bytes_rcvd: 0` means the client sent nothing at all, so neither listener filter
had anything to inspect and only `listener_filters_timeout` +
`continue_on_listener_filters_timeout` could have moved the connection onto this
chain.

## Clean up

```sh
kubectl ate pause actor my-sandbox-1 -a demo
kubectl ate delete actor my-sandbox-1 -a demo
```
