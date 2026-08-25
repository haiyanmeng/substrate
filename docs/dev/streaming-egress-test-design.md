# Testing gRPC, WebSocket, and SSE egress from a sandbox actor

## 1. Setup

**Step 0 — an actor called `probe`.** Every command addresses `probe.demo`, so
it must exist under that name. Installing the demo is separate from installing
the system: a cluster built with `--deploy-ate-system` alone has no
`sandbox-template`, and the router answers `actor demo/probe not found` with a
404.

```sh
hack/install-ate.sh --deploy-demo-sandbox          # or hack/install-ate-kind.sh on kind
kubectl wait --for=condition=Ready actortemplate/sandbox-template -n ate-demo-sandbox --timeout=6m

kubectl ate create atespace demo                   # skip if it already exists
kubectl ate create actor probe -a demo --template ate-demo-sandbox/sandbox-template
kubectl ate resume actor probe -a demo

kubectl port-forward -n ate-system svc/atenet-router 8000:80 &
```

The actor reports `STATUS_UNSPECIFIED` with no ateom pod even when it is
healthy; that field is not populated by this API version. Smoke-test instead:

```sh
curl -sS -X POST http://localhost:8000/process \
  -H "Host: probe.demo.actors.resources.substrate.ate.dev" \
  -H "Content-Type: application/json" \
  -d '{"command":["sh","-c","echo hello"]}'
```

For a longer-lived probe, prefer a template with `onPause: Data` and no
DurableDir volumes — `demos/sandbox/ssh-egress-test.yaml.tmpl` is one — so the
tools installed below are not copied into the snapshot bucket on every pause.

Then the `ate_run` helper. It checks the router's reply is JSON before parsing:
without that, a 404 or a 504 surfaces as `jq: parse error: Invalid numeric
literal`, which points at `jq` rather than at the actor.

```sh
ate_run() {  # ate_run <actor>.<atespace> <shell command...>
  local host="$1"; shift
  local resp
  resp=$(jq -n --arg cmd "$*" '{command:["sh","-c",$cmd]}' \
    | curl -sS -m 300 -X POST http://localhost:8000/process \
        -H "Host: ${host}.actors.resources.substrate.ate.dev" \
        -H "Content-Type: application/json" --data-binary @-)
  if ! printf '%s' "$resp" | jq -e . >/dev/null 2>&1; then
    printf 'not JSON, so this never reached the actor: %s\n' "$resp" >&2
    return 1
  fi
  printf '%s' "$resp" \
    | jq -r '.stdout, .stderr, (if .exitCode != 0 then "[exit \(.exitCode)]" else empty end) | select(. != "")' \
    | tr -d '\r'
}
```

**Step 1 — extract the MITM root, on the host.** `RootCertificatePEM` is empty
in practice; `RootCertificateDER` is the populated field.

```sh
kubectl get secret egress-mitm-ca-pool -n ate-system -o jsonpath='{.data.pool}' \
  | base64 -d \
  | jq -r '.CAs[] | select(.ID=="mitm") | .RootCertificateDER' \
  | base64 -d \
  | openssl x509 -inform DER -out /tmp/mitm-ca.pem
openssl x509 -in /tmp/mitm-ca.pem -noout -subject   # CN=substrate egress MITM CA
```

The pool blob also contains the signing key. Do not pass the whole blob into the
actor — extract the certificate on the host, as above, and inject only that.

**Step 2 — repos to HTTP.** No TLS is available in the actor yet. Alpine signs
packages independently of the transport, so this weakens nothing that matters.

```sh
ate_run probe.demo 'sed -i "s|https|http|g" /etc/apk/repositories'
```

**Step 3 — install what Alpine has.** Cleartext, so it works before any trust is
established. This is also the traffic that should appear as `leg: cleartext` in
the gateway log.

```sh
ate_run probe.demo 'apk add --no-cache curl ca-certificates websocat'
```

`websocat` is in the community repo (1.14.1). `grpcurl` is not packaged and has
to be fetched in step 5.

**Step 4 — install the MITM root into the actor's trust store.** No egress: the
certificate travels in the request body. `ate_run` only takes a command, so this
call goes to `/process` directly with an `envvars` map — the same pattern the SSH
document uses for a private key, keeping the material out of shell history and
out of the actor's process list.

```sh
jq -n --rawfile ca /tmp/mitm-ca.pem '{
  command:["sh","-c","printf %s \"$MITM_CA\" > /usr/local/share/ca-certificates/mitm-ca.crt && update-ca-certificates"],
  envvars:{MITM_CA:$ca}}' \
| curl -sS -X POST http://localhost:8000/process \
    -H "Host: probe.demo.actors.resources.substrate.ate.dev" \
    -H "Content-Type: application/json" --data-binary @- \
| jq -r '.stdout, .stderr'
```

The system trust store rather than a loose `--cacert` file, because `websocat`
has no flag to point at one. With it installed, every client below works
unmodified and no test command needs a `--cacert`.

**Step 5 — fetch grpcurl.** Needs HTTPS, which is why it is last. It doubles as
a bootstrap assertion: a verified-chain HTTPS transfer through the MITM.

```sh
ate_run probe.demo 'ver=1.9.3; curl -fsSL -o /tmp/g.tgz \
  "https://github.com/fullstorydev/grpcurl/releases/download/v${ver}/grpcurl_${ver}_linux_x86_64.tar.gz" \
  && tar -xzf /tmp/g.tgz -C /usr/local/bin grpcurl && grpcurl --version'
```

Written to a file rather than piped: busybox `tar` does not reliably extract a
named member from a non-seekable stream. `/usr/local/bin` is writable.

## 2. The tests

Targets are public, which is fine for a manual procedure and not for a presubmit
lane: `grpcb.in:9001` (gRPC/TLS, with reflection so no `.proto` is needed),
`grpcb.in:9000` (the same service over h2c), `wss://echo.websocket.org`, and
`https://stream.wikimedia.org/v2/stream/recentchange`.

### 2.0 Pre-flight: does the leg speak h2 at all?

```sh
ate_run probe.demo 'curl -sS -m 20 -o /dev/null \
  -w "http_version=%{http_version} code=%{http_code}\n" https://example.com'
```

Expect `http_version=2 code=200`. That single line says the connection was
intercepted, the injected root verified the minted leaf, and ALPN negotiated
HTTP/2. If this fails, nothing below will pass.

### 2.1 gRPC over TLS

```sh
ate_run probe.demo 'grpcurl -max-time 30 grpcb.in:9001 list'
ate_run probe.demo 'grpcurl -max-time 30 -d "{\"f_string\":\"hello-mitm\"}" grpcb.in:9001 grpcbin.GRPCBin/DummyUnary'
```

Reflection is itself a bidirectional stream, so `list` succeeding already proves
HTTP/2 and trailers survive the round trip. Expect the service list, then
`{"fString": "hello-mitm"}`.

### 2.2 gRPC over h2c

```sh
ate_run probe.demo 'grpcurl -max-time 30 -plaintext -d "{\"f_string\":\"h2c-hello\"}" grpcb.in:9000 grpcbin.GRPCBin/DummyUnary'
```

Exercises `use_downstream_protocol_config` on the cleartext cluster.

### 2.3 gRPC server-streaming — detached

`DummyServerStream` takes about 10s, which is on the wrong side of the route
timeout, so this one has to be detached even though it is not a long test.

```sh
ate_run probe.demo 'rm -f /tmp/g4.log; setsid sh -c "grpcurl -max-time 90 \
  -d {\\\"f_string\\\":\\\"stream\\\"} grpcb.in:9001 grpcbin.GRPCBin/DummyServerStream \
  > /tmp/g4.log 2>&1; echo EXIT=\$? >> /tmp/g4.log" < /dev/null > /dev/null 2>&1 & echo started'

sleep 45
ate_run probe.demo 'echo "messages: $(grep -c fString /tmp/g4.log)"; tail -1 /tmp/g4.log'
```

Expect `messages: 10` and `EXIT=0`.

### 2.4 WebSocket over TLS

```sh
ate_run probe.demo '{ printf "hello-echo-1\n"; sleep 3; printf "hello-echo-2\n"; sleep 3; } \
  | timeout 20 websocat wss://echo.websocket.org'
```

Both messages must come back. Do not use `websocat -n1` for this — it prints the
server's greeting and exits before the echo arrives, so a broken upgrade path
looks identical to a working one.

### 2.5 SSE over TLS

```sh
ate_run probe.demo 'curl -sS -N -m 8 -H "Accept: text/event-stream" \
  https://stream.wikimedia.org/v2/stream/recentchange | grep -c "^event:"'

ate_run probe.demo 'curl -sS -N -m 6 -o /dev/null -H "Accept: text/event-stream" \
  -w "time_starttransfer=%{time_starttransfer}s time_total=%{time_total}s size=%{size_download}\n" \
  https://stream.wikimedia.org/v2/stream/recentchange'
```

The first counts events; the second is the one that proves the body is *not*
buffered to completion. A `time_starttransfer` far below `time_total`, with a
large `size_download`, is incremental delivery.

### 2.6 Idle stream survival — detached

Must outlast both the 10s route timeout and the 5-minute idle timeout under
test. Run both variants together so they share one idle window and differ only
in the keepalive flag.

```sh
for ka in "-keepalive-time 30" ""; do
  tag=$([ -n "$ka" ] && echo with || echo without)
  ate_run probe.demo "rm -f /tmp/idle-$tag.log; setsid sh -c '
    { printf \"{\\\"f_string\\\":\\\"before-idle\\\"}\n\"; sleep 400;
      printf \"{\\\"f_string\\\":\\\"after-idle\\\"}\n\"; sleep 5; } \
    | grpcurl $ka -max-time 500 -d @ grpcb.in:9001 grpcbin.GRPCBin/DummyBidirectionalStreamStream \
        > /tmp/idle-$tag.log 2>&1
    echo EXIT=\$? >> /tmp/idle-$tag.log' </dev/null >/dev/null 2>&1 & echo started-$tag"
done

# ...wait > 7 minutes, then collect
ate_run probe.demo 'cat /tmp/idle-with.log /tmp/idle-without.log'
```

`-d @` reads request messages from stdin, so the stream stays open exactly as
long as the producing subshell does — that 400s `sleep` is the idle window, and
it has to exceed 300s to mean anything. With no producer on stdin, `grpcurl`
sees EOF immediately and the stream closes before it can be idle at all.

The expected result is asymmetric; see [§4](#4-results-2026-08-20).

### 2.7 `grpc_status` on a failed RPC

```sh
ate_run probe.demo 'grpcurl -max-time 30 -d "{\"code\":5,\"reason\":\"deliberate\"}" \
  grpcb.in:9001 grpcbin.GRPCBin/SpecificError'
```

Expect `Code: NotFound`, `Message: deliberate`.

It has to be a *server-side* error. Calling a nonexistent method looks like the
obvious test and is not one: `grpcurl` resolves the method against reflection
first and refuses locally, so nothing is sent and the gateway logs nothing.

## 3. How to verify the results

A client exit code is not evidence — it cannot distinguish traffic that went
through the gateway from traffic that bypassed it. Every test above is confirmed
against the gateway's own records.

**The MITM legs, as JSON.** The gateway writes two formats to one stream, the
CONNECT leg as text and the three MITM legs as JSON, so filter before parsing:

```sh
kubectl logs -n ate-system deploy/atenet-egress -c envoy --since=15m \
  | grep '^{' \
  | jq -r '[.leg, .protocol, .authority, (.path|.[0:46]), .status, (.grpc_status//"-"), .flags, .duration_ms] | @tsv'
```

| Test | `leg` | `protocol` | other |
| --- | --- | --- | --- |
| 2.1 gRPC/TLS | `mitm` | `HTTP/2` | `status 200`, `grpc_status OK` |
| 2.2 gRPC/h2c | `cleartext` | `HTTP/2` | `status 200`, `grpc_status OK` |
| 2.3 streaming | `mitm` | `HTTP/2` | `duration_ms` ≈ 10000 |
| 2.4 WebSocket | `mitm` | `HTTP/1.1` | `status 101` — HTTP/1.1, **not** HTTP/2 |
| 2.5 SSE | `mitm` | `HTTP/2` | `duration_ms` ≈ probe duration, `flags DR` |
| 2.6 idle | `mitm` | `HTTP/2` | `duration_ms` > 400000 with keepalive, exactly ≈300100 without |
| 2.7 failed RPC | `mitm` | `HTTP/2` | `status 200` *and* `grpc_status NotFound` |

`status 200` with a non-OK `grpc_status` on the same line is the whole reason
that field exists: without it every failed RPC reads as a success.

**The CONNECT leg** authorizes all of them. Its absence means the traffic never
went through the gateway and the test measured nothing. It is also where an
idle-timeout kill shows up, as `flags=SI`:

```sh
kubectl logs -n ate-system deploy/atenet-egress -c envoy --since=15m | grep '\[egress\]'
```

**Upstream protocol, per cluster.** This is what distinguishes "the request
worked" from "the request took the intended path", and it is the assertion for
the gRPC/WebSocket split specifically:

```sh
kubectl port-forward -n ate-system deploy/atenet-egress 15000:15000 &
curl -s localhost:15000/stats \
  | grep -E 'cluster\.egress_forward_proxy(_grpc|_cleartext)?\.upstream_(cx_http[12]_total|rq_(5xx|completed))' \
  | grep -v ': 0$'
```

`egress_forward_proxy` must show **only** `http1`, `egress_forward_proxy_grpc`
**only** `http2`, and `upstream_rq_5xx` must be absent. An `http2` count on the
default cluster means gRPC and WebSocket traffic are sharing an upstream
protocol, which is the failure described in §4.

## 4. Results (2026-08-20)

Run by hand against a GKE cluster (`substrate-poc`) with the sdsmint gateway and
the sandbox demo installed, from actor `probe` in atespace `demo`. Every row was
confirmed against a gateway access-log line, not just a client exit code.

| Test | Result | Gateway saw |
| --- | --- | --- |
| 2.0 ALPN pre-flight | pass | `http_version=2 code=200` |
| 2.1 gRPC over TLS | pass | `HTTP/2`, `grpc_status: OK`, reflection + unary |
| 2.2 gRPC over h2c | pass | `leg: cleartext`, `HTTP/2`, `grpc_status: OK` |
| 2.3 gRPC server-streaming | pass | 10 messages, `EXIT=0`, `duration_ms: 10108` |
| 2.4 WebSocket over TLS | pass *after a fix* | `HTTP/1.1`, `status: 101`, bidirectional echo |
| 2.5 SSE over TLS | pass | `HTTP/2`, 306 KB in 6 s, first byte at 0.32 s |
| 2.6 Idle stream survival | pass, **conditional** | see below |
| 2.7 `grpc_status` on failure | pass | `status: 200`, `grpc_status: NotFound` |
| WebSocket over cleartext | not covered | every public `ws://` echo service now 301s to HTTPS |
