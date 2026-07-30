# sdsmint: a PoC for on-demand per-SNI certificate minting

An egress proxy cannot police an HTTPS request it cannot read. The usual answer
is to MITM the connection — terminate the actor's TLS at the proxy, inspect and
police the plaintext, re-originate to the real origin — but that requires
presenting a certificate for whatever host the actor happens to dial, and for a
substrate actor the destination set is open-ended. This PoC mints that leaf
**per SNI, at handshake time**, from a CA key that lives outside the data plane.

The mechanism is two Envoy extensions that already ship in the box, composed:
`on_demand_secret`, a certificate selector that pauses the handshake and fetches
a secret by name over SDS, and `cert_mappers.sni`, a certificate mapper that
makes that name the connection's SNI. What is left to build is the SDS server —
and because it is a separate process, **the MITM CA private key never enters the
data plane**. Envoy only ever receives short-lived leaf keys, over a unix socket.

The PoC's job was to find out whether that works and what it costs: get the
config running on a real Envoy, then measure the things nobody could answer on
paper — how an on-demand secret expires, whether the secret cache is per-worker,
what happens when SDS is down, and how far the whole arrangement scales. Those
answers are below, along with the findings that changed the design.

**[EXPLAINER.md](EXPLAINER.md) explains how it works** — the mechanism, the
connection lifecycle, and a walkthrough of the code. This file covers what it
proves.

## Running it

```sh
go test ./poc/sdsmint/...          # unit tests, no Envoy needed
./poc/sdsmint/hack/run-poc.sh      # hermetic end-to-end: 14 assertions
./poc/sdsmint/hack/run-poc.sh --forward-proxy   # + real MITM of example.com: 16
./poc/sdsmint/hack/run-scale.sh    # scalability phases 0-7 (see below)
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

## The questions that had to be measured

### 1. How does an on-demand secret expire or rotate?

**Rotation is push-driven. Envoy has no TTL of its own for an on-demand
secret.** Measured: with `--rotate` and a 6s TTL, `cert_updated` went `2 -> 8`
and the served serial changed under a held subscription. The leaf's own
`notAfter` does not cause Envoy to re-fetch — nothing expires client-side.

Consequences for the design:

- The minting server owns the rotation clock. `sdsmintd --rotate` re-mints and
  pushes at ~2/3 of TTL for every live subscription.
- Eviction is server-driven too: a name returned in `removed_resources` cancels
  the data plane's subscription for it. `sdsmintd --idle <duration>` is what
  drives that — see "How far it scales", where it takes `cert_active` from 3,000
  to 0 and Envoy's heap from 139 MB to 9.5 MB.
- This is why `DELTA_GRPC` is load-bearing rather than a stylistic preference —
  state-of-the-world SDS has no way to withdraw a single name.
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
than one per host per worker. This is the good case, and it was the main scaling
worry going in.

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

## Three things to know before building this

1. **A server cannot refuse a name by NACKing it.** The intuitive design is for
   the minting server to NACK a name that fails the allowlist — but NACK is a
   *client* action in xDS, and a server has no NACK to send. The correct way to
   say "this name will not be issued" is to return it in `removed_resources`,
   which per the Envoy docs also cancels the data plane's subscription for it.
   The PoC does that, and Check 2 confirms it produces a clean handshake failure
   with no lingering subscription.
2. **This needs Envoy 1.37+.** `on_demand_secret` and `cert_mappers.sni` first
   shipped in 1.37 — `extensions_build_config.bzl` has 0 matches in v1.35.13 and
   v1.36.9, 3 in v1.37.5. On anything older the config is rejected outright.
   `manifests/ate-install/atenet-router.yaml:177` currently pins
   `v1.30-latest`, so **the router's Envoy must be bumped before this design can
   ship**. The harness version-gates and fails with a clear message.
3. **Envoy's own documentation answers the expiry question**, and the PoC
   confirms it: *"A resource removal sent via the xDS response will cancel the
   data plane subscription for the specific secret name."* There is no
   client-side TTL to lean on; see question 1 above.

## Three config details that fail silently

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
| pack a Secret into a delta `Resource` † | 3,003 | 10 |

† 1,529 B on the wire, reported by the benchmark as a custom `wire_bytes`
metric. It is a serialised size, not an allocation figure.

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

#### Reading the table row by row

`ns/op` and `allocs` are Go's `-benchmem` output — nanoseconds and *allocation
count* per operation. `B/op` is omitted deliberately; this table is about time,
and the allocation count is here only as a proxy for GC pressure.

- **`CA.Sign`** is `BenchmarkSign`: one complete mint, end to end. 374.6 µs
  inverts to ~2,670/sec on one core, which is where "~2,700 mints/second/core"
  comes from. The three indented rows decompose it.
- **Fresh P-256 keypair** is bare `ecdsa.GenerateKey`, nothing else. It is
  isolated to settle one design question — every mint generates a fresh leaf key
  so that no two hosts share one, and if that dominated, a pre-generated key pool
  would be the obvious lever. At 8.6% it is not.
- **`x509.CreateCertificate`** is timed with the leaf key and the template
  already built outside the loop, so it is purely marshal-the-TBS plus sign.
  76.6% of a mint, and see above for what is hiding inside it.
- **PKCS#8 + PEM encoding** is everything after the signature: PEM the leaf DER,
  append the PEM'd CA chain, marshal the leaf key to PKCS#8, PEM that. 5.7% —
  cheap, but paid on every mint and it allocates, which is why it is listed
  rather than folded away.
- **Cache hit** is a repeat `GetCertificate` for a host already held. Zero
  allocations because it returns the cached pointer rather than copying.
  374,608/196 = **1,911×** cheaper than a miss.
- **Allowlist check** is approximate because it is two sub-benchmarks: matching
  the last of 32 patterns (890 ns, 0 allocs) and rejecting after scanning all 32
  (996 ns, 1 alloc — the error). Zero allocations on the match path because
  `matchLabels` walks labels in place instead of splitting into slices.
- **Pack a Secret** is not part of minting at all. It is the per-push cost —
  build the `Secret` proto, wrap it in an `Any`, marshal — paid once per live
  subscription per rotation tick.

**The box-drawing glyphs oversell the decomposition.** The three children sum to
340,470 ns against a 374,608 ns parent, so **about 9% is unattributed**: that gap
is the work `Sign` does that no sub-benchmark covers — `randomSerial()` (a
`crypto/rand` read plus a `big.Int`), `time.Now()`, building the
`x509.Certificate` template and its `pkix.Name`, the `parseIP` branch, and
allocating the `MintedCert`. Read `├`/`└` as "the three parts worth isolating",
not as a partition.

**On a warm proxy, policy costs more than caching.** `GetCertificate` validates
before it looks in the cache, so the allowlist is on every call, hit or miss. The
196 ns cache-hit figure already contains a validate — against a *one-pattern*
allowlist, which is what the benchmark minter is built with. Swap in 32 patterns
and validation alone is ~890 ns, against under 200 ns for everything else on the
warm path put together. The lookup is not what to tune; the pattern list is.

~~The 1.5 KB wire size is the number to multiply for rotation: a single rotation
response carrying every live secret crosses Envoy's 4 MB default gRPC receive
limit somewhere around 2,700 live names.~~ **Wrong — measured and withdrawn.**
That assumed one stream carrying many names. Envoy opens one stream *per
secret*, so a response never carries more than one resource (largest observed:
1,705 B) and the 4 MB ceiling is unreachable this way. The real ceiling is the
stream count; see [How far it scales](#how-far-it-scales).

### The cache used to get slower as it got bigger

The first implementation was a plain map, and `--cache-cap` was a trap: raising
it made the server *slower*. Eviction ran two full map scans per miss — one for
entries past their deadline, one to find the LRU victim — both with the
exclusive lock held.

`cache.go` replaces the scans with a recency list (the victim is the tail) and
a min-heap on the reuse deadline (dead entries are at the root). Both are O(1)
plus O(log n) of heap work, and the numbers go flat:

| `--cache-cap` | miss, serial | miss, 14 goroutines | insert alone |
|---|---:|---:|---:|
| 256 | 441 µs → **400 µs** | 129 µs → **61 µs** | 12.4 µs → **368 ns** |
| 1,000 | 448 µs → **389 µs** | — | 47 µs → **371 ns** |
| 10,000 | 960 µs → **376 µs** | 808 µs → **48 µs** | 533 µs → **403 ns** |
| 100,000 | 13.3 ms → **331 µs** | — | 12.1 ms → **598 ns** |

A miss at cap 100k is **40× faster**, and the insert inside it 20,000×. What is
left is essentially just the signing, which is why the serial column is now the
same number at every cap.

The parallel column is the part that matters for a busy proxy. Signing happens
*outside* the lock, so concurrent misses ought to scale — before, at cap 10,000,
they did not at all (960 → 808 µs, 1.2× on 14 threads) because every miss sat
in the eviction scan holding `m.mu`. Now the lock is held for a few hundred
nanoseconds and misses scale ~7× on 14 threads regardless of cap.

One deliberate limit: a put reclaims at most 64 dead entries. Draining a
100k-entry cache that expired all at once costs 86 ms, which is not something
to do with the lock held; the remainder comes back on subsequent puts, and
making room never depends on it because eviction by recency is always O(1).

## How far it scales

The benchmarks above measure the signer. This section measures **Envoy**, which
turns out to be where the limits are.

```sh
./poc/sdsmint/hack/run-scale.sh              # quick sweep, phases 0-7, ~17 min
./poc/sdsmint/hack/run-scale.sh --full       # 50k names, 2 x 30 min idle watch
./poc/sdsmint/hack/run-scale.sh --phases 1,6
```

Nine phases, each isolating one question. Phases 0–2 and 4–7 run
`sdsmintd --null-minter`, which serves pre-signed wildcard leaves: a mint costs
~375 µs and would swamp everything Envoy does, so the signer is removed from the
measurement rather than measured through. Phase 3 is the exception — the signer
is its subject. Phase 0 is a static-certificate control on an otherwise
identical listener, so every number below is a *difference*, not an absolute.

Numbers are the quick sweep on an Intel Core Ultra 7 165U, Envoy 1.37.5,
`--concurrency 2`, 3,000 live names.

Latency figures are from an otherwise-idle machine. A sweep run with other work
on the box reproduced every *shape* below but inflated the tails badly —
phase 3's p99 at 500/s went to 285 ms against the 28.9 ms quoted here. Treat the
absolute microseconds as one machine's numbers and the ratios as the finding.

### The two findings that change the design

**1. Envoy opens one DELTA_GRPC stream per secret name, and holds it open.**
3,120 live secrets produced 3,120 concurrent streams on the single SDS
connection. This was not the assumed model — the server was written expecting
one stream carrying many subscriptions — and most of the predictions in this
repo followed from the wrong picture.

The consequence is a hard, silent host limit. Streams are concurrent requests,
so **the live host count is a concurrent-request count against the SDS
cluster**, and Envoy's default circuit breaker is `max_requests: 1024`. Past it
the subscription overflows, no secret is ever delivered, and the handshake fails
~15 s later with `initial fetch timed out for ...v3.Secret`. The harness walked
straight into this at exactly 1024 hosts, and — worse — the failure made the
memory curve look *flatter*, i.e. it read as good news. `deploy/envoy-egress.yaml`
pinned that same 1024 and has been corrected; `expect_all_served` now aborts any
phase whose setup did not fully succeed.

**2. ~60 KB of Envoy RSS per live secret, and nothing takes it back on its own.**
Linear and stable across the ramp: 68.9 KB/secret at 200 live, 62.7 at 1,000,
60.5 at 3,000. Left alone for 120 s, RSS went 234 MB → 238 MB — *up* — with
`cert_active` pinned at 3,000 and `server.memory_allocated` climbing 139 MB →
172 MB. Nothing is reclaimed because nothing evicts. Envoy has no expiry of its
own for an on-demand secret, and it never says it is finished with a name.

Untreated, memory for an egress proxy fronting arbitrary destinations is a
function of **every host ever contacted since the process started**, not of the
working set: 10k hosts ≈ 600 MB, 50k ≈ 3 GB.

**This is now fixed server-side.** `sdsmintd --idle <duration>` withdraws names
the proxy has stopped asking for, via `removed_resources`. Phase 6 runs both
arms over the same 3,000-name fill and the difference is unambiguous:

| | control (`--idle` unset) | `--idle 30s` |
|---|---|---|
| `cert_active` | 3,000 → **3,000** | 3,000 → **0** (inside 60 s) |
| `server.memory_allocated` | 139 MB → **172 MB** | 140 MB → **9.5 MB** |
| Envoy RSS | 234 MB → 239 MB | 234 MB → 235 MB |
| withdrawn | 0 | 3,000, over 3,000 sweeps |

Three things in that table are worth reading carefully:

- **Envoy really does act on a removal.** `cert_active` going to zero says the
  data-plane subscription was cancelled, and 43.4 KB per withdrawn secret came
  off `server.memory_allocated` — the same order as the ~60 KB/secret measured
  going up.
- **RSS did not follow.** tcmalloc kept the pages. So withdrawal bounds *growth*
  — the freed heap is reused by the next N hosts instead of being added to — but
  it does not shrink the process. **Size the pod's memory limit for the peak
  live set either way.** This is exactly the disagreement the harness reports
  both numbers for; had it only watched RSS it would have concluded the fix did
  nothing.
- **3,000 withdrawals took 3,000 sweeps**, one apiece. That is finding #1 seen
  from the other side: the sweep is per-stream, and each stream holds exactly
  one name.

The three runs of this phase agreed to within 1% on every column.

And the property that makes it safe rather than merely cheap: **all 40 re-fetches
of withdrawn hosts succeeded**, at p50 6.2–6.4 ms across runs — the same range as
phase 2's cold first contact, which is what it is. Withdrawing a host that turns out to still be wanted
costs exactly one cold handshake, not an error. That matters because the server
cannot actually observe idleness: once Envoy holds a secret it never mentions the
name again however much traffic flows for it, so `--idle` is a bet that a name
nobody has *asked* for recently is a name nobody needs. The bet is cheap to lose,
which is what makes it takeable — and it is asserted in the harness rather than
assumed, because a broken re-fetch would be invisible in the memory curve.

### The rest

| Phase | Question | Answer |
|---|---|---|
| 2 | Does first contact slow down as the live set grows? | **No.** Cold-fetch p50 was 6,535 µs at 200 live and 6,629 µs at 3,000 — flat. Lookup is not the problem. |
| 3 | Where does minting saturate? | Every offered rate to **500/s was fully served, zero failures**, at ~650 µs/sign. The tail is what moves: handshake p99 goes 8,967 µs → 28,860 µs (3.2×) from 50/s to 500/s while p50 barely changes. Capacity-plan against the p99 knee, not the throughput one. |
| 4 | What does the selector cost on a cache hit? | **Nothing measurable.** p50 deltas vs the control were −2 µs at `--concurrency 1` and −130 µs at 4, i.e. below this harness's resolution. No sign of contention on the shared secret cache. |
| 5 | What does a rotation tick cost? | At 500 live names, 999 pushes reached the data plane, per-stream cost ≤ 1 ms, largest response 1,705 B, no NACKs, and concurrent handshakes were undisturbed (p99 3,588 µs). But one stream per secret means a tick is **N independent sign-and-push operations** — at 50k live names, 50k signatures per tick. |
| 7 | What does an SDS restart cost? | Warm names kept serving with SDS fully down (200/200, p99 3,569 µs) — no per-connection dependency. On restart Envoy replayed **2,997 names across 2,997 separate requests**, first at +306 ms, settled by +20 s. With a real signer that burst is N mints, not N cache hits. |

### What this harness cannot tell you

The load generator is the bottleneck, and it says so: **2,300 µs of client CPU
per connection against a 2,266 µs handshake**. Every connection is a full
handshake with resumption disabled on both sides, so the client pays a P-256
verify each time. Two consequences, both of which the harness prints rather than
hides:

- Differences under ~1 ms between phases are client scheduling, not Envoy. This
  is why phase 4 reports "below resolution" instead of quoting a negative
  overhead as though it were a measurement.
- Phase 3's "no knee up to 500/s" is a floor on Envoy's capacity, not a ceiling.
  Finding the real one needs the client on a separate machine.

Phase 8 (`dynamic_forward_proxy` with a real origin) is opt-in and excluded from
the default set: real DNS and a real upstream add variance that would pollute
every comparison above.

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
  minter.go        allowlist + issuance audit log, over the cache below
  cache.go         bounded LRU with per-entry reuse deadlines; recency list
                   plus expiry heap, so nothing is O(cache size) under the lock
  server.go        delta SDS: per-stream subscriptions, initial_resource_versions,
                   removed_resources for refusals and for idle withdrawal,
                   optional rotation timer
  nullminter.go    MEASUREMENT ONLY: pre-signed shared leaves, so the scale
                   phases measure Envoy instead of a P-256 signing loop
  metrics.go       nil-safe atomic counters behind --metrics-addr, alongside
                   net/http/pprof
  cmd/sdsmintd/    the daemon; UDS by default, chmod 0600
  cmd/sdsload/     open-loop TLS load generator: one SNI per connection,
                   handshake timed apart from request, arrivals on a fixed
                   timeline so saturation cannot hide as coordinated omission
  rotation_test.go the rotation-vs-cache clock invariant
  idle_test.go     --idle: what withdraws a name, what keeps one alive, and that
                   a withdrawn host is still reachable afterwards
  cache_test.go    structural invariants of the three structures agreeing
  bench_test.go    cost decomposition and cache-cap scaling
  deploy/          envoy-egress.yaml         PRODUCTION. the one to deploy.
  testdata/        envoy-bootstrap.yaml      PoC; NO connect timeout, by design
                   -fwdproxy.yaml            PoC; real egress re-origination
                   -good.yaml                PoC; hermetic, + the connect timeout
                   -scale.yaml               measurement; raised circuit breakers
                   -static.yaml              the phase 0 control
  hack/lib.sh      shared process lifecycle and admin-stat plumbing
  hack/run-poc.sh  the end-to-end harness
  hack/run-scale.sh  the nine scale phases
```

Zero new Go dependencies — everything needed is already vendored, so `vendor/`
is untouched. The two on-demand protos are only referenced from YAML.

## Security controls implemented

A CA trusted by every actor is the most dangerous key in the system, so the
controls against abusing it are part of the PoC rather than deferred: hostname
allowlist (`--allow`, and `sdsmintd` refuses to start without one), issuance
audit log (one structured `slog` line per mint with host, serial, notAfter),
short leaf TTLs (`--ttl`, default 5m), CA name constraints
(`--ca-name-constraint`, marked critical, so even a total compromise of this
service cannot impersonate hosts outside the constrained domains), a local-only
channel (unix socket, mode 0600 — leaf private keys transit it), and
`crypto.Signer` indirection so the CA key can live in a KMS or HSM instead of a
file.

## Non-goals

This is deliberately not wired into `atenet-router` or any manifest. The version
gate above is a finding to report, not a change to make here.

## Known rough edges

- `sdsmintd`'s cache is per-process; a replicated deployment mints once per
  replica. Fine for the PoC, worth deciding on before it ships.
- The rotation timer pushes to every live subscription on one shared ticker
  rather than per-name deadlines. Response size turned out not to be the
  problem — Envoy's one-stream-per-secret model means one resource per message —
  but the tick is N independent signatures, so at 50k live names it is 50k mints
  in a burst on a shared ticker. Per-name deadlines would spread it.
- `--idle` defaults to **off**, which is the unbounded-growth behaviour. That is
  deliberate for a PoC — it keeps the control arm of phase 6 available and does
  not change what `run-poc.sh` measures — but any real deployment should set it,
  and a shipping version should probably invert the default.
- Withdrawal frees Envoy's heap but not its RSS (tcmalloc keeps the pages), so
  it bounds growth without lowering the peak. A pod still has to be sized for
  the largest live host set it will see at once.
- The idle sweep is a wall-clock timeout, not a use signal, because Envoy never
  reports use of a secret it already holds. A host under continuous heavy
  traffic is withdrawn on exactly the same schedule as one that has gone quiet;
  it just pays a cold handshake and comes straight back.
- An SDS restart reconnects every stream at once — 2,997 replays in one burst at
  3,000 live names. With the real minter that is a mint storm at exactly the
  moment the server is coldest. The cache survives a restart only if it is
  warmed or persisted; today it is neither.
- `GracefulStop` cannot be used unconditionally: an xDS stream never ends on its
  own, so the daemon falls back to a hard stop after a 2s grace period. Worth
  remembering for any other long-lived-stream service in the repo.
