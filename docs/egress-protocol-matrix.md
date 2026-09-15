# Egress protocol matrix

What an actor's outbound traffic is allowed to be, protocol by protocol, and
what the actor is told when the answer is no.

An `EgressPolicy` is written in terms of destinations, but the egress path does
not treat every protocol alike: some traffic is read as requests and decided one
at a time, some is an opaque stream decided once on its address, and some never
reaches the gateway at all. This document records an experiment that measures
each shape from inside a real actor, on a real cluster, and reports the exact
status code, body or socket error the workload sees.

The suite behind it is `internal/e2e/suites/egressmatrix`. Every number below is
copied from one run of it; [Reproducing](#reproducing) says how to produce
another.

## The path under test

```
actor process
  │  connect()
  ▼
nftables in the actor's netns ── UDP, except dport 53 ─▶ dropped
  │ TCP redirected                └─ DNS/UDP masqueraded ─▶ resolver (no gateway)
  ▼
atunnel (worker pod)
  │  mTLS + HTTP CONNECT <actor's original destination>
  ▼
atenet-egress ── ext_proc ──▶ decision 1: may this tunnel open?
  │                            (address only: `cidrs` and `all` rules)
  ▼
inner listener
  ├─ cleartext chain (HTTP/1.x, h2c) ── ext_proc ──▶ decision 2: per request
  │                                     (all three rule types, on Host)
  └─ passthrough chain (TLS, anything opaque) ──▶ no second decision
  ▼
origin
```

Two decisions, and which one applies is decided by the protocol:

- **Tunnel open.** atunnel's CONNECT carries the IP:port the actor's kernel
  dialed, never a name, so only `cidrs` and `all` rules can match here. If one
  does, the gateway remembers the address and the tunnel is opaque from then on.
  If none does but the policy has `hostnames` rules, the tunnel still opens, in
  a mode where nothing may be relayed until a request inside it is allowed. If
  the policy has no rule that could ever match, the CONNECT is refused.
- **Per request.** Only on the cleartext chain, and only for traffic Envoy's
  `http_inspector` recognized as HTTP/1.0, HTTP/1.1 or h2c. Every request is
  matched on its `Host` and on the dialed address, by all three rule types.

The cluster used here runs the non-MITM gateway
(`manifests/ate-install/atenet-egress.yaml`), which does not terminate TLS.
Under the sdsmint gateway, TLS traffic is decrypted and lands on the cleartext
chain instead, so the TLS rows below would change; nothing else would.

## Design of the experiment

One actor, one origin, five policies, sixteen protocol shapes.

**The origin** (`internal/e2e/fixtures/testserver`, `matrixtarget` subcommand)
serves every shape on one sniffed port: it peeks at the first byte, sends a TLS
`ClientHello` to a TLS server with ALPN `h2,http/1.1`, the HTTP/2 client preface
to an h2c server and anything else to HTTP/1.1. It answers `/echo` with how the
request actually arrived, serves `/ws` both as an RFC 6455 upgrade and as an RFC
8441 extended `CONNECT`, and acts as a forward proxy for a tunneling `CONNECT`.
It runs with `GODEBUG=http2xconnect=1`, without which Go's HTTP/2 server never
advertises `SETTINGS_ENABLE_CONNECT_PROTOCOL`. Two Services publish it, one on
port 80 and one on port 443, so a case can ask whether the port or the content
decides how the gateway treats a connection.

**The probe** (`matrixprobe` subcommand) runs as the actor's only container and
takes one case per HTTP request, so the same actor can be re-measured under a
changed policy without being redeployed. It reports a status code and body when
something answered and a Go error plus its type when nothing did, which is how a
refusal is told from a drop without parsing prose.

Three parts of the probe are hand-written rather than taken from a library,
because a library would have hidden the thing being measured:

- The tunneling `CONNECT` over HTTP/1.1 is written as bytes, because the status
  line and body of a refusal are the result; `http.Transport` turns both into
  one opaque error.
- Everything over HTTP/2 goes through a small framer-level client
  (`matrixh2.go`). `net/http` rejects the `:protocol` pseudo-header before it
  reaches the wire, and since Go 1.27 `x/net/http2` is a wrapper over `net/http`
  and rejects it too, so an extended `CONNECT` cannot be sent any other way. It
  also gets the pseudo-header sets exactly right — a tunneling `CONNECT` sends
  neither `:scheme` nor `:path`, an extended one sends both — and it records
  whether the peer advertised `SETTINGS_ENABLE_CONNECT_PROTOCOL`, which for a
  WebSocket over HTTP/2 is half the answer on its own.
- The QUIC case sends a hand-built QUIC v1 Initial packet: long header, version
  1, a random 8-byte destination connection ID, padded to the 1200 bytes the
  spec requires. It carries no CRYPTO frame, so no handshake can complete; what
  it asks is whether a datagram of that shape and size leaves the actor at all,
  which is settled before a handshake would begin.

**The policies**, each applied to the same actor in turn, with 13 s between
phases to outwait the gateway's 10 s policy cache:

| Phase | Policy | What it isolates |
|---|---|---|
| `allow-all` | one `all` rule | the protocol's own limits, with policy out of the way |
| `hostnames-only` | `hostnames: [both origins]`, dialed by DNS name | what a hostname rule can and cannot reach |
| `hostname-not-allowed` | `hostnames: [the TLS origin]`, cleartext origin dialed anyway | the per-request denial |
| `cidrs-only` | `cidrs: [/32 of each origin]` | what an address rule reaches |
| `no-policy` | the `EgressPolicy` deleted | the closed default |

The hostname phases dial the origin by its cluster DNS name on purpose: a
hostname rule has nothing to match when the actor dials an address, because the
authority the gateway then sees is that address.

## Results

One run, 2026-09-15, GKE cluster `substrate-poc`. `RST` is
`read: connection reset by peer`, `EOF` is an unexpected clean close, and the
time after either is how long the actor waited for it. Every timeout is the
probe's own 4 s bound, not the network's.

| Case | `allow-all` | `hostnames-only` | `hostname-not-allowed` | `cidrs-only` | `no-policy` |
|---|---|---|---|---|---|
| HTTP/1.1 cleartext | 200 | 200 | **403 `egress denied`** | 200 | RST, 9 ms |
| HTTP/2 cleartext (h2c) | 200 | 200 | **403 `egress denied`** | 200 | RST, 5 ms |
| HTTP/1.1 over TLS | 200 | EOF, 1.03 s | EOF, 1.03 s | 200 | RST, 6 ms |
| HTTP/2 over TLS (ALPN h2) | 200 | EOF, 1.02 s | EOF, 1.01 s | 200 | RST, 5 ms |
| CONNECT over HTTP/1.1, cleartext | **404**, empty body | 404 | **403 `egress denied`** | 404 | RST, 6 ms |
| CONNECT over HTTP/2, cleartext (h2c) | **404**, empty body | 404 | **403 `egress denied`** | 404 | RST, 5 ms |
| CONNECT over HTTP/1.1, inside TLS | 200, tunnel relays | EOF, 1.01 s | EOF, 1.01 s | 200, tunnel relays | RST, 5 ms |
| CONNECT over HTTP/2, inside TLS | 200, tunnel relays | EOF, 1.01 s | EOF, 1.01 s | 200, tunnel relays | RST, 6 ms |
| WebSocket over HTTP/1.1, `ws://` | 101, echo | 101, echo | **403 `egress denied`** | 101, echo | RST, 5 ms |
| WebSocket over HTTP/2, cleartext (extended CONNECT) | **EOF, 2.01 s** | EOF, 2.01 s | EOF, 2.01 s | EOF, 2.01 s | RST, 6 ms |
| WebSocket over HTTP/1.1, `wss://` | 101, echo | EOF, 1.02 s | EOF, 1.02 s | 101, echo | RST, 5 ms |
| WebSocket over HTTP/2, inside TLS (extended CONNECT) | 200, echo | EOF, 1.01 s | EOF, 1.01 s | 200, echo | RST, 7 ms |
| QUIC Initial datagram, UDP 443 | timeout | timeout | timeout | timeout | timeout |
| DNS over UDP, 1.1.1.1:53 | resolved | resolved | resolved | resolved | **resolved** |
| DNS over UDP, actor's own resolver | resolved | resolved | resolved | resolved | **resolved** |
| DNS over TCP, 1.1.1.1:53 | resolved | timeout | timeout | RST, 99 ms | RST, 101 ms |

### Denials, by where the decision was made

Four different things happen to a denied connection, and an actor can tell them
apart:

| Decision | Actor sees | Gateway logs |
|---|---|---|
| Per-request denial, cleartext chain | `403` with the body `egress denied` (13 bytes) | `"leg":"cleartext","status":403,"flags":"-","bytes_sent":13` and `egress denied: no rule allows the destination` |
| Tunnel-open refusal | TCP reset, within ~6 ms | `[egress] authority=… code=403 flags=- down_bytes=13` and `egress denied: no rule allows the destination` with `"leg":"egress"` |
| Tunnel opened, then nothing may be relayed | clean EOF after ~1 s, no status at all | `"leg":"passthrough","flags":"UH","upstream":null,"destination":null` |
| Protocol the gateway's codec rejects | clean EOF, no status | `"leg":"cleartext","flags":"DPE","status":0,"method":null` |

`egress denied` is the only message any denial carries; the reason goes to the
gateway's log, never to the actor (`cmd/atenet/internal/router/egress`,
`deniedBody`). Three of the four cases above cannot carry it at all, because
there is no response to put it in.

The tunnel-open refusal reaches atunnel, not the actor: atunnel has an open TCP
connection from the workload and no way to report a status on it, so it closes,
and because the workload has already written its request the close arrives as a
reset. This is why an actor with no policy sees `connection reset by peer` for
every TCP destination, and sees it in about 5 ms.

The third row is the one worth knowing about: under a `hostnames`-only policy, a
TLS connection to a destination no rule allows does not fail fast. The tunnel
opens, the actor completes its side of the handshake, and the gateway closes the
inner connection with no upstream (`flags: UH`) — so the actor waits about a
second and then reads EOF mid-handshake. TCP protocols that are not cleartext
HTTP inherit this: DNS over TCP under the same policy gets no error at all, it
hangs until the caller gives up — here, until the probe's own 4 s bound.

Where that second comes from is not settled by the evidence collected here: the
gateway's own access log records the passthrough connection as lasting 0 ms. It
matches the inner listener's `listener_filters_timeout: 1s`, which would fit a
`ClientHello` the listener never gets to act on, but that is a coincidence in
the numbers rather than a confirmed cause.

## What the matrix shows

**Cleartext HTTP is the only traffic the policy reads.** HTTP/1.1 and h2c are
decided per request, on the `Host` the request names, and both answer a denial
with `403 egress denied` — the one case where the workload gets a usable error.
Everything else is decided once, on the address, when the tunnel opens.

**HTTP/2 is not second-class, but extended CONNECT is.** h2c prior-knowledge
requests are inspected and policed exactly like HTTP/1.1, and ALPN `h2` inside
TLS passes through exactly like `http/1.1`. The exception is RFC 8441: the
cleartext chain's HTTP Connection Manager is configured with
`upgrade_configs: [websocket]`, which covers the HTTP/1.1 upgrade, but its
HTTP/2 codec has no `allow_connect`. The probe recorded
`SETTINGS_ENABLE_CONNECT_PROTOCOL=false` from the gateway, sent the extended
`CONNECT` anyway, and the codec rejected the stream as a protocol error
(`flags: DPE`); the actor read EOF after 2 s with no status. This fails
identically under every policy, `allow-all` included — it is a gateway
capability gap, not a decision. The same handshake succeeds inside TLS, where
the gateway relays bytes and the origin's own
`SETTINGS_ENABLE_CONNECT_PROTOCOL=true` is what the client sees.

**A workload's own CONNECT never leaves in the clear.** A tunneling `CONNECT`
on the cleartext chain has no route to match: both routes in the `cleartext`
route config match on a path prefix and on the `dev.ate.egress` `dial` metadata
the ext_proc sets, a `CONNECT` carries no `:path` at all, and there is no
default — so Envoy answers `404` with an empty body (`flags: NR`) over both
HTTP/1.1 and HTTP/2. Under a
policy that denies it, ext_proc gets there first and it is a `403 egress denied`
instead. Inside TLS the same `CONNECT` is just bytes, and it works: an actor
that needs an outbound forward proxy has to reach it over HTTPS.

**QUIC cannot leave the actor.** The netns drops UDP except destination port 53,
so a QUIC Initial is not refused, rejected or logged anywhere — it disappears,
and the actor learns nothing until its own timeout. This is independent of
policy: `allow-all` drops it too. An actor that speaks QUIC has to fall back to
TCP, and will only discover that by waiting.

**DNS over UDP is outside the policy.** Port 53/UDP is masqueraded straight out
of the netns and never reaches the gateway, so name resolution keeps working
with no `EgressPolicy` at all — including to an arbitrary public resolver such
as `1.1.1.1`. DNS over TCP is the opposite: it is ordinary TCP, so it goes
through the tunnel and is policed like everything else, which is why the
`cidrs`-only phase resets it (1.1.1.1 is not in the allowed prefixes) while UDP
to the same resolver succeeds. The gap is worth stating plainly: **an actor with
a restrictive egress policy can still exchange arbitrary bytes with an arbitrary
host, encoded as DNS queries and answers over UDP port 53.**

**The shape of a denial says where it happened.** A `403 egress denied` means a
request was refused inside a tunnel that opened. A reset a few milliseconds into
the connection means the tunnel itself was refused. A clean EOF about a second
in means the tunnel opened and the stream had nowhere to go — the only one of
the three that costs the actor real time, and the only one an actor cannot tell
apart from an origin that hung up on it.

## Reproducing

The suite needs a cluster with Agent Substrate installed and an egress gateway
deployed — the same prerequisites as the rest of `internal/e2e`:

```console
$ hack/run-e2e.sh ./internal/e2e/suites/egressmatrix -v
```

It deploys both origins, builds and deploys the probe actor, runs all sixteen
cases under all five policies, and then dumps the egress gateway's own logs for
the window it ran in. Results are in the output as one line per cell:

```
MATRIX|<phase>|<case>|ok=|status=|proto=|body=|detail=|error=|errorType=|elapsedMs=
GATEWAY|<container>|<the gateway's own log line>
```

Only one measurement is asserted — an `allow-all` policy must let a plain
HTTP/1.1 request through, or every denial above is just a broken fixture. Every
other cell reports rather than asserts, because the point is to find out what
the egress path does, not to freeze it.

## Limitations

- One cluster, one gateway build, one run. The non-MITM gateway only; the
  sdsmint MITM gateway would move every TLS row onto the cleartext chain.
- The WebSocket cases over HTTP/2 exchange raw stream bytes after the extended
  `CONNECT` rather than RFC 6455 frames. The handshake is genuine; the framing
  after it is the client's business and is not what the egress path decides on.
- The QUIC case proves a datagram does not leave. It does not prove a QUIC
  handshake would otherwise succeed.
- The origin's certificate is self-signed and the probe does not verify it. This
  gateway does not terminate TLS, so verifying it would only test the fixture.
