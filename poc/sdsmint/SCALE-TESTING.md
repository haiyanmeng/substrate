# Running the sdsmint scale tests

`poc/sdsmint/hack/run-scale.sh` answers one question: **how far does Envoy's
`on_demand_secret` certificate selector go before something bends?** It is not a
correctness harness — `run-poc.sh` is that — and it is not a pass/fail gate. It
produces a table of numbers whose *shape* is the finding.

The measured results and what they mean live in
[`README.md` § How far it scales](README.md#how-far-it-scales). This file is only
about operating the harness.

## Which harness you actually want

There are four layers of measurement, and picking the wrong one wastes a lot of
wall-clock time:

1. **Go microbenchmarks** — `go test ./internal/sdsmint/ -run XXX -bench . -benchmem`.
   Seconds. Costs a mint, a cache hit, a secret encode. No Envoy, no cluster.
   Start here when you changed `ca.go`, `cache.go`, or `minter.go`.
2. **`hack/run-scale.sh`** — this document. Minutes to hours. Measures **Envoy**,
   with the signer mostly removed from the picture on purpose.
3. **`hack/run-stress.sh`** — one rate, held long enough for the tail
   percentiles to mean something, repeated so a number can be checked against
   its own spread. Use it when phase 3 turned up something you want to
   characterise rather than locate.
4. **`__run/cluster-check.sh`** — the deployed gateway on a real cluster, driven
   through its mTLS CONNECT front door with `sdsload --connect-via`. Correctness
   and a handful of latencies, not a scale sweep.

## Prerequisites

- **Linux x86-64.** The harness downloads a pinned `envoy-1.37.5-linux-x86_64`
  release binary. 1.37 is a hard floor — `on_demand_secret` and
  `cert_mappers.sni` do not exist before it.
- **Go on `PATH`.** In this environment `go` is *not* in `/usr/local/go/bin`:
  ```sh
  export PATH="$PATH:$HOME/go/bin"
  ```
- **`curl`**, and outbound network for the first run only (~90 MB Envoy
  download into `poc/sdsmint/__run/`, cached afterwards). Phase 8 additionally
  needs working DNS and a reachable origin, which is why it is not in the
  default set.
- **Ports 18443, 19000 and 19100 free**, plus no stale `__run/sdsmint.sock`.
  The harness checks this up front and refuses to start otherwise — see
  [Troubleshooting](#troubleshooting).
- **File descriptors.** The script tries `ulimit -n 65536` and carries on if it
  cannot. Every connection is a full handshake with no resumption, so at 1000/s
  both the fd limit and the ephemeral port range come into play. The run prints
  what it actually got; if that line says 1024, the high-rate phases will report
  failures that are the box's, not Envoy's.
- **An otherwise idle machine**, if you intend to quote the numbers. A sweep run
  alongside other work reproduced every *shape* but inflated the tails badly:
  phase 3's p99 at 500/s went from 28.9 ms to 285 ms.

Nothing needs root, and nothing needs a Kubernetes cluster.

## Running it

```sh
export PATH="$PATH:$HOME/go/bin"

./poc/sdsmint/hack/run-scale.sh                  # quick sweep, phases 0-7, ~17 min
./poc/sdsmint/hack/run-scale.sh --full           # production-scale N, 30m idle watches
./poc/sdsmint/hack/run-scale.sh --phases 0,1,6   # a subset
./poc/sdsmint/hack/run-scale.sh --keep           # leave Envoy and sdsmintd up
./poc/sdsmint/hack/run-scale.sh --help
```

The script builds what it needs on every run — `./cmd/sdsmintd` and
`./poc/sdsmint/cmd/sdsload` into `__run/` — so there is no separate build step
and no stale-binary failure mode.

### `--full` versus the quick sweep

The quick sweep is sized to finish in a few minutes and still show the shape of
every curve. `--full` is the one whose numbers are worth quoting.

| | quick | `--full` |
|---|---|---|
| live-name steps | 200, 1 000, 3 000 | 1 000, 5 000, 20 000, 50 000 |
| arrival rates (phase 3) | 50, 200, 500 | 10, 50, 100, 250, 500, 1 000 |
| rotation storm sizes | 500 | 1 000, 5 000 |
| idle watch (phase 6) | 120 s, `--idle 30s` | 1 800 s, `--idle 300s` |
| steady-state count | 3 000 | 20 000 |

`--full` runs well over an hour, dominated by phase 6: it watches idle
reclamation twice — once with `--idle` and once without — for 30 minutes each.
Budget accordingly, or exclude it with `--phases 0,1,2,3,4,5,7`.

### `--keep`

Leaves `sdsmintd` and Envoy running after the last phase, and prints the admin
and metrics URLs:

- Envoy admin — `http://127.0.0.1:19000` (`/stats`, `/config_dump`, `/memory`)
- sdsmintd metrics — `http://127.0.0.1:19100/metrics`, plus `net/http/pprof`
- listener under test — `127.0.0.1:18443`

Useful for poking at a live process with the exact state a phase left it in.
Remember to kill them before the next run, or `require_clean_ports` will refuse
to start.

## The nine phases

Each isolates one question. `--phases` takes any comma-separated subset.

- **0 — control.** A static certificate on an otherwise identical listener.
  This is the floor: every other number is a *difference* against it, not an
  absolute.
- **1 — bytes per secret.** Envoy RSS against the number of live secrets.
- **2 — lookup.** Does first contact with a new name get slower as the live set
  grows?
- **3 — saturation.** Ramps the new-SNI arrival rate against the **real
  signer**. This is the only phase whose subject is the minter.
- **4 — warm path.** Per-connection cost of the selector on a cache hit.
- **5 — rotation storm.** Cost of one rotation tick at N live names.
- **6 — idle reclamation.** Does memory come back, with and without `--idle`?
  The long one.
- **7 — reconnect.** Cost of Envoy replaying its whole live set after sdsmintd
  restarts.
- **8 — realism.** One run through `dynamic_forward_proxy` against a real
  origin. **Opt-in, not in the default set**: real DNS and a real origin add
  variance that would pollute every other number. Run it with
  `--phases 8` to confirm nothing changes qualitatively, not to get a figure.

Every phase walks a disjoint slice of the synthetic host space
(`h%d.mitm.example`), so a name is never accidentally warm because an earlier
phase touched it. That is also why running a subset is safe.

### Why most phases do not use the real signer

Phases 0–2 and 4–7 run `sdsmintd --unsafe-null-minter`, which serves
**pre-signed, shared** wildcard leaves. A real mint costs ~375 µs and would
swamp everything Envoy does, so the signer is removed from the measurement
rather than measured through.

This means those phases prove nothing about per-SNI certificate binding — every
connection gets the same leaf. That is the point, and it is also why the flag is
named `--unsafe-`: it is a measurement affordance and must never appear in a
deployed manifest. Phase 3 is the phase that exercises real signing.

## Reading the output

Everything lands in `poc/sdsmint/__run/` (gitignored):

- **`scale-results.txt`** — one line per recorded measurement, the same lines
  the run prints as it goes. This is the artefact worth keeping.
- **`load-<label>.json`** — the full `sdsload` result object per load segment,
  including every latency percentile the summary line omits.
- **`sds.log`** — sdsmintd, appended across phases. `--log-level warn`, because
  at a thousand requests a second the audit log is itself a bottleneck.
- **`envoy.log`** — Envoy, replaced each time a phase restarts it.
- **`ca.pem` / `leaf.pem` / `leaf-key.pem`** — the one CA and one control leaf
  generated up front, so the phase 0 control and the on-demand arm serve
  byte-identical certificates.
- **`envoy-*.yaml`** — copied from `testdata/` at the start of each run, so
  editing the copies in `__run/` has no effect on the next run. Edit
  `poc/sdsmint/testdata/` instead.

The run ends with a `=== N passed, M failed ===` line. Treat failures as "a
threshold this harness asserts was crossed", then read `scale-results.txt` for
the number — a failure on a loaded machine is usually the machine.

Watch for the `????` marker. It is the harness telling you a number is below its
own noise floor — phase 0 emits one comparing the load generator's CPU per
connection against the handshake it just measured, and when the former is the
larger of the two, differences under about a millisecond between later phases
are the client, not Envoy. It is not a failure; it is a bound on what the rest
of the run can claim.

## Troubleshooting

**`something is already listening on: 18443 19000 19100`.** A previous run left
processes behind, most likely via `--keep` or an interrupt. This check exists
because the failure it prevents is silent: a leftover sdsmintd answers `/healthz`
and owns the UDS path, so the readiness probe passes for a process that is not
the one just started — and the harness then SIGTERMs a pid that may by then be a
brand-new server halfway through writing the control leaf. That is not
hypothetical; it is how this harness once produced a zero-byte `leaf.pem`.

```sh
pkill -f 'poc/sdsmint/__run/sdsmintd'; pkill -f 'poc/sdsmint/__run/envoy'
```

**`go: command not found`.** See [Prerequisites](#prerequisites) — `export PATH="$PATH:$HOME/go/bin"`.

**`the control leaf was not written`.** sdsmintd died during the setup step.
`tail -20 poc/sdsmint/__run/sds.log`.

**High-rate phases report failures.** Check the `open file limit:` and
`ephemeral ports:` line the run prints near the top before blaming Envoy.

**Phase 8 reports no connection completed.** It needs outbound DNS and a
reachable origin. Nothing else in the harness does.

## Related

- [`README.md` § How far it scales](README.md#how-far-it-scales) — the measured
  results, the two findings that changed the design, and what this harness
  cannot tell you.
- [`README.md` § What it costs](README.md#what-it-costs) — the Go
  microbenchmark numbers.
- [`README.md` § Measuring the deployed gateway](README.md#measuring-the-deployed-gateway-sdsload---connect-via) —
  `sdsload --connect-via` against a real cluster.
- [`EXPLAINER.md`](EXPLAINER.md) — what `on_demand_secret` is and why any of
  this is necessary.
