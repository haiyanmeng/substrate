# sdsmint: a PoC for on-demand per-SNI certificate minting

This is a working proof of concept for the design in [`mint.md`](../../mint.md):
let a substrate actor dial an arbitrary HTTPS destination while the egress proxy
still sees and polices the request, by having Envoy MITM the connection with a
leaf certificate **minted per SNI, on demand**.

It implements the doc's recommended Option C — Envoy's shipped
`on_demand_secret` certificate selector plus the `sni` certificate mapper,
backed by a custom Go SDS server that does the signing. The MITM CA private key
never enters the data plane.

The PoC's job was to de-risk the design: prove the config works on a real Envoy,
and turn the doc's three open questions into measured answers. All three are
answered below, along with three corrections to the doc.

**[EXPLAINER.md](EXPLAINER.md) explains how it works** — the mechanism, the
connection lifecycle, and a walkthrough of the code. This file covers what it
proves.

## Running it

```sh
go test ./poc/sdsmint/...          # unit tests, no Envoy needed
./poc/sdsmint/hack/run-poc.sh      # hermetic end-to-end: 14 assertions
./poc/sdsmint/hack/run-poc.sh --forward-proxy   # + real MITM of example.com: 16
```

The harness downloads `envoy-1.37.5-linux-x86_64` into `poc/sdsmint/__run/`
(gitignored), generates a throwaway CA, starts `sdsmintd`, and runs the checks
and experiments below. `--forward-proxy` adds one leg that needs outbound
internet. `--keep` leaves the processes up for poking at.

Last full run: **16 passed, 0 failed** (Envoy 1.37.5, Linux x86-64).

## What it proves

| | Result |
|---|---|
| A leaf is minted per SNI, on demand | `a.mitm.example` and `b.mitm.example` each get their own leaf — correct CN, SAN matching the SNI, **distinct serials**. `cert_requested=3`, one subscription per name. |
| `prefetch_secret_names` works | `default.mitm.example` is minted at config load, before any request arrives. |
| The hostname allowlist really blocks | A host outside `--allow` fails the handshake with `error:0A000410:SSL routines::ssl/tls alert handshake failure`, is audited server-side, and leaves **no live subscription** (`cert_active` unchanged). |
| Full MITM re-origination works | Through the `dynamic_forward_proxy` bootstrap: real `example.com` content fetched, HTTP 200, while the client is served our leaf (`issuer=CN=sdsmint PoC MITM CA`). The upstream leg is still verified against the system trust store via `auto_sni` + `auto_san_validation`, so the MITM does not weaken origin authentication. |

## Answers to mint.md's open questions

### 1. How does an on-demand secret expire or rotate?

**Rotation is push-driven. Envoy has no TTL of its own for an on-demand
secret.** Measured: with `--rotate` and a 6s TTL, `cert_updated` went `2 -> 8`
and the served serial changed under a held subscription. The leaf's own
`notAfter` does not cause Envoy to re-fetch — nothing expires client-side.

Consequences for the design:

- The minting server owns the rotation clock. `sdsmintd --rotate` re-mints and
  pushes at ~2/3 of TTL for every live subscription.
- Eviction is server-driven too: a name returned in `removed_resources` cancels
  the data plane's subscription for it.
- This is why the doc's `DELTA_GRPC` choice is load-bearing rather than a
  stylistic preference — SotW has no way to withdraw a single name.
- **Because Envoy never expires a secret itself, the rotation clock and the
  minter's cache clock have to be designed together.** They were not, and the
  result was a real bug — see below.

#### The bug this turned up: rotation through a cache that had not expired

Rotation refreshes by calling `Minter.GetCertificate`. The cache used to hand a
leaf back for its full TTL, so the tick at ⅔T was a **cache hit**: it re-pushed
the identical serial and changed nothing. The cache only let go at T, so the
real re-mint happened at the *next* tick, 4/3·T — by which point the leaf Envoy
was serving had been expired for T/3. At the 5m default that is **100 seconds
per cycle of serving an expired certificate**, on every live subscription.

Measured before the fix, at a 6s TTL: push at t+0 (valid to t+5.4s), push at
t+4s carrying *the same serial*, replacement not until t+8s — **2.62s of
expired leaf**. `TestDeltaSecretsRotationPushesNewVersion` did not catch it
because it only asserted the serial eventually changes, which the late re-mint
still satisfies.

The fix is an ordering invariant, `reuseFraction < rotateFraction < 1`: the
cache stops reusing a leaf at half its life, so the tick at ⅔ life always finds
a stale entry and really re-mints. `TestRotationNeverServesAnExpiredLeaf` walks
two full cycles and asserts no leaf is ever left in place past its `notAfter`;
`TestRotationOutlivesCacheReuse` guards the constants cheaply.

Worth knowing if you tune `--ttl` very low: **x509 encodes `notAfter` with
one-second granularity**, so a leaf's real expiry is up to a second earlier than
`--ttl` asked for. Irrelevant at 5m, but it eats a meaningful slice of the
rotation margin below ~10s.

### 2. Is the secret cache per-worker or shared?

**Shared.** Measured: 12 concurrent connections to one SNI across
`--concurrency 4` produced `cert_requested +1`, not `+4`.

So the memory footprint of a large live host set scales with the number of
*hosts*, not hosts × workers, and the signing load is one mint per host rather
than one per host per worker. This is the good case for the design; it was the
doc's main scaling worry.

### 3. What happens when the SDS server is down?

Two distinct behaviours, and the second one is the finding that matters:

- **Cached names keep working.** A host whose secret Envoy already holds still
  served HTTP 200 with `sdsmintd` killed. There is no per-connection dependency
  on SDS.
- **Cold names stall forever.** This is worse than "fails". The selector pauses
  the handshake waiting for SDS and **Envoy never gives up** — measured still
  waiting after the client's full 20s budget in the harness, and after 3 minutes
  by hand. The client hangs; it never gets an error.

  The fix is a filter-chain-level `transport_socket_connect_timeout`. With
  `5s` set, the same request fails in **5.03s** with
  `error:0A000126:SSL routines::unexpected eof while reading`. The harness proves
  both legs.

  **`transport_socket_connect_timeout` is therefore required, not optional, in
  any real deployment of this design.** Without it an SDS outage does not
  degrade egress — it wedges every actor that dials a host not already in cache.

  `envoy-bootstrap.yaml` and `-fwdproxy.yaml` deliberately omit it so the
  harness can demonstrate the default; both carry a comment saying so.
  **[`envoy-bootstrap-good.yaml`](testdata/envoy-bootstrap-good.yaml) is the
  same config with the knob set** — that is the one to copy, and the harness
  runs it as leg 2.

## Corrections to mint.md

1. **"NACK names that fail validation" is backwards.** NACK is a *client* action
   in xDS — a server cannot NACK. The correct way for the server to say "this
   name will not be issued" is to return it in `removed_resources`. The PoC does
   that, and Check 2 confirms it produces a clean handshake failure with no
   lingering subscription.
2. **The design needs Envoy 1.37+.** `on_demand_secret` and `cert_mappers.sni`
   first shipped in 1.37 — `extensions_build_config.bzl` has 0 matches in
   v1.35.13 and v1.36.9, 3 in v1.37.5. On anything older the config is rejected
   outright. `manifests/ate-install/atenet-router.yaml:177` currently pins
   `v1.30-latest`, so **the router's Envoy must be bumped before this design can
   ship**. The harness version-gates and fails with a clear message.
3. **Open question #1 was already answerable from the docs**, and the PoC
   confirms it: *"A resource removal sent via the xDS response will cancel the
   data plane subscription for the specific secret name."*

## Two config requirements that are easy to miss

- **`tls_inspector` is mandatory.** Without that listener filter Envoy never
  reads the ClientHello, so the `sni` mapper has nothing to map and every
  connection silently collapses to `default_value`. It fails as a subtle
  wrong-certificate bug, not as an error.
- **Session resumption must be disabled.** A resumed session skips the handshake
  and therefore skips certificate selection entirely. `disable_stateless_session_resumption`
  and `disable_stateful_session_resumption` sit on `DownstreamTlsContext`, *not*
  inside `common_tls_context`.
- **A node id and cluster are required**, or the SDS subscription is rejected with
  `TlsCertificateSdsApi: node 'id' and 'cluster' are required`. They can come
  from a `node:` block *or* from `--service-node`/`--service-cluster`, and the
  file wins if both are present — so per-pod identity means **omitting the block
  entirely**. Note `/config_dump` renders the bootstrap as parsed from disk, not
  the effective node, so it cannot be used to check which one took effect.

## What it costs

`go test ./poc/sdsmint/ -run XXX -bench . -benchmem` — numbers below are the
mean of three rounds on an Intel Core Ultra 7 165U (14 threads), Linux x86-64.
They are here to size a deployment, not to be a leaderboard; re-measure on the
target hardware before believing any absolute figure.

### Where a mint goes

| | ns/op | allocs |
|---|---:|---:|
| `CA.Sign` (the whole thing) | 374,608 | 403 |
| ├ fresh P-256 keypair | 32,263 | 16 |
| ├ `x509.CreateCertificate` | 286,907 | 276 |
| └ PKCS#8 + PEM encoding | 21,300 | 77 |
| cache hit | 196 | 0 |
| allowlist check, 32 patterns | ~940 | 0 |
| pack a Secret into a delta `Resource` | 3,003 | 10 (**1.5 KB on the wire**) |

**~2,700 mints/second/core, and the fresh keypair is not why.** Keygen is 9% of
a mint. `x509.CreateCertificate` is 77%, and a CPU profile says **more than half
of that is Go verifying the signature it just produced** — `signTBS` calls
`checkSignature` unconditionally, so every mint pays one ECDSA sign *plus* one
ECDSA verify, and the verify is the more expensive of the two. That is inside
`crypto/x509` and not something this code can opt out of; the lever, if one is
ever needed, is to not call `CreateCertificate` per leaf.

So a key pool would buy at most 9%. Anyone optimising this should start by
noting the cache hit is **1,900× cheaper than a miss** and go looking for misses
instead.

The 1.5 KB wire size is the number to multiply for rotation: a single rotation
response carrying every live secret crosses Envoy's 4 MB default gRPC receive
limit somewhere around **2,700 live names**. `rotateAll` batches unconditionally,
so that is a real ceiling, not a soft one.

### The cache gets slower as it gets bigger

| `--cache-cap` | miss, serial | miss, 14 goroutines | eviction alone |
|---|---:|---:|---:|
| 256 | 441 µs | 129 µs | 12 µs |
| 1,000 | 448 µs | — | 47 µs |
| 10,000 | 960 µs | 808 µs | 533 µs |
| 100,000 | 13.3 ms | — | 12.1 ms |

**Raising `--cache-cap` makes the server slower.** `evictLocked` runs two full
map scans per miss — one for expired entries, one to find the LRU victim — and
both hold the exclusive lock. At cap 100k a single miss costs 30× what it costs
at cap 256, and 91% of that is the scan rather than the signing.

The parallel column is the sharper version of the same point. Signing happens
*outside* the lock, so concurrent misses should scale; at cap 256 they do
(441 → 129 µs, ~3.4×). At cap 10,000 they do not (960 → 808 µs, 1.2× on 14
threads) because every miss now sits in the eviction scan holding `m.mu`. The
lock, not the CA, is the throughput ceiling once the cache is large.

Nothing here is fixed yet — it is a measurement, and the shape of the fix is
obvious enough (an intrusive LRU list plus an expiry heap turns both scans into
O(1)) that it is not worth guessing at before there is a target cache size.
**Until then, treat `--cache-cap` as a knob with a real cost and leave it low.**

## Deploying this for real

**[`deploy/envoy-egress.yaml`](deploy/envoy-egress.yaml)** is the deployable
config. Verified against Envoy 1.37.5: `--mode validate` clean with no
deprecation warnings, and smoke-tested end to end — prefetch mints before first
request, `example.com` is fetched through the MITM at HTTP 200 with the origin's
real body, and the access log emits `sni=example.com` matching `authority`.

What it does about each gap in the PoC bootstraps:

| Gap | Production config |
|---|---|
| Connect timeout | `transport_socket_connect_timeout: 5s`. The one non-negotiable. |
| Upstream | `dynamic_forward_proxy` with ALPN-negotiated `auto_config`. Avoids the cluster-level `upstream_http_protocol_options`, which is deprecated and slated for removal — it lives under `typed_extension_protocol_options` now. |
| SDS socket | Absolute, `/var/run/sdsmint/sdsmint.sock`, on a shared `emptyDir`. |
| Listen address | `0.0.0.0:8443`. Admin stays on loopback `9901`. |
| Node identity | No `node:` block; supplied per-pod via `--service-node=$(POD_NAME).$(POD_NAMESPACE)`. |
| Policy enforcement | `ext_proc` ahead of DNS resolution, `failure_mode_allow: false` so egress fails closed. |
| Access logging | JSON to stdout, including `REQUESTED_SERVER_NAME` — the SNI the cert was minted for. |
| Memory | `overload_manager` with a fixed-heap ceiling. An unbounded destination set means unbounded DNS entries *and* unbounded live secret subscriptions. |
| Resilience | Circuit breakers on all three clusters, TLS 1.2 floor on both legs, connect-failure retries. |

**It still would not ship as a bootstrap file for the router.** That Envoy is
ADS-driven: `cmd/atenet/internal/router/envoyrunner.go:77` writes a bootstrap
containing only `dynamic_resources` plus the `xds_cluster`, and listeners are
built in Go and pushed. The block to port is the `DownstreamTlsContext` at
`cmd/atenet/internal/router/xds.go:627`, so treat this file as the specification
for what LDS should emit. Bumping the image to 1.37+ is necessary but not
sufficient — `go-control-plane` also needs the `on_demand_secret` and
`cert_mappers.sni` message types (`envoy@v1.37.0` has them, already vendored).

**Four decisions the config cannot make for you**, all called out in its header:
how traffic reaches the listener (it is a transparent MITM, not an HTTP CONNECT
proxy); where the CA key lives (a KMS signer, not a file); who trusts the MITM
CA, and how that bundle is protected; and whether `default_value` is the right
answer for non-SNI clients versus rejecting them at the listener.

## Layout

```
poc/sdsmint/
  ca.go            CA, LoadCA, GenerateCA, Sign -> MintedCert. key is a
                   crypto.Signer so a KMS/HSM signer drops in unchanged.
  minter.go        allowlist + bounded TTL cache + issuance audit log
  server.go        delta SDS: per-stream subscriptions, initial_resource_versions,
                   removed_resources for refusals, optional rotation timer
  cmd/sdsmintd/    the daemon; UDS by default, chmod 0600
  rotation_test.go the rotation-vs-cache clock invariant
  bench_test.go    cost decomposition and cache-cap scaling
  deploy/          envoy-egress.yaml         PRODUCTION. the one to deploy.
  testdata/        envoy-bootstrap.yaml      PoC; NO connect timeout, by design
                   -fwdproxy.yaml            PoC; real egress re-origination
                   -good.yaml                PoC; hermetic, + the connect timeout
  hack/run-poc.sh  the end-to-end harness
```

Zero new Go dependencies — everything needed is already vendored, so `vendor/`
is untouched. The two on-demand protos are only referenced from YAML.

## Security controls implemented

From mint.md's list: hostname allowlist (`--allow`, and `sdsmintd` refuses to
start without one), issuance audit log (one structured `slog` line per mint with
host, serial, notAfter), short leaf TTLs (`--ttl`, default 5m), CA name
constraints (`--ca-name-constraint`), local-only channel (unix socket, mode
0600), and `crypto.Signer` indirection so the CA key can live in a KMS.

## Non-goals

This is not wired into `atenet-router` or any manifest, per the plan. The
version gate above is a finding to report, not a change made here.

## Known rough edges

- `sdsmintd`'s cache is per-process; a replicated deployment mints once per
  replica. Fine for the PoC, worth deciding on before it ships.
- The rotation timer pushes to every live subscription on one shared ticker
  rather than per-name deadlines, and `rotateAll` puts them all in one response.
  Measured at 1.5 KB per secret, that response passes Envoy's 4 MB default gRPC
  receive limit at roughly 2,700 live names.
- `evictLocked` is O(cache size) per miss, under the lock — see "What it costs".
  It is why `--cache-cap` should stay small for now.
- The stream's `versions` map never shrinks. Minter cache eviction does not
  propagate to it, so Envoy's live secret set only grows; names leave only when
  the allowlist refuses them. That interacts badly with the batching above.
- `GracefulStop` cannot be used unconditionally: an xDS stream never ends on its
  own, so the daemon falls back to a hard stop after a 2s grace period. Worth
  remembering for any other long-lived-stream service in the repo.
