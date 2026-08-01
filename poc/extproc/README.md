# PoC: ext_proc egress authorization

An Envoy `ext_proc` gRPC server that authorizes actor egress, wired into a
hermetic two-checkpoint gateway and driven end to end against Envoy 1.37.5.

This implements the design in `egress-authn.md`. The policy table is hardcoded
in the server, so there is no control-plane dependency and nothing here needs
ate-api, Kubernetes, or a CA.

```
./poc/extproc/hack/run-poc.sh          # 26 assertions, ~15s
go test ./poc/extproc/...
```

## What it proves

The five policies from the design doc, enforced at the checkpoint that can
actually see what each one needs:

| Policy | Decided at | On what evidence |
|---|---|---|
| `DENY_ALL` | CONNECT | the actor alone |
| `ALLOW_ALL` | CONNECT | the actor alone |
| `ALLOW_BY_IP_BLOCK` | CONNECT | the CONNECT authority, as an `ip:port` literal |
| `ALLOW_BY_HOSTNAME` | MITM | the inner request's `Host` |
| `BASIC_CREDENTIAL_INJECT` | MITM | the inner `Host`, plus a header rewrite |

Measured, not assumed:

- A `DENY_ALL` actor gets 403 at the CONNECT and no tunnel is opened.
- An `ALLOW_BY_IP_BLOCK` actor reaches `127.0.0.0/8` and is refused `8.8.8.8`.
- `ALLOW_BY_HOSTNAME` admits `github.com` and `GitHub.COM`, and refuses
  `evil.example` and `sub.github.com` — over a CONNECT that is byte-identical to
  the allowed one. Only the inner `Host` differs, which is the whole reason the
  second checkpoint exists.
- `BASIC_CREDENTIAL_INJECT` removes the actor's own `Authorization` and the
  destination receives `Token: X` instead. The harness asserts on what the
  destination echoed back, not on the response code.
- No `X-Ate-*` header reaches the destination, including ones the actor
  re-sends inside the tunnel.
- A forged `x-ate-egress-mode: passthrough` or `x-ate-actor-key` on the CONNECT
  is overwritten, not honoured.
- With `extprocd` killed, the CONNECT fails. The 500 is the filter's own local
  reply, not the catch-all route happening to be safe — the harness checks the
  body to tell those apart.

## Topology

```
egressprobe --CONNECT--> :18500 egress listener
                           ext_proc #1  (:19600)  -> deny | passthrough | mitm
                           set_filter_state ate.actor
                           route on x-ate-egress-mode
                             passthrough -> ORIGINAL_DST on :authority
                             mitm        -> internal listener
                                              ext_proc #2 (:19601)
                                              -> echo (:19602)
```

`egressprobe` exists because curl cannot drive this: through a proxy it will not
let the CONNECT authority and the inner `Host` be set independently, and the two
checkpoints read exactly those two different values.

Deviation from the doc's topology diagram: denial is an ext_proc
`ImmediateResponse`, not a route to a deny cluster. It works on CONNECT, removes
a route, and carries a reason string the actor can act on.

Deliberately omitted: the inner listener has no `DownstreamTlsContext`. In the
real gateway it terminates TLS with a leaf minted per SNI by `sdsmintd`
(`poc/sdsmint`), which is what puts the inner request in the clear. Here the
tunnel carries plaintext, so the same checkpoint sees the same headers with none
of the certificate machinery.

## Findings

**1. Filter state does not cross an internal listener on `shared_with_upstream`
alone.** This is the doc's open runtime question #1, and the answer is yes-but.
`set_filter_state` with `shared_with_upstream: TRANSITIVE` puts the object on the
*upstream connection*. Getting it onto the internal listener's *downstream*
connection additionally requires the `internal_upstream` transport socket on the
cluster:

```yaml
transport_socket:
  name: envoy.transport_sockets.internal_upstream
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.transport_sockets.internal_upstream.v3.InternalUpstreamTransport
    transport_socket:
      name: envoy.transport_sockets.raw_buffer
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.transport_sockets.raw_buffer.v3.RawBuffer
```

Without it nothing errors. The hop succeeds, the request is proxied, and the
object is simply absent — measured as `fs=[acme-prod/repo-reader]` on the egress
chain and `fs=[-]` on the inner one. Since an absent actor falls through to
`DENY_ALL`, the symptom is "every MITM request is refused", which reads like a
policy bug rather than a config one.

**2. `request_attributes` must subscript the filter-state key.** Asking for bare
`filter_state` yields the whole CEL map, which ext_proc flattens into the
literal string `"CelMap value"` — an attribute that is present, non-empty, and
carries nothing. The working form is `filter_state['ate.actor']`.

**3. `clear_route_cache: true` is still mandatory and still silent.** Envoy
selects the route before the ext_proc mutation lands, so without it a request
carrying `x-ate-egress-mode` stays on whatever route matched the unmutated
headers. The catch-all here returns a loud 500 for exactly this reason; a
passthrough fallback would have returned 200 and skipped every check.

**4. `failure_mode_allow: false` makes extprocd a hard availability dependency**
of every actor's outbound traffic. That is the correct trade for an
authorization gate, but it is a real coupling: an extprocd outage is an egress
outage, and the design doc should say so where it discusses deployment.

**5. Envoy's default `--file-flush-interval-msec` is 10s**, longer than this
whole run. Anything asserting on the access log has to lower it or it concludes
that requests never happened.

## The identity gap

`resolveActor` prefers the filter-state object and falls back to the
`x-ate-actor-key` header and then to the `X-Ate-*` tunnel headers. Only the
filter-state source is sound. The fallbacks exist so the authorization logic can
be exercised, and `/stats` reports which one fired
(`inner.actor_source.{filter_state,actor_key,ate_headers,none}`) so the
distinction is measurable rather than assumed. A green run shows
`filter_state=7, none=0` and the others at zero.

The deeper gap is upstream of this PoC and unchanged by it: at the CONNECT
checkpoint the actor identity comes from `X-Ate-Atespace` / `X-Ate-Actor-Name`,
which the actor sends. `internal/atunnel/client.go` says as much — *"The egress
gateway must authenticate this metadata before using it for policy decisions."*
In production the gateway authenticates the worker's podidentity SVID at the TLS
layer and must resolve worker → actor itself. This PoC does not close that;
it assumes it closed and tests everything downstream.

## Layout

- `policy.go` — policy kinds, `Snapshot`, and a `Store` that hot-swaps via
  `atomic.Pointer`. Policy is data, not config: a change is a pointer swap, no
  restart, no torn read.
- `hardcoded.go` — the five demo actors in atespace `acme-prod`. The seam a real
  control-plane client would replace.
- `decision.go` — the pure decision functions. `DecideConnect` returns
  deny/passthrough/mitm; `DecideInner` returns allow plus injections. No Envoy
  types, so the policy semantics are testable on their own.
- `headers.go`, `server.go` — the ext_proc plumbing: reading headers from either
  `Value` or `RawValue`, building mutations, and the two checkpoint handlers.
- `stats.go`, `admin.go` — counters, `/stats`, `/healthz`, and the `/echo`
  endpoint that stands in for the destination.
- `cmd/extprocd` — both gRPC servers and the admin HTTP server.
- `cmd/egressprobe` — the CONNECT client.
- `testdata/envoy-extproc.yaml` — the gateway config.

## Not done

- No control-plane client. `HardcodedSnapshot()` is the whole policy source.
- No TLS anywhere: no client cert on the egress listener, no MITM leaf on the
  inner one, no re-origination to a real destination.
- Nothing is wired into `atenet-router`, whose Envoy is pinned at `v1.30-latest`
  and would need a bump to 1.37+ before any of this could ship.
