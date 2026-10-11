# ate-api-server cold resume capacity from 2 to 16 CPUs, 2026-10-09

- Tests `ateapi_cold_resume_pause_fake_2cpu`, `_4cpu` and `_8cpu`, run back to back with the local orchestrator on branch `fake-dataplane-test` at `4ded1d6b`.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88), 13:17–14:49 UTC. Each test complete, no request failed, no `FleetExhausted`.
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_{2,4,8}cpu/run_date=2026-10-09/` (run_ts 1791552193, 1791554033, 1791555848); local copies and per-step analysis (`steps.jsonl`) in `results/{2cpu,4cpu,8cpu}/`. Orchestrator log: `~/bench-runs/ateapi-cold-2-4-8cpu-20261009/orchestrator.log`.
- Repeat of the 4 CPU test, same commit and test entry, 15:40–16:12 UTC: complete, no request failed. Results under run_ts 1791560781, local copy in `results/4cpu-run2/`, log in `~/bench-runs/ateapi-cold-4cpu-run2-20261009/orchestrator.log`.
- The 8 CPU test on a fine ladder, at `f9453cc2` (which changes only that test's entry), 16:28–16:58 UTC: complete, no request failed. Results under run_ts 1791563724, local copy in `results/8cpu-fine/`, log in `~/bench-runs/ateapi-cold-8cpu-fine-20261009/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_10cpu`, at `034f879e` (which adds that test), 17:24–17:55 UTC: complete, no request failed. Results under run_ts 1791567119, local copy in `results/10cpu/`, log in `~/bench-runs/ateapi-cold-10cpu-20261009/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_2x2cpu`, at `034f879e`, 18:03–18:34 UTC: complete, no request failed. Results under run_ts 1791569350, local copy in `results/2x2cpu/`, log in `~/bench-runs/ateapi-cold-2x2cpu-20261009/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_2x4cpu`, at `034f879e`, 18:49–19:22 UTC: complete, no request failed. Results under run_ts 1791572118, local copy in `results/2x4cpu/`, log in `~/bench-runs/ateapi-cold-2x4cpu-20261009/orchestrator.log`. Postgres restarted onto a different node in this run, so ate-api-server ran on `…-4d75` and the runner on `…-9v65`; all three are the same machine type, each role on its own node.
- `ateapi_cold_resume_pause_fake_2x8cpu`, at `00f9fc57` (which adds that test), 21:42–22:12 UTC: complete, no request failed. Results under run_ts 1791582469, local copy in `results/2x8cpu/`, log in `~/bench-runs/ateapi-cold-2x8cpu-20261009/orchestrator.log`. A first attempt at 20:31 is invalid: the GKE API server failed requests at 20:38, the orchestrator's partial teardown deleted the benchmark templates, and every resume after that failed; its results are kept in `results/2x8cpu-invalid-20261009T2038/` with an explanation.
- `ateapi_cold_resume_pause_fake_3x4cpu` and `_4x4cpu`, at `30d76d8c` (which adds those tests), run back to back on 2026-10-10, 20:19–21:50 UTC: both complete, no request failed. Results under run_ts 1791663938 and 1791666408, local copies in `results/3x4cpu/` and `results/4x4cpu/`, with Postgres's WAL fsync, commit and wait counters sampled every 15 s for both runs in `results/4x4cpu/wal-io-3x4-and-4x4.csv` (`track_wal_io_timing` off, so counts without times); log in `~/bench-runs/ateapi-cold-3x4-4x4cpu-20261010/orchestrator.log`.
- The 1M-actor runs on size10 Postgres, 2026-10-10, are in `summary-size10.md`; 2 × 8 CPU against three Postgres setups is in `summary-2x8-postgres.md`; and 2 to 8 × 8 CPU replicas on size10 Postgres are in `summary-size10-replicas.md`.

## Setup

- Isolate mode (fake data plane): 3 fake-atelets, each AteomHerder call answered after 150 ms; 200 fake Workers of 1000 actors each, so placement never binds.
- resumecold: each user takes a paused actor from a shared fleet, resumes it, pauses it, and returns it. The fleet equals the user count: 1000 users for 2 CPU, 2000 for 4 and 8 CPU, 3000 for the 8 CPU fine run, 4000 for 10 CPU, 2000 for 2 × 2 CPU, 4000 for 2 × 4 CPU, and 6000 for 2 × 8, 3 × 4 and 4 × 4 CPU. One cycle is one resume plus one pause.
- Ladders of 3-minute steps: 2 CPU 100–1500/s, 4 CPU 250–2500/s, 8 CPU 250–3000/s, the 8 CPU fine run 2400–2900/s in steps of 100 with 3000 users and actors, 10 CPU 2400–3400/s in steps of 200 with 4000, 2 × 2 CPU the same 250–2500/s as 4 CPU, 2 × 4 CPU 500–4000/s, 2 × 8 and 4 × 4 CPU 2500–5000/s in steps of 500, and 3 × 4 CPU 2000–4500/s in steps of 500.
- ate-api-server and Postgres: see Resources below.
- Placement: Postgres, ate-api-server and the runner each on their own node; fake-atelet on all three.
- The test definition is from https://github.com/haiyanmeng/substrate/pull/32/ 

## Resources

Each test installed substrate fresh with `hack/install-ate.sh` at the default cluster size, `size0`, then `pre_test` applied the test's `ateapi:` block. The values below are what the orchestrator log shows each test applied.

**ate-api-server**

| | Installed default | 2 CPU test | 4 CPU test | 8 CPU test | 10 CPU test | 2 × 2 CPU test | 2 × 4 CPU test | 2 × 8 CPU test | 3 × 4 CPU test | 4 × 4 CPU test |
|---|---|---|---|---|---|---|---|---|---|---|
| Replicas | 2 | 1 | 1 | 1 | 1 | 2 | 2 | 2 | 3 | 4 |
| CPU request / limit | none (BestEffort) | 2 / 2 | 4 / 4 | 8 / 8 | 10 / 10 | 2 / 2 each | 4 / 4 each | 8 / 8 each | 4 / 4 each | 4 / 4 each |
| Memory request / limit | none | 4Gi / 4Gi | 4Gi / 4Gi | 4Gi / 4Gi | 4Gi / 4Gi | 4Gi / 4Gi each | 4Gi / 4Gi each | 4Gi / 4Gi each | 4Gi / 4Gi each | 4Gi / 4Gi each |
| `GOMAXPROCS` (from the CPU limit) | node's CPUs | 2 | 4 | 8 | 10 | 2 each | 4 each | 8 each | 4 each | 4 each |
| Read/write DB pool per replica | 64 (size0 DSN) | 64 | 64 | 64 | 64 | 40 | 40 | 40 | 26 | 18 |
| Owner and watch pools per replica | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 | 2 + 3 |

The 8 CPU fine run applied the same settings as the 8 CPU test; it differs only in its ladder and its 3000 users. The pool is set through `ATE_API_POSTGRES_POOL_MAX_CONNS` in the `ate-api-server-envvars` ConfigMap. The two-replica tests' pools are 40 per replica so that both replicas, 2 × (40 + 5) = 90 connections, stay within Postgres's 100; both replicas share the ateapi node. For the same reason the 3 × 4 test's pools are 26, 3 × (26 + 5) = 93, and the 4 × 4 test's 18, 4 × (18 + 5) = 92, all replicas again on the ateapi node. Requests equal to limits make the pod Guaranteed; the CFS quota is the CPU limit × 100 ms per 100 ms period.

**Postgres** (`postgres-0`, size0; the same in every test)

| Container | CPU request / limit | Memory request / limit |
|---|---|---|
| `postgres` | 2 / 16 | 1Gi / 2Gi |
| `postgres-exporter` (added by `pre_test`) | 100m / none | 128Mi / none |
| `tls-reloader` (native sidecar) | 10m / none | 32Mi / none |

- `postgresql.conf` at size0 sets only TLS and the `pg_hba` path, so tuning is PostgreSQL's defaults: `max_connections = 100`, `shared_buffers = 128MB`.
- `pre_test` adds `shared_preload_libraries=pg_stat_statements` as a command-line flag, which restarts Postgres once before ate-api-server rolls out.
- Peak use was 2.37 cores (2 CPU test), 4.50 (4 CPU), 7.94 (8 CPU), 8.74 (10 CPU), 4.35 (2 × 2 CPU), 8.13 (2 × 4 CPU), 11.66 (2 × 8 CPU), 10.00 (3 × 4 CPU) and 10.62 (4 × 4 CPU): above the 2-CPU request in all of them, far below the 16-CPU limit, so never throttled. Connections stayed at 68–69 of 100 with one replica, 86–88 with two, 86–88 with three and 85–86 with four.

## Result

| ate-api-server CPUs | Ceiling (cycles/s) | Bound by |
|---|---|---|
| 2 | about 815 (808–818) | ate-api-server CPU: 2.00 cores, 23–25% throttled |
| 4 | about 1540 ± 1% (1522–1562, two runs) | ate-api-server CPU: 3.97–3.98 cores, 29–34% throttled |
| 8 | about 2610 (2598–2615, fine ladder) | ate-api-server CPU: 7.74–7.76 of 8 cores (97%), little throttling since `GOMAXPROCS` is 8; Postgres follows the load at about 7.9 of its 16 cores, with some contention. Confirmed by the 10 CPU run, whose ceiling rose |
| 10 | about 2900 (2880–2949) | Mostly ate-api-server, but it used only 9.24–9.36 of its 10 cores (93%), unthrottled, while its CPU per cycle kept rising; Postgres at up to 8.7 of 16 cores |
| 2 × 2 | about 1460 (1431–1469) | ate-api-server CPU: both replicas at their 2-core limits (1.86–1.99 each, 3.85–3.88 together) |
| 2 × 4 | about 2600 (2589–2613) | ate-api-server CPU: one replica at its 4-core limit (3.94–3.95), the other at 3.66–3.71; the same ceiling as one 8 CPU replica |
| 2 × 8 | about 3670 (3648–3674) | Postgres's commit path: 18–31 connections waiting on the WAL write at a time; neither ate-api-server (85% of 16 cores, unthrottled) nor Postgres (73% of 16) is CPU-bound |
| 3 × 4 | about 3160 (3151–3167) | Most likely Postgres's commit path, as at 2 × 8 (see Beyond two replicas); ate-api-server at 10.7 of 12 cores (89%), no replica at its 4-core limit (3.44–3.77), throttled in at most 1.1% of periods |
| 4 × 4 | about 3385 (3369–3393) | Most likely Postgres's commit path, as at 2 × 8 (see Beyond two replicas); ate-api-server at 12.9 of 16 cores (81%), no replica at its 4-core limit (3.09–3.45), unthrottled |

| ate-api-server CPUs | Ceiling (cycles/s) | Gain over the previous size |
|---|---|---|
| 2 | about 815 | |
| 4 | about 1540 | 1.89× for 2× the CPU |
| 8 | about 2610 | 1.69× for 2× the CPU |
| 10 | about 2900 | 1.11× for 1.25× the CPU |
| 16 (2 × 8) | about 3670 | 1.26× for 1.6× the CPU |

Throughput keeps rising with ate-api-server's total CPU, but above about 2000/s each cycle costs more, whether the CPU is in one replica or two (see Replicas versus CPU), and at about 3700/s Postgres's commit path stops it (see Postgres's commit path). Well below the ceiling, a resume takes 157–160 ms (150 ms of it fake-atelet) and a pause about the same, rising to 170–190 ms just below it; past it, both rise to 600+ ms as calls queue at the server, until every user is busy.

**Replicas versus CPU.** Splitting ate-api-server's CPU across two replicas did not raise the ceiling:

| Total CPUs | One replica | Two replicas |
|---|---|---|
| 4 | about 1540/s | 2 × 2: about 1460/s, 5% less |
| 8 | about 2610/s | 2 × 4: about 2600/s, the same |

In both two-replica tests the load split evenly below the ceiling and both replicas saturated, though at 2 × 4 one replica sat at its limit while the other had about 0.3 cores spare. What sets the ceiling is ate-api-server's CPU per cycle, and that depends on the cycle rate, not on how the CPU is divided:

| Configuration | Rate (cycles/s) | ate-api-server CPU per cycle |
|---|---|---|
| 1 × 4 | about 1500 | 2.53 ms |
| 2 × 2 | about 1470 | 2.64 ms |
| 1 × 8 | about 2600 | 2.96–2.99 ms |
| 2 × 4 | about 2600 | 2.93 ms |
| 2 × 8 | about 3670 | 3.70 ms |
| 3 × 4 | about 3160 | 3.37–3.38 ms |
| 4 × 4 | about 3385 | 3.80–3.84 ms |
| 1 × 10 | about 2900 | 3.1–3.2 ms |

Above about 2000/s each cycle costs more because the whole system is busier, with more contention on shared state, mostly through Postgres, whose own CPU per cycle rises the same way (2.2 ms at 1000/s to 3.1 ms at 2600/s). At 4 CPUs the second replica's own background work (Worker cache, template and assignment reconcilers) and its extra connections cost about 4%; at 8 CPUs that overhead no longer shows.

**Beyond two replicas.** Three and four 4 CPU replicas add far less than their CPU, and four end up below two 8 CPU replicas with the same 16 CPUs:

| Configuration | Total CPUs | Ceiling (cycles/s) | Against 2 × 4 | ate-api-server cores used | Commits/s at the ceiling | WAL fsyncs/s at the ceiling | Commits per fsync |
|---|---|---|---|---|---|---|---|
| 2 × 4 | 8 | about 2600 | 1.00× | 7.6 (95%) | about 65,000 | not sampled | |
| 3 × 4 | 12 | about 3160 | 1.21× for 1.5× the CPU | 10.7 (89%) | about 79,000 | 1,817–1,832 | 43 |
| 4 × 4 | 16 | about 3385 | 1.30× for 2× the CPU | 12.9 (81%) | about 85,000 | 1,786–1,810 | 47 |
| 2 × 8 | 16 | about 3670 | 1.41× for 2× the CPU | 13.5 (85%) | about 92,000 | not sampled | |

- ate-api-server's CPU does not set these ceilings. No 4 CPU replica reached its limit: 3.44–3.77 cores at 3 × 4 and 3.09–3.45 at 4 × 4, throttled in at most 1.1% of periods, and the share of ate-api-server's total CPU in use fell as replicas were added, from 95% to 89% to 81%.
- The WAL counters point at Postgres's commit path, as at 2 × 8. WAL fsyncs ran at about 1,800–2,000/s at every step, from 1,952/s at 2000/s to about 1,820/s at the ceiling, in both runs: a flush running back to back regardless of load, as the size10 run with WAL timing measured directly (`summary-size10.md`). Throughput is then fsyncs/s times the commits each fsync covers, which rose with load and levelled off at about 43 at 3 × 4 and 47 at 4 × 4. At 2 × 8, 92,000 commits/s would be about 50 per fsync if its fsync rate was the same; that run sampled no fsyncs.
- Lock waits stayed under one connection at every step, and 19–34 connections waited on `WALWrite`, as at 2 × 8.
- Why more, smaller replicas leave fewer commits per fsync is not shown by this data. ate-api-server's CPU per cycle also rose with the replica count, to 3.8 ms at 4 × 4 against 3.7 ms at 2 × 8 at a lower rate, consistent with each replica's own background work (Worker cache, reconcilers, the outbox watch poll) adding up.
- Past the ceiling the latency spread is wider than at 2 × 8: a resume's server p50 was about 200–390 ms and its p90 2.3–2.8 s at 4 × 4, against 820 ms and 1.5 s at 2 × 8, so some calls waited far longer than others.

**Postgres's commit path.** At 2 × 8 CPUs the ceiling, about 3670/s, is set by neither CPU: ate-api-server used 13.5 of its 16 cores with no throttling, and Postgres 11.6 of its 16. Disk bandwidth and memory are not it either: Postgres wrote a steady 37–43 MiB/s at every step, and its I/O, CPU and memory pressure stalls stayed at about 2% or less (cAdvisor's `container_pressure_*`). What grows is the wait on the write-ahead log: broken down by wait event (postgres-exporter's `pg_stat_activity_count`), 18–31 active connections at a time were waiting on `LWLock: WALWrite`, against about 5 running a query. Every commit must flush its WAL record to disk before it returns, and at about 92,000 commits/s, 25 per cycle, commits queue behind that flush: work bound by flush latency, which more CPU or more disk bandwidth does not shorten. The same queue was already there in earlier runs, 11–22 connections in the 8 and 10 CPU runs and from about 2000/s in the 2 × 4 run, but there ate-api-server's CPU ran out first, as the ceiling rising with its CPU shows. Waiting on the WAL write sleeps rather than spins, so it does not explain the rising CPU per cycle. Raising the ceiling past about 3700/s needs a cheaper commit path: fewer commits per cycle, Postgres group commit (`commit_delay`, `commit_siblings`) and a larger `wal_buffers`, or lower-latency storage for the WAL.

## Per step

Columns as in `analyze.py`: rates are successful calls over the step's window ÷ its length; latencies are resume means unless marked; cores are cAdvisor CPU over the window; commits and connections come from postgres-exporter. Server p50 / p90 / p95 / p99 are quantiles of ate-api-server's resume handler time (`histogram_quantile` over the window's bucket increases) and are bucket-limited; see Confidence and limits.

**2 CPU (1000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 100 | 100.0 | 156.4 / 156.6 | 154 / 195 / 200 / 204 | 155.7 | 0.34 | 0% | 0.21 | 2,535 | 3.1 |
| 200 | 200.0 | 157.1 / 157.3 | 154 / 195 / 200 / 204 | 156.5 | 0.63 | 0% | 0.42 | 5,018 | 3.8 |
| 400 | 400.0 | 157.9 / 158.1 | 154 / 195 / 200 / 204 | 157.3 | 1.15 | 0% | 0.84 | 10,025 | 4.8 |
| 600 | 600.0 | 159.8 / 159.8 | 154 / 195 / 200 / 204 | 158.9 | 1.60 | 0% | 1.21 | 15,005 | 8.1 |
| 800 | 800.0 | 168.1 / 167.6 | 154 / 195 / 200 / 204 | 165.4 | 1.94 | 5.6% | 1.95 | 20,010 | 10.9 |
| 1000 | 808.5 | 635.0 / 631.6 | 614 / 778 / 799 / 815 | 591.6 | 2.00 | 23.1% | 2.35 | 20,246 | 15.6 |
| 1500 | 818.4 | 628.7 / 626.0 | 614 / 778 / 799 / 815 | 587.2 | 2.00 | 24.6% | 2.37 | 20,513 | 8.1 |

**4 CPU, run 1 (2000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 250 | 250.0 | 157.1 / 157.4 | 154 / 195 / 200 / 204 | 156.4 | 0.85 | 0% | 0.52 | 6,289 | 3.2 |
| 500 | 500.0 | 157.6 / 157.8 | 154 / 195 / 200 / 204 | 157.1 | 1.59 | 0% | 1.03 | 12,479 | 6.8 |
| 1000 | 1000.0 | 158.6 / 158.8 | 154 / 195 / 200 / 204 | 157.9 | 2.88 | 0% | 2.20 | 25,050 | 10.8 |
| 1500 | 1500.1 | 166.9 / 166.6 | 154 / 196 / 201 / 219 | 165.6 | 3.80 | 1.7% | 4.03 | 37,586 | 14.9 |
| 1750 | 1531.5 | 660.7 / 659.2 | 614 / 778 / 799 / 815 | 615.6 | 3.98 | 34.2% | 4.44 | 38,317 | 11.0 |
| 2000 | 1521.9 | 672.6 / 671.3 | 614 / 778 / 799 / 815 | 624.7 | 3.98 | 30.5% | 4.43 | 38,135 | 17.4 |
| 2500 | 1546.2 | 664.4 / 663.0 | 614 / 778 / 799 / 815 | 618.1 | 3.68 | 32.3% | 4.50 | 38,638 | 19.7 |

**4 CPU, run 2 (2000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 250 | 250.0 | 156.8 / 157.0 | 154 / 195 / 200 / 204 | 156.0 | 0.82 | 0% | 0.52 | 6,267 | 3.8 |
| 500 | 500.0 | 157.2 / 157.4 | 154 / 195 / 200 / 204 | 156.6 | 1.55 | 0% | 1.04 | 12,574 | 6.0 |
| 1000 | 1000.0 | 158.1 / 158.2 | 154 / 195 / 200 / 204 | 157.3 | 2.83 | 0% | 2.17 | 25,003 | 11.0 |
| 1500 | 1499.9 | 166.2 / 166.0 | 154 / 196 / 201 / 212 | 165.2 | 3.76 | 2.3% | 3.99 | 37,492 | 18.1 |
| 1750 | 1547.7 | 653.1 / 651.6 | 614 / 778 / 799 / 815 | 608.3 | 3.97 | 32.3% | 4.46 | 38,668 | 21.9 |
| 2000 | 1541.1 | 664.8 / 663.5 | 614 / 778 / 799 / 815 | 616.7 | 3.98 | 30.4% | 4.49 | 38,504 | 13.3 |
| 2500 | 1562.1 | 658.0 / 656.6 | 614 / 778 / 799 / 815 | 610.1 | 3.98 | 28.9% | 4.52 | 39,070 | 14.8 |

Run 2 repeated run 1 with the same commit and test entry. Below the ceiling every step matches run 1; the saturated steps averaged 1550/s against run 1's 1533/s, 1.1% apart. Run 1's 3.68 ateapi cores at 2500/s is the only saturated reading below 3.97 in either run, and run 2 shows 3.98 at that step, so it is most likely a cAdvisor sampling artifact.

**8 CPU, coarse ladder (2000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 250 | 250.0 | 157.2 / 157.4 | 154 / 195 / 200 / 204 | 156.4 | 0.86 | 0% | 0.53 | 6,283 | 4.0 |
| 500 | 500.0 | 157.5 / 157.7 | 154 / 195 / 200 / 204 | 157.0 | 1.69 | 0% | 1.04 | 12,493 | 6.1 |
| 1000 | 1000.0 | 158.0 / 158.2 | 154 / 195 / 200 / 204 | 157.4 | 3.03 | 0% | 2.20 | 25,039 | 11.0 |
| 1500 | 1500.0 | 158.5 / 158.7 | 154 / 195 / 200 / 204 | 157.9 | 4.76 | 0% | 3.50 | 37,483 | 16.7 |
| 2000 | 2000.0 | 160.2 / 160.3 | 154 / 195 / 200 / 205 | 159.2 | 6.08 | 0% | 4.66 | 50,072 | 21.0 |
| 2500 | 2497.2 | 171.4 / 171.0 | 157 / 200 / 231 / 390 | 168.1 | 7.20 | 0.4% | 7.04 | 62,466 | 31.7 |
| 3000 | 2638.7 | 383.4 / 382.6 | 310 / 394 / 404 / 648 | 363.3 | 7.70 | 2.5% | 7.94 | 66,011 | 28.0 |

Open Postgres connections were 68–69 at every loaded step (pool 64 plus the owner and watch pools and the exporter); about one connection waited on a lock in the busier steps.

**8 CPU, fine ladder (3000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 2400 | 2400.0 | 171.7 / 171.3 | 157 / 200 / 237 / 388 | 168.5 | 7.04 | 0.3% | 6.69 | 60,192 | 25.9 |
| 2500 | 2500.2 | 175.6 / 175.4 | 157 / 201 / 243 / 383 | 173.3 | 6.78 | 0.2% | 7.10 | 62,595 | 24.0 |
| 2600 | 2602.3 | 189.0 / 188.6 | 166 / 302 / 356 / 399 | 186.4 | 7.48 | 1.0% | 7.02 | 64,945 | 24.1 |
| 2700 | 2614.6 | 582.7 / 582.1 | 614 / 778 / 799 / 815 | 548.6 | 7.74 | 4.6% | 7.91 | 65,292 | 19.8 |
| 2800 | 2598.0 | 587.8 / 587.1 | 615 / 779 / 800 / 816 | 553.0 | 7.75 | 4.0% | 7.90 | 65,080 | 30.4 |
| 2900 | 2598.7 | 588.0 / 587.3 | 614 / 778 / 799 / 815 | 553.6 | 7.76 | 4.1% | 7.83 | 64,985 | 24.6 |

2600/s still held, with latency starting to rise; from 2700/s throughput stays at 2598–2615 while latency jumps to about 585 ms. At those steps 3000 users ÷ the cycle time is 2627–2651/s, so every user is busy and the server is the limit. ate-api-server ran at 7.74–7.76 cores at every saturated step. Postgres levelled off at about 7.9 cores together with throughput; if it were the bottleneck, ate-api-server would wait on it and use less CPU. Only 20–30 of the 64 pool connections were active.

**10 CPU (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 2400 | 2400.0 | 163.7 / 163.7 | 155 / 197 / 202 / 335 | 161.3 | 7.72 | 0% | 6.43 | 60,018 | 27.9 |
| 2600 | 2600.0 | 166.9 / 166.8 | 155 / 197 / 202 / 344 | 165.3 | 8.14 | 0% | 7.27 | 65,081 | 24.9 |
| 2800 | 2800.1 | 176.5 / 176.5 | 158 / 202 / 281 / 401 | 174.1 | 8.63 | 0% | 8.02 | 69,968 | 29.6 |
| 3000 | 2948.6 | 431.3 / 431.0 | 405 / 741 / 782 / 816 | 409.7 | 9.24 | 0.1% | 8.74 | 73,937 | 27.9 |
| 3200 | 2880.0 | 709.8 / 709.5 | 619 / 786 / 807 / 1228 | 668.4 | 9.34 | 0% | 8.68 | 71,900 | 28.8 |
| 3400 | 2898.7 | 705.7 / 705.3 | 618 / 785 / 806 / 1176 | 664.6 | 9.36 | 0% | 8.16 | 72,492 | 27.0 |

2800/s held; 3000/s fell just short at 2949/s with latency rising to 431 ms, and 3200 and 3400/s settled at 2880–2899/s with latency at about 706 ms, slightly lower under the deeper overload. At those steps 4000 users ÷ the cycle time equals the achieved rate, so every user is busy and the server is the limit. The ceiling rose by about 300/s over 8 CPUs, so ate-api-server's CPU did limit the 8 CPU test. But at saturation ate-api-server used only 93% of its cores, unthrottled, so a growing share of the slowdown now comes from something other than CPU.

**2 × 2 CPU (2000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (both) | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|
| 250 | 250.0 | 156.9 / 157.1 | 154 / 195 / 200 / 204 | 156.1 | 0.84 | 0% | 0.54 | 6,294 | 3.7 |
| 500 | 500.0 | 157.3 / 157.5 | 154 / 195 / 200 / 204 | 156.7 | 1.57 | 0% | 1.05 | 12,517 | 5.7 |
| 1000 | 1000.0 | 159.3 / 159.5 | 154 / 195 / 200 / 204 | 158.3 | 2.91 | 0% | 2.30 | 25,106 | 11.7 |
| 1500 | 1467.7 | 418.3 / 417.2 | 192 / 1264 / 1451 / 1601 | 394.3 | 3.85 | 4.7% | 4.18 | 36,729 | 23.4 |
| 1750 | 1465.0 | 701.4 / 699.9 | 711 / 1470 / 1554 / 1622 | 653.8 | 3.88 | 8.8% | 4.35 | 36,719 | 19.5 |
| 2000 | 1468.9 | 699.6 / 698.1 | 716 / 1469 / 1554 / 1621 | 654.2 | 3.87 | 9.4% | 4.35 | 36,789 | 14.2 |
| 2500 | 1430.6 | 720.4 / 718.9 | 822 / 1475 / 1557 / 1622 | 672.6 | 3.73 | 12.5% | 3.90 | 35,901 | 14.2 |

1000/s held; from 1500/s throughput stays at 1431–1469/s, where the single 4 CPU replica still held 1500/s at 166 ms. Per pod, the replicas used almost the same CPU at every step: 1.46 and 1.45 cores at 1000/s, and 1.86–1.99 each at the saturated steps, near their 2-core limits, so the client spread calls evenly and neither saturated first.

**2 × 4 CPU (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (both) | Per replica | Throttled | Postgres cores | Commits/s | Active PG conns |
|---|---|---|---|---|---|---|---|---|---|---|
| 500 | 500.0 | 157.5 / 157.8 | 154 / 195 / 200 / 204 | 156.9 | 1.77 | 0.89 / 0.88 | 0% | 1.07 | 12,521 | 5.8 |
| 1000 | 1000.0 | 158.0 / 158.2 | 154 / 195 / 200 / 204 | 157.3 | 3.35 | 1.67 / 1.69 | 0% | 2.25 | 25,132 | 9.3 |
| 2000 | 2000.0 | 160.9 / 161.0 | 154 / 196 / 201 / 224 | 159.6 | 6.06 | 3.03 / 3.03 | 0% | 4.89 | 50,058 | 20.9 |
| 2500 | 2500.7 | 176.6 / 176.1 | 157 / 200 / 246 / 620 | 172.2 | 7.17 | 3.59 / 3.59 | 0.9% | 7.34 | 62,525 | 27.2 |
| 3000 | 2613.3 | 787.8 / 787.2 | 821 / 1477 / 1559 / 1625 | 733.5 | 7.66 | 3.95 / 3.71 | 11.3% | 8.13 | 65,516 | 40.4 |
| 3500 | 2598.3 | 795.6 / 795.0 | 822 / 1475 / 1557 / 1622 | 738.6 | 7.62 | 3.95 / 3.67 | 11.8% | 8.05 | 64,993 | 32.6 |
| 4000 | 2589.0 | 798.6 / 798.0 | 821 / 1477 / 1558 / 1624 | 742.9 | 7.60 | 3.94 / 3.66 | 11.3% | 8.00 | 64,796 | 31.6 |

2500/s held; from 3000/s throughput stays at 2589–2613/s with latency at about 790 ms, the same ceiling as one 8 CPU replica. The replicas used identical CPU up to 2500/s; at the saturated steps one sat at its 4-core limit and the other at 3.66–3.71, so perfect balancing might add a few percent.

**2 × 8 CPU (6000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (both) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2500.0 | 161.5 / 161.7 | 154 / 196 / 201 / 283 | 160.2 | 9.37 | 4.73 / 4.63 | 6.82 | 62,625 | 28.7 | 18.1 |
| 3000 | 3000.1 | 163.3 / 163.3 | 155 / 197 / 202 / 337 | 161.5 | 11.04 | 5.56 / 5.48 | 8.67 | 75,074 | 41.1 | 28.0 |
| 3500 | 3500.0 | 175.1 / 175.0 | 159 / 204 / 309 / 418 | 170.8 | 12.53 | 6.30 / 6.23 | 10.65 | 87,450 | 38.7 | 23.7 |
| 4000 | 3647.5 | 845.5 / 845.4 | 820 / 1506 / 1591 / 2549 | 790.3 | 13.48 | 7.00 / 6.48 | 11.50 | 91,246 | 41.7 | 25.7 |
| 4500 | 3673.0 | 842.6 / 842.4 | 821 / 1504 / 1589 / 2499 | 786.2 | 13.54 | 7.01 / 6.52 | 11.58 | 91,900 | 41.9 | 28.6 |
| 5000 | 3673.9 | 841.3 / 841.0 | 801 / 1496 / 1581 / 2250 | 787.9 | 13.58 | 7.02 / 6.56 | 11.66 | 92,008 | 45.6 | 30.7 |

3500/s held at 175 ms; from 4000/s throughput stays at 3648–3674/s with latency at about 840 ms. ate-api-server was never throttled. The fleet warmed with no failures. The runner used 2.6–3.5 cores, well within its 8.

**3 × 4 CPU (6000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 2000 | 2000.0 | 160.3 / 160.5 | 154 / 195 / 201 / 205 | 159.2 | 7.06 | 2.39 / 2.34 / 2.33 | 5.33 | 50,052 | 25.4 | 17.1 |
| 2500 | 2500.0 | 162.2 / 162.3 | 154 / 196 / 201 / 290 | 160.7 | 8.55 | 2.89 / 2.83 / 2.82 | 7.13 | 62,545 | 26.0 | 17.4 |
| 3000 | 3000.1 | 169.6 / 169.4 | 156 / 199 / 205 / 379 | 166.8 | 9.71 | 3.34 / 3.28 / 3.09 | 9.15 | 75,076 | 37.0 | 23.7 |
| 3500 | 3158.2 | 975.2 / 975.0 | 278 / 2785 / 3031 / 3228 | 909.7 | 10.65 | 3.77 / 3.45 / 3.44 | 9.94 | 78,990 | 40.0 | 21.9 |
| 4000 | 3167.2 | 980.2 / 980.0 | 270 / 2788 / 3032 / 3228 | 907.5 | 10.70 | 3.77 / 3.47 / 3.46 | 10.00 | 79,258 | 36.6 | 23.4 |
| 4500 | 3151.3 | 984.9 / 984.7 | 265 / 2787 / 3032 / 3228 | 915.0 | 10.65 | 3.77 / 3.44 / 3.44 | 9.98 | 78,800 | 37.5 | 25.8 |

3000/s held at 169 ms; from 3500/s throughput stays at 3151–3167/s with a mean of about 980 ms. One replica used 3.77 cores and the other two 3.44–3.47 at the saturated steps, none at its 4-core limit; throttled in 0.4–1.1% of periods. The WAL sampler counted 1,817–1,832 fsyncs/s at the saturated steps, about 43 commits each.

**4 × 4 CPU (6000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2501.5 | 162.9 / 163.0 | 155 / 197 / 202 / 332 | 160.8 | 9.56 | 2.43 / 2.43 / 2.36 / 2.35 | 6.52 | 62,619 | 29.9 | 18.7 |
| 3000 | 3000.0 | 165.1 / 165.2 | 155 / 198 / 203 / 362 | 162.9 | 11.19 | 2.84 / 2.84 / 2.77 / 2.75 | 8.97 | 75,001 | 36.6 | 24.7 |
| 3500 | 3380.6 | 831.1 / 831.1 | 387 / 2297 / 2790 / 3185 | 776.2 | 12.98 | 3.40 / 3.36 / 3.12 / 3.10 | 10.53 | 84,580 | 37.0 | 23.3 |
| 4000 | 3368.7 | 919.5 / 919.5 | 196 / 2662 / 3002 / 3274 | 857.7 | 12.80 | 3.44 / 3.16 / 3.11 / 3.09 | 10.50 | 84,388 | 35.6 | 23.6 |
| 4500 | 3390.5 | 915.0 / 915.0 | 197 / 2641 / 2974 / 3240 | 852.6 | 12.87 | 3.44 / 3.19 / 3.14 / 3.11 | 10.62 | 84,708 | 43.3 | 25.0 |
| 5000 | 3393.2 | 914.7 / 914.7 | 197 / 2654 / 2990 / 3260 | 852.5 | 12.89 | 3.45 / 3.18 / 3.14 / 3.12 | 10.62 | 84,881 | 39.8 | 26.6 |

3000/s held at 165 ms; from 3500/s throughput stays at 3369–3393/s with a mean of 831–920 ms. The busiest replica used 3.40–3.45 cores and the others 3.09–3.36, none at its 4-core limit, and none was throttled. The WAL sampler counted 1,786–1,810 fsyncs/s at the saturated steps, about 47 commits each.

## Cost per cycle

| | ateapi CPU per cycle | Postgres CPU per cycle | Commits per cycle |
|---|---|---|---|
| 2 CPU, 800/s | 2.43 ms | 2.44 ms | 25.0 |
| 4 CPU, 1500/s | 2.53 ms | 2.69 ms | 25.1 |
| 8 CPU, 2000/s | 3.04 ms | 2.33 ms | 25.0 |
| 8 CPU, 2500/s | 2.88 ms | 2.82 ms | 25.0 |
| 8 CPU, 3000/s | 2.92 ms | 3.01 ms | 25.0 |
| 8 CPU fine, 2600/s | 2.87 ms | 2.70 ms | 25.0 |
| 8 CPU fine, 2800/s | 2.98 ms | 3.04 ms | 25.0 |
| 10 CPU, 2800/s | 3.08 ms | 2.86 ms | 25.0 |
| 10 CPU, 3000/s | 3.13 ms | 2.97 ms | 25.1 |
| 10 CPU, 3200/s | 3.24 ms | 3.01 ms | 25.0 |
| 2 × 2 CPU, 1000/s | 2.91 ms | 2.30 ms | 25.1 |
| 2 × 2 CPU, 1500/s | 2.62 ms | 2.85 ms | 25.0 |
| 2 × 2 CPU, 2000/s | 2.63 ms | 2.96 ms | 25.0 |
| 2 × 4 CPU, 2000/s | 3.03 ms | 2.44 ms | 25.0 |
| 2 × 4 CPU, 2500/s | 2.87 ms | 2.94 ms | 25.0 |
| 2 × 4 CPU, 3000/s | 2.93 ms | 3.11 ms | 25.0 |
| 2 × 8 CPU, 3000/s | 3.68 ms | 2.89 ms | 25.0 |
| 2 × 8 CPU, 3500/s | 3.58 ms | 3.04 ms | 25.0 |
| 2 × 8 CPU, 4000/s | 3.70 ms | 3.15 ms | 25.0 |
| 3 × 4 CPU, 3000/s | 3.24 ms | 3.05 ms | 25.0 |
| 3 × 4 CPU, 3167/s | 3.38 ms | 3.16 ms | 25.0 |
| 4 × 4 CPU, 3000/s | 3.73 ms | 2.99 ms | 25.0 |
| 4 × 4 CPU, 3381/s | 3.84 ms | 3.12 ms | 25.0 |

- Every cycle costs 25 Postgres commits, at every rate and size.
- ate-api-server's CPU per cycle near the ceiling grows with the cycle rate: about 2.4 ms at 815/s, 2.5–2.6 at about 1500/s, 2.9–3.0 at 2600/s and 3.1–3.2 at 2900/s. The 2 × 4 test, at the same rate as one 8 CPU replica and the same cost, shows it is the rate, not the core count per replica. It accounts for most of the falling gain per CPU. Well below the ceiling the cost is higher again (3.5 ms at 500/s for 2 × 4), as fixed background work is spread over fewer cycles.
- At 8 CPUs Postgres's CPU per cycle climbs from 2.33 to 3.01 ms between 2000 and 3000/s, a sign of contention; `pg_stat_statements` (in the cluster's Prometheus) would show which statements grow.

## Memory

Peak memory over each step's window, from cAdvisor and postgres-exporter; per step in each run's `steps.jsonl` (`ateapi_mem_*`, `postgres_mem_*`, `pg_cache_hit_ratio`, `pg_blks_read_per_s`). Limits: ate-api-server 4Gi per replica, Postgres 2Gi.

| Test | ate-api-server per replica, peak: steps that held / saturated steps | Postgres working set, lowest → highest step | Postgres RSS | Buffer cache hit rate, lowest step | Blocks read/s, highest step |
|---|---|---|---|---|---|
| 2 CPU | 88 / 115 MiB | 155 → 425 MiB | 179 MiB | 99.992% | 9 |
| 4 CPU, run 1 | 116 / 180 MiB | 195 → 449 MiB | 178 MiB | 99.980% | 43 |
| 4 CPU, run 2 | 118 / 180 MiB | 195 → 449 MiB | 179 MiB | 99.982% | 40 |
| 8 CPU, coarse | 147 / 183 MiB | 195 → 468 MiB | 182 MiB | 99.940% | 226 |
| 8 CPU, fine | 162 / 239 MiB | 432 → 506 MiB | 182 MiB | 99.894% | 396 |
| 10 CPU | 171 / 296 MiB | 436 → 537 MiB | 182 MiB | 99.853% | 609 |
| 2 × 2 CPU | 65 / 143 MiB | 236 → 502 MiB | 228 MiB | 99.990% | 22 |
| 2 × 4 CPU | 130 / 242 MiB | 340 → 568 MiB | 229 MiB | 99.916% | 323 |
| 2 × 8 CPU | 112 / 346 MiB | 490 → 613 MiB | 230 MiB | 99.861% | 760 |
| 3 × 4 CPU | 91 / 326 MiB | 514 → 600 MiB | not computed | not computed | not computed |
| 4 × 4 CPU | 68 / 315 MiB | 514 → 617 MiB | not computed | not computed | not computed |

- Memory limited nothing: ate-api-server used at most 346 MiB of its 4Gi and Postgres at most 613 MiB of its 2Gi, memory pressure stalls stayed at 0.01% of the time or less, and no container was OOM-killed.
- ate-api-server's memory jumps once a step saturates, from 112 to 342 MiB per replica between 3500 and 4000/s in the 2 × 8 run, as calls queue in the server and each holds its goroutine and buffers.
- Postgres's working set grows through each run as it touches more of its shared buffers and page cache, not with demand: its RSS stays at 180–230 MiB.
- The buffer cache hit rate falls as the rate rises, from 99.99% at 2 CPU to 99.86% at 2 × 8, where Postgres read 760 blocks/s that its 128MB `shared_buffers` did not hold. I/O pressure stayed at about 2% or less, so those reads were cheap, most likely from the page cache; size10's 35GB `shared_buffers` brings it to 100% (`summary-size10.md`).

## Reading the saturated steps

In every step past the ceiling, cycles/s equals users ÷ cycle time (2 CPU at 1500: 1000 ÷ 1.22 s = 822/s, measured 818): every user is busy, and latency rises until it absorbs the extra demand. That is what a closed-loop load shows when the server is the bottleneck. Adding users would raise latency, not throughput. At 8 CPUs, throttling stays low (about 4%) even at saturation, because `GOMAXPROCS` of 8 keeps ate-api-server from bursting past its quota; its 7.75 of 8 cores, not throttling, is the saturation signal there.

## Confidence and limits

**Solid**

- The 2 CPU ceiling, about 815 cycles/s: measured three times (2026-10-06, the 2026-10-09 tiny run and this run) within about 2% (803–818), with ate-api-server at 2.00 cores and 23–25% throttled each time.
- Behavior below the ceiling: throughput tracks the target at every size, and a resume or pause takes 157–168 ms, fake-atelet's 150 ms plus a few, across 18 steps in three tests.
- The 4 CPU ceiling, about 1540 cycles/s: two runs 1.1% apart (1533 and 1550 over the saturated steps), both with ate-api-server at 3.97–3.98 cores and 29–34% throttled.
- 25 Postgres commits per cycle at every step of every test, and about 2.4–2.5 ms of ate-api-server CPU per cycle at 2 and 4 CPUs.

**Directional**

- The 8 CPU ceiling, about 2610/s: one fine-ladder run. Its bottleneck is ate-api-server: it held at 97% of its 8 cores, and the 10 CPU run raised the ceiling.
- The 10 CPU ceiling, about 2900/s: one run. Below 100% CPU at saturation, so part of the limit is not CPU; what it is, this data cannot say.
- The 2 × 2 CPU ceiling, about 1460/s, 5% below one 4 CPU replica: one run, but the gap is larger than the ±1% run-to-run variation at 4 CPUs, and its cause, the higher CPU per cycle, is measured. Which of the listed overheads accounts for it is not.
- The 2 × 4 CPU ceiling, about 2600/s, the same as one 8 CPU replica: one run each, so "the same" means within a few percent.
- The 2 × 8 CPU ceiling, about 3670/s, and its cause, Postgres's commit path: one valid run. The WAL-write waits, with CPU, disk bandwidth and memory all below saturation, point clearly at it; a run with group commit or `synchronous_commit = off` would confirm it.
- The 3 × 4 and 4 × 4 ceilings, about 3160 and 3385/s, and their cause: one run each. That no replica reached its CPU limit is measured; that Postgres's WAL flush sets them rests on fsyncs/s staying flat at every load, with fsync times not recorded. 4 × 4 falling 8% below 2 × 8 at the same CPU is larger than the ±1% run-to-run variation, but why more replicas leave fewer commits per fsync is not measured.
- The 1.89×, 1.69× and 1.11× scaling factors: the 2 and 4 CPU ceilings repeat within about 1%; the 8 and 10 CPU ones rest on a single run each.
- ate-api-server's higher CPU per cycle at high rates: the 2 × 4 test shows it follows the rate, not the replica size, which points at system-wide contention rather than one replica's concurrency. Where in ate-api-server the extra CPU goes is not measured.

**Not conclusive**

- Server quantiles: `locust_server_duration_milliseconds` has doubling buckets (102.4, 204.8, 409.6, 819.2 ms), and `histogram_quantile` interpolates linearly within one. Nearly every call below the ceiling falls in the 102.4–204.8 ms bucket, so the 153.6 / 194.6 / 199.7 / 203.8 of those steps are positions in that bucket, saying only that calls took 102–205 ms; the mean, about 157 ms, is the better median. Likewise the 614 / 778 / 799 / 815 of the saturated steps say 410–819 ms. These are interpolated bucket bounds, not measurements; the means are precise.

**Scope**

- Isolate mode: a fixed 150 ms per call with no tail, no Worker churn, one placeholder object per snapshot, no router traffic, and pause rather than suspend.
- One configuration: size0 Postgres with default tuning (2Gi memory limit, `shared_buffers = 128MB`, default WAL and commit settings), one ate-api-server replica with pool 64 (two with pool 40 in the two-replica tests), c3-highcpu-88 nodes. Postgres tuning, above all of the commit path, may move the 8 CPU and larger results; `summary-size10.md` has the 1M-actor runs on size10 Postgres.
- No repeat run at 8 or 10 CPUs or of any multi-replica test; run-to-run variation is about ±1% at 2 and 4 CPUs.

**To make it conclusive**

1. The 2 × 8 CPU test with Postgres group commit (`commit_delay`, `commit_siblings`) and a larger `wal_buffers`, and once with `synchronous_commit = off` (benchmark only: it gives up durability) to see the ceiling without flush waits.
2. Fewer commits per cycle in ate-api-server: at 25 per resume and pause, the largest lever on the commit path.
3. A `pg_stat_statements` breakdown of the saturated steps of the 8 CPU, 10 CPU, 2 × 4 and 2 × 8 runs, from the cluster's Prometheus while it is retained: which statements commit most and which waits grow with the rate.
4. A Go CPU profile of ate-api-server at 2500–3000/s, to see where its CPU per cycle grows with the rate.
5. Repeat the 8 CPU and 2 × 8 CPU tests two or three times.
6. Finer server buckets, or Prometheus native histograms, if p99 matters.
7. The 3 × 4 and 4 × 4 CPU tests on size10 Postgres, with `track_wal_io_timing = on`, to see whether replicas scale with ate-api-server's total CPU once size0's commit path no longer caps them.

## How results are measured

`analyze.py` measures each step from 30 s after it starts to 5 s before it ends, 145 s of each 3-minute step: about 14 Prometheus samples for boomer's and the exporter's series and about 10 for cAdvisor's. The 2026-10-09 tiny run's summary (`~/bench-runs/ateapi-cold-tiny-20261009/summary.md`) describes each column in detail.


