# egressauthz e2e: runtime cost

Measured cost of `internal/e2e/suites/egressauthz` on a GKE cluster, and where
that time actually goes. The short version: the two assertions cost about half
a second, and the other ~12 seconds are fixture bring-up.

## What was measured

- **Cluster**: `gke_haiyanmeng-gke-dev_us-central1-c_substrate-poc`, Substrate
  installed and healthy (`ate-system` up, `actor-id-ca-pool` present).
- **Command**: `./hack/run-e2e.sh ./internal/e2e/suites/egressauthz -json -timeout 20m`
- **Date**: 2026-08-23. Three consecutive runs, all passing, no leftover
  namespaces afterwards.
- **Method**: wall clock from `/usr/bin/time`; phase boundaries recovered from
  the `Time` field on each `go test -json` event.

## Totals

| | Wall clock | `go test` package time |
| --- | --- | --- |
| Run 1 | 24.5s | 18.8s |
| Run 2 | 18.2s | 13.1s |
| Run 3 | 17.8s | 13.2s |

Steady state is **~18s wall, ~13s package**. Run 1 was about 5s slower purely
from image-layer pushes that were already present for the runs after it.

The ~5s gap between wall clock and package time is spent outside the suite:
`hack/run-e2e.sh` sources `.ate-dev-env.sh`, which shells out to
`gcloud projects describe`, and Go then compiles the test binary.

## Phase breakdown

Runs 2 and 3 agreed to within 0.1s at every boundary; the figures below are
run 2.

| Phase | Δ | Cumulative |
| --- | --- | --- |
| Framework init → `Creating namespace` | 1.45s | 1.45s |
| Namespace created, unknown-actor credential minted | 0.31s | 1.76s |
| `ko` build | 3.05s | 4.81s |
| `ko` publish to GCR | 1.92s | 6.73s |
| `kubectl apply` → pod created | 3.42s | 10.15s |
| Pod scheduled, image pulled, ready | 2.36s | 12.51s |
| `TestGatewayRefusesANonActorWorkload` | **0.49s** | 13.00s |
| `TestGatewayRefusesAnUnknownActor` | **0.05s** | 13.05s |

## Reading the numbers

**The assertions cost 0.54s; the fixture costs ~11.5s.** Roughly 96% of the
suite is building, pushing, and scheduling the probe pod. The gateway
interactions themselves are two sub-second round trips.

**Go's per-test attribution is misleading here.** Run 1 reports
`--- PASS: TestGatewayRefusesANonActorWorkload (17.10s)`, but that is
`sharedProbe` performing the entire bring-up inside the first test that needs
it. The second test's 0.05s is the honest marginal cost of a test once the
probe exists.

**Both tests fail at the hop they are supposed to**, which is the whole point
of asserting on `Stage` rather than on the error text:

```
TestGatewayRefusesANonActorWorkload: atunnel: egress gateway TLS handshake ...
TestGatewayRefusesAnUnknownActor:    atunnel: egress gateway rejected CONNECT with 403 ...
```

## Marginal cost, and what scales it

Adding this suite to a lane costs about **18s** of wall clock.

- **Extra tests in this package are nearly free** — ~0.05s each, because they
  reuse the one probe pod. The ~11.5s fixture cost is paid once per package,
  not once per test.
- **The variable cost is `ko`.** All three runs had a warm Go build cache and
  base layers already in the registry (run 1 logged fourteen `existing blob`
  lines). A genuinely cold run — fresh clone, empty registry — will be
  meaningfully slower, and that is the figure CI pays on a cache miss.

## Timeout headroom

The suite's timeouts are far above measured reality:

| Budget | Configured | Measured |
| --- | --- | --- |
| Probe pod readiness (`waitForProbeReady`) | 3m | 2.4s |
| Probe HTTP client (`probeClient.http`) | 90s | 0.5s |
| One handshake inside the probe (`--handshake-timeout`) | 20s | < 0.5s |

That is headroom rather than waste, but it does mean a genuine hang fails
slowly: a wedged probe burns three minutes before the suite gives up.

## Reproducing

```bash
/usr/bin/time -f 'WALL=%e' ./hack/run-e2e.sh ./internal/e2e/suites/egressauthz \
  -json -timeout 20m > /tmp/egressauthz.json 2>/tmp/egressauthz.err
```

Then recover the phase boundaries from the JSON stream, keying on the `Output`
of each event and diffing its `Time` against the first event: `Creating
namespace`, `Building github.com`, `Publishing gcr.io`, `pod/egressprobe
created`, `is ready`, and the two per-test `--- PASS` lines.
