# ate-api-server cold resume capacity with and without OpenFGA authorization, 4 CPUs to 6 × 8 CPUs, 2026-10-20

- Eight cold resume tests, each run with authorization on (the install default) and again with it off (`ATE_API_EXPERIMENTAL_ENABLE_AUTHZ=false`), back to back on `nh-prow-parity`, 01:58–10:37 UTC, by `scripts/batch.sh` through `scripts/driver.py`, which calls `benchmarking/automation/orchestrator.py`'s own steps from a local checkout.
- All 16 runs complete. `6x8cpu_size10` with authorization on first failed at 06:00 UTC before touching the cluster (pushing the runner image to `gcr.io` failed with `no route to host` on the laptop running the batch; `orchestrator-failed-push.log`) and was rerun on its own, 12:45–13:30 UTC, by `scripts/rerun.sh`, with the same code, test entry and nodes.
- Code: PR 2686's branch `fake-dataplane-test-1` at `a9814339`, on `fix-fake-data-plane` (`600119d0`, fake-workersync resolves SandboxConfigs through `internal/workerpoolsandbox`) on `main` at `90de075f`, with the test entries of `benchmarking/automation/tests.yaml` as in the working tree then (`actorCPU: "5m"`, the converted multi-replica and size10 entries). `.ate-dev-env.sh` sets `SNAPSHOT_LOCATION=gs://snapshot-substrate-test-haiyanmeng-gke-dev/`, required since `6a552d59`.
- Results: `results/<authz-on|authz-off>/<test>/`, each with `stats.jsonl`, the per-step rows in `steps.jsonl`, `failures.csv`, `server_summary.json`, the driver's log as `orchestrator.log`, `run-times.txt`, and the upload path in `gcs-path.txt`. `results/summary.tsv` is the batch's run list.

## Setup

- Isolate mode: fake-atelet answers each call after 150 ms; 200 fake Workers; each actor books 5m CPU, so placement never binds.
- resumepaused: each user owns one actor; a cycle is one `ResumeActor` plus one `SuspendActor(ASYNCHRONOUS)`. Ladders of 3-minute steps, as in `tests.yaml`.
- ate-api-server replicas at 8 CPU and 4Gi each (4 CPU for the 4 CPU test), requests = limits. Read/write pool 64 for one replica, 40 per replica for several.
- Postgres: size0 (2 CPU request, 16 limit) for `4cpu`, `8cpu` and `2x8cpu`; size10 (80 CPU request, no limit, 140Gi, `max_connections = 300`) for the `_size10` tests. `pg_stat_statements` and postgres_exporter on all; `track_wal_io_timing=on` on the 2 × 8 and size10 tests.
- Install: `--cordon-control-plane --fake-data-plane`, plus `--cluster-size=size10` on the size10 tests.

**Nodes** (`nh-prow-parity`, us-central1-a)

| Role | Node | Machine |
|---|---|---|
| Postgres | `default-pool-…-4d75`, `ate.dev/workloadType=ate-postgres` | c3-highcpu-88 |
| Control plane, every ate-api-server replica | `default-pool-…-9v65`, `ate.dev/workloadType=ate-control-plane` | c3-highcpu-88 |
| Runner | `loadgen-pool-…-fl76`, tainted `ate.dev/benchmarking=true`, the Job pinned there by a `nodeSelector` | c3-highcpu-22 |
| Workers (fake-atelet) | 3 × `worker22-pool`, 7 × `worker-pool` | c3-highcpu-22, c3-highcpu-4 |

The runner kept each test's own CPU request (8, 12 or 16).

## Result

Ceiling is the plateau of achieved cycles/s once the steps stop reaching their target. ate-api-server cores and Postgres cores are at the ceiling.

| Test | Authz on: ceiling | Authz on: ate-api-server cores (busiest replica) | Authz on: Postgres cores | Authz off: ceiling | Authz off: ate-api-server cores (busiest replica) | Authz off: Postgres cores | On ÷ off |
|---|---|---|---|---|---|---|---|
| 1 × 4 CPU | about 935 (910–963) | 3.98 of 4, 21–25% throttled | 3.3 | about 1355 (1349–1366) | 3.7–4.0 of 4, 42–46% throttled | 3.8–4.2 | 0.69 |
| 1 × 8 CPU | about 1655 (1638–1664) | 7.80 of 8, 4% throttled | 5.7 | about 2445 (2415–2477) | 7.87 of 8, 16–21% throttled | 7.5 | 0.68 |
| 2 × 8 CPU, size0 | about 2665 (2657–2690) | 14.8 of 16 (7.69) | 10.2 | about 3670 (3655–3688) | 14.3 of 16 (7.44) | 12.4 | 0.73 |
| 2 × 8 CPU, size10 | about 2690 (2659–2717) | 14.4–15.0 of 16 (7.7) | 8.5–9.8 | about 3680 (3657–3711) | 14.0–14.3 of 16 (7.5) | 11.8–12.0 | 0.73 |
| 3 × 8 CPU, size10 | about 3470 (3436–3501) | 22.1 of 24 (7.7) | 14.3 | about 4920 (4877–4942) | 21.2 of 24 (7.4) | 17.6 | 0.71 |
| 4 × 8 CPU, size10 | about 4130 (4039–4163) | 28.7 of 32 (7.7) | 18.2 | about 5870 (5775–5946) | 28.1 of 32 (7.45) | 22.9 | 0.70 |
| 5 × 8 CPU, size10 | about 4860 (4769–4894) | 35.6 of 40 (7.6) | 20–24 | about 6770 (6683–6849) | 34.0 of 40 (7.2) | 27–29 | 0.72 |
| 6 × 8 CPU, size10 | about 5560 (5417–5668) | 41.6–42.2 of 48 (7.45) | 32–36 | about 7540 (7320–7559) | 39.5 of 48 (6.9) | 33.4 | 0.74 |

**Authorization costs about 30% of the ceiling at every size.** On ÷ off stays at 0.68–0.74 from one 4 CPU replica to six 8 CPU replicas.

**Where the ceilings come from**

- **Authorization on: ate-api-server's CPU up to 5 × 8, then both it and Postgres.** One replica sits at 97–100% of its limit; with 2 to 5, the busiest replica is at 7.6–7.7 of its 8 cores and the rest close behind, so the total in use falls only from 92% to 89% of ate-api-server's CPU, while Postgres uses at most 24 of size10's 80 cores. At 6 × 8 the busiest replica is at 7.45 and the total at 87%, and Postgres shows the commit-path pressure of the authorization-off 6 × 8 run: 84–138 active connections, 21–81 waiting on `WALWrite`, 14–37 on locks.
- **Authorization off: ate-api-server's CPU with one replica, Postgres's commit path from about five.** One replica is at its limit (42–46% throttled at 4 CPUs). With 2 to 4 replicas the busiest replica is at 7.4–7.5 of 8. At 5 × 8 it drops to 7.2 and at 6 × 8 to 6.9, while Postgres's active connections rise to 100–155 with 40–80 waiting on `WALWrite` and 10–26 on locks; at 6 × 8 330 cold resumes failed with `Aborted: concurrent update conflict` at the ceiling. That is the commit-path cap `ateapi-cold-4to9x8-size10-20261015/summary.md` (branch `ateapi-cold-resume-capacity`) found.

**Cost per cycle, below the ceiling**

| Test, step | ate-api-server CPU per cycle, on / off | Postgres CPU per cycle, on / off | Postgres commits per cycle, on / off |
|---|---|---|---|
| 1 × 4, 500/s | 5.2 / 3.5 ms | 2.9 / 2.1 ms | 39.1 / 26.1 |
| 3 × 8 size10, 3000/s | 6.3 / 4.5 ms | 4.1 / 2.9 ms | 37.5 / 26.0 |
| 4 × 8 size10, 4000/s | 6.9 / 4.9 ms | 4.5 / 3.3 ms | 35.5 / 26.0 |

- Authorization adds about 1.7–2.0 ms of ate-api-server CPU and about 1.1 ms of Postgres CPU per cycle, and about 13 commits: its OpenFGA `tuple` reads, 7 single-tuple lookups and 6 relation reads per cycle (`../ateapi-cold-2cpu-authz-20261019/results/pg-statements-per-cycle.txt`). OpenFGA runs inside ate-api-server.
- With authorization on, commits per cycle fall from 39 below the ceiling to 33–35 at it; with it off they stay at 26.0 throughout.
- ate-api-server's CPU per cycle rises with the number of replicas at a given rate (4.5 → 4.9 ms off, 6.3 → 6.9 ms on, from 3 to 4 replicas at 3000–4000/s), as the earlier series found.

## Against the earlier series

Branch `ateapi-cold-resume-capacity` measured the same tests on the same cluster from 2026-10-09 to 2026-10-15, on code from before authorization was enforced, so those runs compare with this series' authorization-off column.

| Test | Earlier ceiling | This series, authz off | Change |
|---|---|---|---|
| 1 × 4 CPU | about 1540 (`ateapi-cold-2-4-8cpu-20261009/summary.md`) | about 1355 | −12% |
| 1 × 8 CPU | about 2610–2627 | about 2445 | −6% to −7% |
| 2 × 8 CPU, size0 | about 3670–3735 | about 3670 | 0% to −2% |
| 2 × 8 CPU, size10 | about 3680 (`summary-size10-replicas.md`) | about 3680 | 0% |
| 3 × 8 CPU, size10 | about 4930 | about 4920 | 0% |
| 4 × 8 CPU, size10 | about 5930–5950 (`ateapi-cold-4to9x8-size10-20261015/summary.md`) | about 5870 | −1% |
| 5 × 8 CPU, size10 | about 6750–6870 | about 6770 | 0% |
| 6 × 8 CPU, size10 | about 7340–7400 | about 7540 | +2% to +3% |

- Where ate-api-server's CPU sets the ceiling, one replica, current code is 6–12% slower: each cycle costs more ate-api-server CPU even without authorization, as the 2 CPU comparison found (about 2.7 against 2.45 ms per cycle, `../ateapi-cold-2cpu-authz-20261019/summary.md`). Candidates are the move from `PauseActor` to `SuspendActor(ASYNCHRONOUS)`, the extra `worker_assignments` read and the user class (resumepaused here, resumecold then); none is measured.
- Where Postgres's commit path sets it, several replicas, the ceilings match within run-to-run variation (about ±2% in the earlier series).

## Caveats

- **First steps include warm-up.** Each user creates and pauses its actor in the step that adds it. With authorization on, the first step of `2x8cpu` reached 1128/s and of `2x8cpu_size10` 2296/s against 2500; they are left out of the ceilings.
- **Latency above the ceiling is bucket-limited.** The histograms' buckets double, so percentiles at saturation are interpolated inside wide buckets.
- **The plateau is also closed-loop.** Each user waits for its cycle before the next, so once calls queue the users cannot offer more; what brings on the plateau is the limit named above.
- **Failures.** None in 13 runs. `2x8cpu` with authorization on: one warm-up resume cancelled and one delete aborted. `6x8cpu_size10`, all at or near the ceiling: with authorization off, 330 cold resumes and 103 warm-ups `Aborted: concurrent update conflict` and 176 `ResourceExhausted` (2 cold resumes, 174 warm-ups), out of 13.4 million cold resumes; with authorization on, 331 cold resumes and 44 warm-ups `Aborted: concurrent update conflict` and 357 cold resumes `ResourceExhausted: no worker has room for the actor`, out of 10.8 million.

## Scripts

- `scripts/batch.sh`: runs each test with authorization on, then each with it off, one log per run, a line per run in `summary.tsv`; a failed run does not stop it. Run under `systemd-inhibit --what=sleep:idle:handle-lid-switch --mode=block` with `setsid nohup`, since the laptop running it suspends after 15 idle minutes.
- `scripts/rerun.sh`: runs one test (`6x8cpu_size10`, authorization on) the same way and appends its line to `summary.tsv`.
- `scripts/driver.py`: runs one `tests.yaml` entry through the orchestrator's steps from a local checkout against the cluster in `$KUBECONFIG`; `RUNNER_CPU` overrides the entry's `runnerCpu`, `RUNNER_NODE_SELECTOR=key=value` pins the runner Job.
