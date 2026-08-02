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

## Where the code went

The PoC answered its questions, so the Go code graduated out of `poc/` and now
ships in an image. Everything below still holds — the measurements were taken
against this code — but the files have moved:

- `poc/sdsmint/*.go` → **`internal/sdsmint/`** (package name unchanged)
- `poc/sdsmint/cmd/sdsmintd/` → **`cmd/sdsmintd/`**, built by `make build-images`
- the deployed gateway → **`manifests/ate-install/atenet-egress.yaml`**

What stayed here is the part that is still a PoC: this file and
[EXPLAINER.md](EXPLAINER.md), the harnesses in `hack/`, the Envoy fixtures in
`testdata/`, the generic reference config in `deploy/`, and `cmd/sdsload/`
(measurement code, deliberately not in `ko build`).

One caveat carried over from the move: `--null-minter` is now
`--unsafe-null-minter`. It serves a pre-signed *shared* leaf so a load test
measures Envoy rather than the signer, which was a harness affordance and is a
production footgun now that the binary ships. It logs at Error level when set.

## Running it

```sh
go test ./internal/sdsmint/...     # unit tests, no Envoy needed
./poc/sdsmint/hack/run-poc.sh      # hermetic end-to-end: 14 assertions
./poc/sdsmint/hack/run-poc.sh --forward-proxy   # + real MITM of example.com: 16
./poc/sdsmint/hack/run-scale.sh    # scalability phases 0-7 (see below)
./poc/sdsmint/hack/run-stress.sh   # one rate, held long enough to mean something
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

`go test ./internal/sdsmint/ -run XXX -bench . -benchmem` — numbers below are the
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
`sdsmintd --unsafe-null-minter`, which serves pre-signed wildcard leaves: a mint costs
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

### Sustained load at 500/s

Phase 3 holds each rate for five seconds. That is enough to find where things
break and not enough to characterise a rate, so `hack/run-stress.sh` holds one
rate long enough for the tail percentiles to mean something and repeats it,
which is the only way to know whether a tail number is a measurement or a mood.

```sh
./poc/sdsmint/hack/run-stress.sh                      # 500/s for 30 s, 3 trials
RATE=1000 DURATION=60 TRIALS=1 ./poc/sdsmint/hack/run-stress.sh
```

500/s of never-seen SNIs for 30 s — 15,000 connections per trial, eight trials,
every connection a cold fetch forcing a real mint, the real signer throughout.
Envoy and sdsmintd restart between trials so each starts from an empty live set
instead of inheriting the last one's.

| trial | p50 | p90 | p95 | p99 | max |
|---|---:|---:|---:|---:|---:|
| 1 | 7,020 µs | 29,372 | 41,120 | 64,981 | 101,392 |
| 2 | 7,141 µs | 35,282 | 52,106 | 145,569 | 338,464 |
| 3 | 8,461 µs | 75,088 | 138,107 | 205,401 | 246,379 |
| 4 | 9,196 µs | 58,612 | 90,691 | 155,252 | 200,424 |
| 5 | 8,041 µs | 40,763 | 66,494 | 122,665 | 184,903 |
| 6 | 8,980 µs | 41,926 | 61,560 | 108,949 | 143,346 |
| 7 | 7,738 µs | 38,535 | 58,207 | 121,544 | 173,194 |
| 8 | 7,876 µs | 32,770 | 47,734 | 81,031 | 138,843 |

Trials 1–3 and 4–8 are two separate invocations twenty minutes apart; every
trial restarts both processes regardless.

All eight served 15,000/15,000 with **zero failures and zero drops** at
499.88–499.94/s achieved, client scheduling lag p99 4.0–4.8 ms — the offered
rate really was delivered. Each reported `mints=+15000`: the harness diffs
`mints_issued` against the connection count, so a run that quietly went warm
would be caught rather than reported as a fast one.

**The p50 holds. The p95 does not.**

| | median | range |
|---|---:|---:|
| p50 | 7,959 µs | 7,020 – 9,196 (1.3×) |
| p95 | 59,884 µs | 41,120 – 138,107 (3.4×) |
| p99 | 122,105 µs | 64,981 – 205,401 (3.2×) |

So: **p50 ≈ 8 ms, p95 ≈ 60 ms** with the tail figure carrying a factor of three
either way. Quote the p50 as a number and the p95 as an order of magnitude.

The eight trials also kill the obvious explanation for that spread. In the first
invocation the tail degraded monotonically while the machine's load average rose
2.4 → 7.8, which looked like a clean story about contention. The second
invocation started quieter (1.36), ended at 6.45, and its tail improved
monotonically — 90.7, 66.5, 61.6, 58.2, 47.7 ms — with the *worst* trial of the
two runs occurring on the *least* loaded box. Neither trend is real. Both are
what a three-times-noisy statistic looks like when you read five points of it in
a row, and the honest summary is a range with no trend in it.

Three things the runs say about where the latency lives, all stable across all
eight:

- **Signing is not the bottleneck.** `sign_avg` held at 601–635 µs, so 500/s is
  0.30 cores of P-256 work against 14. The cost is Envoy pausing and resuming
  handshakes and opening 500 new gRPC streams a second, not the CA.
- **The client is expensive and shares the box** — 33.5–35.9 s of CPU over a
  30 s run, ~1.1 cores. Against phase 0's static-certificate p50 of 2,208 µs at
  a comparable rate, roughly 2 ms of the 8 ms p50 is client floor and ~6 ms is
  the on-demand path.
- **Memory tracked the live set exactly as the ramp predicted**: 944–947 MB
  Envoy RSS for 15,200 live secrets, 62 KB apiece, reproducible to within 0.3%.
  At 500 cold SNIs/s that is ~1.9 GB/minute of Envoy heap with `--idle` unset.

One framing caveat: 100% cold SNIs is the pathological case, not a workload.
Real egress traffic re-hits hosts, and a warm hit is 196 ns of minter time. This
measures the worst thing the design can be asked to do, sustained.

### The warm path at 500/s, and what the pause actually costs

The section above is the pathological case: every connection cold. Real egress
traffic re-hits hosts, so the other end of the range matters just as much, and
it is the same run with one flag changed.

```sh
DISTINCT=50 TRIALS=8 ./poc/sdsmint/hack/run-stress.sh
```

`DISTINCT=N` fetches N names *before* the timer starts and then cycles the trial
over them. Same rate, same duration, same processes, same client — the only
difference is that no connection pauses for SDS. 50 names against 15,000
connections is ~300 hits apiece, and every trial reported `mints=+0`, which is
asserted rather than assumed: a warm trial that quietly minted would otherwise
look like a fast one.

| trial | p50 | p90 | p95 | p99 | max |
|---|---:|---:|---:|---:|---:|
| 1 | 2,128 µs | 2,747 | 3,051 | 3,976 | 25,177 |
| 2 | 2,125 µs | 2,757 | 3,102 | 4,266 | 11,554 |
| 3 | 2,208 µs | 2,861 | 3,186 | 4,161 | 19,016 |
| 4 | 2,230 µs | 2,846 | 3,161 | 3,882 | 9,788 |
| 5 | 2,233 µs | 2,816 | 3,126 | 3,756 | 8,367 |
| 6 | 2,211 µs | 2,815 | 3,140 | 3,872 | 15,850 |
| 7 | 2,244 µs | 2,877 | 3,188 | 3,982 | 13,049 |
| 8 | 2,213 µs | 2,834 | 3,156 | 3,976 | 26,483 |

All eight served 15,000/15,000, zero failures, zero drops, 499.96–499.98/s
achieved, `cert_active=50` throughout.

**The warm path is steady in a way the cold path is not.** p50 median 2,212 µs
over a 2,125–2,244 range (1.06×); p95 median 3,148 µs over 3,051–3,188 (1.04×);
p99 median 3,976 µs. Set that beside the cold run's 3.4× p95 spread and the
conclusion is not "the tail is noisy" but something sharper:

| | cold (8 trials) | warm (8 trials) | difference |
|---|---:|---:|---:|
| p50 | 7,959 µs | 2,212 µs | **~5.7 ms** |
| p95 | 59,884 µs | 3,148 µs | **~57 ms** |
| p99 | 122,105 µs | 3,976 µs | ~118 ms |

That difference is the pause-fetch-resume cycle: Envoy stopping the handshake,
opening a DELTA_GRPC stream for the name, the round trip to sdsmintd, the mint,
and the resume. **Around 6 ms at p50 — and effectively all of the tail
variance.** A warm handshake at 500/s has a p95 only 42% above its own median.
Whatever makes the cold p95 swing between 41 ms and 138 ms lives entirely inside
the fetch, not in TLS, not in Envoy's per-connection work, and not in the client.

Signing is the smaller part of that 6 ms. `sign_avg` was 662–826 µs, so ~0.7 ms
is the CA and ~5 ms is stream setup, the xDS round trip and the resume — at
500 cold SNIs/s, 500 new gRPC streams a second. That is where to aim if this
number ever needs to come down, and it agrees with phase 2, which measured a
~4.3 ms cold penalty at 10/s with the signer removed from the path entirely.

Two smaller things fall out of the same run. Envoy held **65 MB of RSS for 50
live secrets against 946 MB for 15,200** — the working set, not the history, is
what a bounded live set buys. And the client got cheaper too (30.6 s of CPU
against 34–36 s, scheduling lag p99 1.9 ms against 4.0–4.8 ms), which is worth
knowing before reading the cold tails as pure server behaviour.

A separate 3-trial run at `DISTINCT=200` landed at p50 2,101 µs and p95 3,067 µs
— within noise of the 50-name numbers, so between 50 and 200 hot names the size
of the working set does not register.

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

### Measuring the deployed gateway: `sdsload --connect-via`

Every number above was taken against a listener `sdsload` dialled directly.
The deployed gateway is not reachable that way — its front door is mTLS
CONNECT, and the MITM listener behind it has no socket at all. `--connect-via`
drives it through that front door, using `internal/atunnel` rather than a
hand-rolled CONNECT, because a load test that speaks a slightly different
protocol measures something the actors never do.

```sh
sdsload --connect-via atenet-egress.ate-system.svc:443 \
        --target 192.0.2.1:443 --sni example.com --count 1000
```

Run it from a pod that has a podidentity credential bundle; the defaults for
`--connect-credential-bundle` and `--connect-trust-bundle` match the paths
`cmd/ateom-gvisor` uses. `--connect-destination` defaults to `192.0.2.1:443`
and has to be an IP — `atunnel` rejects hostnames, because in the real path the
authority comes from `SO_ORIGINAL_DST`. It is not load-bearing: the gateway
re-resolves from the inner SNI, so `--sni` is what decides where the traffic
goes.

**The dial number is not comparable to anything above.** In tunnel mode that
bucket also contains the gateway's own TLS handshake and a CONNECT round trip,
which is most of what is in it: a 50-connection run at 25/s against the shipped
config measured **dial p50 4,668 µs** against a direct-dial baseline in the tens
of microseconds. Handshake still means what it meant (p50 7,621 µs, in line with
the cold-path numbers above, on a box also running Envoy, sdsmintd and the
client). `--json-out` records `connect_via` for exactly this reason, so a saved
tunnelled run cannot later be read as a direct-dial baseline.

That run also confirms the gateway does through the tunnel what the harness
proved directly: 50 cold SNIs produced `cert_requested +50`, so every connection
minted its own leaf rather than sharing one; a name outside `--allow` failed all
three attempts with `certificate request denied` server-side; and a client
credential signed by a CA outside the podidentity trust bundle was refused at
the front door before reaching the MITM leg at all.

## Deploying this for real

**substrate deploys
[`manifests/ate-install/atenet-egress.yaml`](../../manifests/ate-install/atenet-egress.yaml)**,
not the file in `deploy/`. It differs in exactly one place — how traffic
arrives. substrate's front door is mTLS HTTP/1.1 CONNECT, because that is what
`internal/atunnel` speaks and it is the only way the actor's identity reaches
the gateway; the MITM listener then hangs off it as an Envoy *internal*
listener. Everything from `on_demand_secret` inward is the same config, and the
combination was validated before the manifest was written (an internal listener
was the one thing the PoC had never run `on_demand_secret` on).

Two limitations of the deployed gateway, stated in its header and worth
repeating: **it ships dormant** — nothing sets `EgressGatewayAddress`, so no
actor routes to it, and `sdsload --connect-via` is how it gets exercised — and
**there is no per-actor authorization**. `sdsmintd --allow` is a single
cluster-wide allowlist. The mTLS check proves a caller is a substrate workload,
not which actor it is; the `X-Ate-*` headers are metadata, not authentication.

**[`deploy/envoy-egress.yaml`](deploy/envoy-egress.yaml)** stays as the generic
reference for the transparent-redirect topology, and carries the longer
commentary. Verified against Envoy 1.37.5: `--mode validate` clean with no
deprecation warnings, and smoke-tested end to end — prefetch mints before first
request, `example.com` is fetched through the MITM at HTTP 200 with the origin's
real body, and the access log emits `sni=example.com` matching `authority`.

What both do about each gap in the PoC bootstraps:

- **Connect timeout.** `transport_socket_connect_timeout: 5s` on the MITM
  listener. The one non-negotiable — without it an SDS outage does not fail a
  first-contact handshake, it pauses it forever.
- **Upstream.** `dynamic_forward_proxy`, re-resolving from the request's own
  authority so the name that was policed is the name that gets dialled. Both
  configs spell the upstream protocol options under
  `typed_extension_protocol_options`, because the cluster-level
  `upstream_http_protocol_options` field is deprecated and slated for removal.
- **SDS socket.** Absolute, `/var/run/sdsmint/sdsmint.sock`, on a shared
  `emptyDir`. Nothing listens on TCP.
- **Listen address.** `0.0.0.0:8443`. Admin stays on loopback `9901`, and
  `sdsmintd --metrics-addr` on loopback `9091`.
- **Access logging.** JSON to stdout. `atenet-egress.yaml` logs both legs,
  because neither can see the whole picture: the CONNECT leg has the client
  certificate and the `X-Ate-*` headers but only an IP for a destination, and
  the MITM leg has `REQUESTED_SERVER_NAME` — the SNI the leaf was minted for —
  but by then the peer certificate is Envoy's own.
- **Memory.** `overload_manager` with a fixed-heap ceiling, plus a global
  downstream connection limit. An unbounded destination set means unbounded DNS
  entries *and* unbounded live secret subscriptions; `--idle` is what actually
  bounds the second one, and this is the backstop.
- **Resilience.** Circuit breakers on all three clusters, raised well above
  Envoy's 1024 defaults on the two internal legs and kept tighter on the one
  that faces the internet. On `sds_mint` the limit that matters is
  `max_requests`, which caps concurrent DELTA_GRPC streams: hitting it refuses a
  mint, which surfaces as a handshake failure indistinguishable from the
  allowlist denying the name.

Two rows where the two configs genuinely differ:

- **Node identity.** `deploy/envoy-egress.yaml` omits the `node:` block and
  expects `--service-node=$(POD_NAME).$(POD_NAMESPACE)`, because it assumes an
  SDS server shared across pods. `atenet-egress.yaml` hardcodes it: sdsmintd is
  a sidecar on a pod-local socket and keys subscription state per gRPC stream,
  so two replicas sending the same id are talking to two different processes.
- **Policy enforcement.** `deploy/envoy-egress.yaml` shows an `ext_proc` filter
  ahead of DNS resolution with `failure_mode_allow: false`. `atenet-egress.yaml`
  has none — the deployed gateway's only control is `sdsmintd --allow`, one
  cluster-wide list. Per-actor policy is the follow-up, and it is the reason the
  gateway ships dormant.

**Four decisions the config cannot make for you**, all called out in its header:
how traffic reaches the listener (that file is a transparent MITM, not an HTTP
CONNECT proxy — substrate answers this differently, see
`manifests/ate-install/atenet-egress.yaml`); where the CA key lives (a `localca` pool Secret is the default; add
`--ca-intermediate-ttl`, and a KMS signer for the root if you have one); who
trusts the MITM CA, and how that bundle is protected; and whether
`default_value` is the right
answer for non-SNI clients versus rejecting them at the listener.

## Layout

```
internal/sdsmint/
  ca.go            CA, FromPool, LoadCA, GenerateCA, Sign -> MintedCert. Key is
                   a crypto.Signer so a KMS/HSM signer drops in unchanged;
                   refuses a CA with no name constraint; optionally delegates
                   leaf signing to a short-lived in-memory intermediate.
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
  rotation_test.go the rotation-vs-cache clock invariant
  idle_test.go     --idle: what withdraws a name, what keeps one alive, and that
                   a withdrawn host is still reachable afterwards
  cache_test.go    structural invariants of the three structures agreeing
  bench_test.go    cost decomposition and cache-cap scaling

cmd/sdsmintd/      the daemon; UDS by default, chmod 0600

manifests/ate-install/
  atenet-egress.yaml  the deployed gateway: mTLS CONNECT front door, internal
                   listener for the MITM leg, sdsmintd as a native sidecar

poc/sdsmint/
  cmd/sdsload/     open-loop TLS load generator: one SNI per connection,
                   handshake timed apart from request, arrivals on a fixed
                   timeline so saturation cannot hide as coordinated omission.
                   --connect-via drives the deployed gateway through its real
                   CONNECT front door instead of dialling a listener directly
  deploy/          envoy-egress.yaml         generic reference, transparent MITM
  testdata/        envoy-bootstrap.yaml      PoC; NO connect timeout, by design
                   -fwdproxy.yaml            PoC; real egress re-origination
                   -good.yaml                PoC; hermetic, + the connect timeout
                   -scale.yaml               measurement; raised circuit breakers
                   -static.yaml              the phase 0 control
  hack/lib.sh      shared process lifecycle and admin-stat plumbing
  hack/run-poc.sh  the end-to-end harness
  hack/run-scale.sh  the nine scale phases
  hack/run-stress.sh sustained single-rate load against the real signer,
                     repeated so a tail number can be checked for spread
```

Zero new Go dependencies — everything needed is already vendored, so `vendor/`
is untouched. The two on-demand protos are only referenced from YAML.

## Security controls implemented

A CA trusted by every actor is the most dangerous key in the system, so the
controls against abusing it are part of the PoC rather than deferred: hostname
allowlist (`--allow`, and `sdsmintd` refuses to start without one), issuance
audit log (one structured `slog` line per mint with host, serial, notAfter),
short leaf TTLs (`--ttl`, default 5m), and a local-only channel (unix socket,
mode 0600 — leaf private keys transit it).

The controls on the CA key itself are worth stating separately, because they are
what bounds the damage when the allowlist and the audit log have both already
failed:

- **Name constraints are mandatory, not advisory.** `sdsmintd` refuses to start
  on a CA carrying no critical `dNSName` constraint. Starting anyway takes an
  explicit `--ca-allow-unconstrained`, whose help text says what it means: the
  key can forge a certificate for any name on the internet. A constrained CA
  that leaks is the difference between forging one vendor's API and forging
  anyone's bank — worth more than any amount of care about where the key file
  sits. `--ca-name-constraint` applies when generating; `kubectl-ate admin
  make-ca-pool --permitted-dns-domain` is the equivalent for a real pool.
- **The key is held the way substrate holds its other three CAs.** `--ca-pool`
  takes an `internal/localca` pool JSON, the format `podcertcontroller` and
  `ate-api-server` already mount from a projected Secret. No new custody
  mechanism, no new thing to get wrong.
- **A short-lived intermediate, held only in memory.** `--ca-intermediate-ttl`
  has the root sign a delegated signer at ~2/3 of its lifetime and leaves signed
  by that. It bounds a filesystem or heap compromise to the intermediate's
  lifetime rather than the root's, and costs nothing measurable: the
  intermediate is a local key, so a mint stays at the 374.6 µs measured above.
  It needs a root with `pathLenConstraint >= 1`; `sdsmintd` says so by name
  rather than letting the chain fail later inside a TLS handshake.
- **`crypto.Signer` throughout**, now including `localca.CA.SigningKey`, so a
  KMS or HSM signer substitutes for the root without touching any of this code.
  Substrate ships no such signer — picking one would mean picking a cloud — so
  this is the seam, not the implementation. Pair it with the intermediate: a KMS
  signature is 10–50 ms against 374.6 µs local, a 30–100× regression on every
  cache miss if the root signs leaves directly, but a non-event if it signs an
  intermediate twice a day.

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
