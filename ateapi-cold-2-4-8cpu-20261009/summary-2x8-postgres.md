# ate-api-server at 2 × 8 CPUs against three Postgres setups, 2026-10-10

The 2 × 8 CPU cold resume test stops at about 3670 cycles/s on size0 Postgres, at what `summary.md` attributes to Postgres's commit path. These runs try to lift that path with Postgres alone: size10 Postgres, size0 with size10's WAL settings, and those plus group commit.

**Result: none of the three raises the ceiling.** size10 and the WAL settings reach the baseline within 1%, and group commit lowers it 4.5%. In every setup the WAL flush runs back to back at about 0.5 ms per fsync. Each fsync carries about 50 commits, against about 135 in the 10-replica size10 run of `summary-size10.md`. With two replicas, the ceiling looks set by how many commits ate-api-server keeps in flight, not by how Postgres is configured.

## Runs

- **Tests:** `ateapi_cold_resume_pause_fake_2x8cpu_size10`, `_2x8cpu_walconf` and `_2x8cpu_walconf_commitdelay`, run back to back with the local orchestrator on branch `fake-dataplane-test` at `2a04ee26`, which adds them and the `postgresArgs` setting of the `ateapi:` block. Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88).
- **Outcome:** every runner Job completed and uploaded its results, with no request failed (7,200,749, 7,173,792 and 7,010,968 requests).
- **The `timeout` verdicts are a reporting error.** The orchestrator reports `timeout` for the first two tests and exits 1. While it waited on each Job, the gcloud credentials kubectl uses stopped working ("cannot retrieve default token"). `wait_for_job`'s `kubectl get job` then hung inside `gke-gcloud-auth-plugin` until credentials came back, about 3 and 10 hours later. The benchmarks themselves ran normally.

| Test | Ladder (UTC) | run_ts | Local copy |
|---|---|---|---|
| `2x8cpu_size10` | 2026-10-10 22:31–22:49 | 1791671486 | `results/2x8cpu-size10/` |
| `2x8cpu_walconf` | 2026-10-11 01:44–02:02 | 1791683061 | `results/2x8cpu-walconf/` |
| `2x8cpu_walconf_commitdelay` | 2026-10-11 11:59–12:17 | 1791719981 | `results/2x8cpu-walconf_commitdelay/` |

- **Results:** `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_2x8cpu_{size10,walconf,walconf_commitdelay}/`.
- **WAL samples:** taken every 15 s from `pg_stat_io`, `pg_stat_wal`, `pg_stat_database` and `pg_stat_activity` for all three runs, in `results/2x8cpu-size10/wal-io-2x8-pgconf.csv`.
- **Log:** `~/bench-runs/ateapi-cold-2x8-pgconf-20261010/orchestrator.log`.
- **Baseline:** the 2 × 8 CPU run on size0 in `summary.md` (run_ts 1791582469, at `00f9fc57`), which recorded no fsync counts.
- **Follow-up:** 3 to 8 × 8 CPU replicas on size10 Postgres, which test the batch-size inference below, are in `summary-size10-replicas.md`.

## Setup

Everything except Postgres is the 2 × 8 CPU test of `summary.md`:
- **Data plane:** isolate mode (fake data plane), 3 fake-atelets answering each call after 150 ms, 200 fake Workers of 1000 actors each.
- **Load:** resumecold with 6000 users and actors, on a ladder of 3-minute steps from 2500 to 5000/s in steps of 500.
- **ate-api-server:** 2 replicas at 8 / 8 CPU, 4Gi, `GOMAXPROCS` 8, a read/write pool of 40 per replica, on their own node.
- **Placement:** Postgres and the runner on the other two nodes.

Each test sets `track_wal_io_timing = on` through `postgresArgs`, passed to Postgres as a `-c` flag in the restart `pre_test` makes to load `pg_stat_statements`.

| | Baseline (`summary.md`) | `2x8cpu_size10` | `2x8cpu_walconf` | `2x8cpu_walconf_commitdelay` |
|---|---|---|---|---|
| Install | size0 | size10 (`--cluster-size=size10`) | size0 | size0 |
| Postgres CPU request / limit | 2 / 16 | 80 / none | 2 / 16 | 2 / 16 |
| Postgres memory | 1Gi / 2Gi | 140Gi / 140Gi | 1Gi / 2Gi | 1Gi / 2Gi |
| `shared_buffers` | 128MB | 35GB | 128MB | 128MB |
| `max_connections` | 100 | 300 | 100 | 100 |
| `max_wal_size` / `min_wal_size` | 1GB / 80MB (defaults) | 16GB / 4GB | 16GB / 4GB | 16GB / 4GB |
| `checkpoint_completion_target` | 0.9 (default) | 0.9 | 0.9 | 0.9 |
| `wal_buffers` | 4MB (1/32 of `shared_buffers`) | 64MB | 64MB | 64MB |
| `wal_compression` | off | on (pglz) | on (pglz) | on (pglz) |
| `commit_delay` / `commit_siblings` | 0 / 5 (defaults) | 0 / 5 | 0 / 5 | 100 µs / 5 |
| Volume | 500Gi pd-balanced (`standard-rwo`) | the same | the same | the same |

`2x8cpu_size10` also has size10's other settings (parallelism, autovacuum, planner costs; `manifests/ate-install/postgres-size10/postgres-config-patch.yaml`). Postgres 18 accepted every `-c` setting (checked with `postgres -C` on the deployed image).

## Result

| Postgres | Ceiling (cycles/s) | Against the baseline | ate-api-server cores at the ceiling | Postgres cores | Commits/s |
|---|---|---|---|---|---|
| size0 (baseline) | about 3670 (3648–3674) | | 13.5 of 16 | 11.6 | about 92,000 |
| size10 | about 3680 (3652–3706) | the same | 13.4–13.6 | 11.8–12.0 | about 92,000 |
| size0 + WAL settings | about 3655 (3607–3693) | the same | 13.2–13.5 | 11.6–11.8 | about 91,500 |
| + group commit | about 3500 (3454–3541) | 4.5% lower | 12.7–13.0 | 11.0–11.3 | about 87,500 |

**The WAL flush, at each run's last steps that held and at its ceiling:**

| Postgres | Step | fsyncs/s | Mean fsync | Flush busy (fsync + write) | Commits per fsync | Waiting on `WALWrite` | Waiting on a lock |
|---|---|---|---|---|---|---|---|
| size10 | 3500/s | 1,820 | 0.52 ms | 97% | 48 | 33 | 0.7 |
| size10 | 4000/s (ceiling) | 1,800 | 0.52 ms | 97% | 50 | 29 | 0.4 |
| size0 + WAL | 3500/s | 1,827 | 0.52 ms | 98% | 48 | 29 | 0.2 |
| size0 + WAL | 4000/s (ceiling) | 1,769 | 0.53 ms | 97% | 51 | 35 | 0.1 |
| + group commit | 3500/s | 1,398 | 0.52 ms | 75% | 63 | 31 | 0.9 |
| + group commit | 4000–5000/s (ceiling) | 1,355–1,389 | 0.53–0.54 ms | 75–76% | 64 | 31–36 | 0.6–0.8 |

**Postgres's setup does not matter at this load.**
- size10's 80 CPUs, 35GB `shared_buffers` and WAL settings give the same fsync rate, fsync latency, commits per fsync and ceiling as size0 with only the WAL settings, and the same ceiling as the size0 baseline.
- So the difference between the size0 and size10 runs elsewhere (about 92,000 against about 173,000 commits/s) does not come from how Postgres is configured.
- Postgres used at most 12.1 cores, and memory and locks stayed far from any limit.

**The flush runs back to back.**
- Fsyncs take 97–98% of wall time at every step, at about 0.5 ms each, in both setups without group commit.
- Throughput is therefore fsyncs/s, about 1,800, times the commits each fsync covers. That grows with load and levels off at about 50 at the ceiling, about 92,000 commits/s, or 25 per cycle at about 3670 cycles/s.

**Batches are small because few commits wait at once.**
- In the 1M-actor size10 run of `summary-size10.md`, the same flush covered about 135 commits per fsync. That run had 10 ate-api-server replicas keeping 110–125 Postgres connections active; here, 2 replicas keep 39–48 active.
- A flush covers the commits waiting when it starts, so fewer commits in flight means smaller batches.
- At 2 × 8, ate-api-server is at 84–85% of its 16 cores, unthrottled. It cannot put more commits in flight without more CPU or less work per cycle.
- That the batch is bounded by ate-api-server's in-flight work, not by Postgres, is inferred from these runs and the size10 runs; no run measured it directly.

**Group commit trades fsyncs for batch size, and loses.**
- With `commit_delay = 100`, each commit that finds 5 other transactions open waits 100 µs before flushing.
- Batches grew from about 50 to 64 commits per fsync, but the flush now idles a quarter of the time: fsyncs fell from about 1,800 to about 1,380/s.
- The net is 4.5% fewer commits and cycles, and slower calls: a resume took 221 ms at 3500/s against 184 ms on size10.
- A shorter delay might break even; none is likely to win while the batch is bounded by the commits in flight.

## Per step

Columns as in `summary.md`. Server p50 / p90 / p95 / p99 are bucket-limited (see `summary.md`, Confidence and limits).

**size10 Postgres (6000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (both) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2500.0 | 162.1 / 162.2 | 154 / 196 / 201 / 279 | 160.6 | 9.24 | 4.67 / 4.58 | 7.13 | 62,533 | 23.7 | 13.2 |
| 3000 | 3000.0 | 166.4 / 166.5 | 155 / 198 / 203 / 379 | 163.7 | 10.91 | 5.50 / 5.42 | 8.97 | 74,995 | 36.2 | 23.9 |
| 3500 | 3499.6 | 183.9 / 183.6 | 160 / 229 / 338 / 585 | 179.2 | 12.47 | 6.27 / 6.20 | 11.05 | 87,543 | 42.9 | 33.2 |
| 4000 | 3651.6 | 844.2 / 844.1 | 820 / 1497 / 1582 / 2303 | 790.5 | 13.40 | 6.96 / 6.44 | 11.82 | 91,136 | 45.9 | 29.4 |
| 4500 | 3706.2 | 833.4 / 833.3 | 746 / 1503 / 1588 / 2487 | 782.3 | 13.59 | 7.04 / 6.54 | 12.00 | 92,896 | 39.2 | 23.1 |
| 5000 | 3680.8 | 841.4 / 841.3 | 821 / 1509 / 1595 / 2624 | 786.3 | 13.49 | 6.99 / 6.50 | 11.98 | 92,015 | 48.1 | 31.7 |

3500/s held at 184 ms; from 4000/s throughput stays at 3652–3706/s with latency at about 840 ms, the same as on size0. ate-api-server was never throttled.

**size0 with size10's WAL settings (6000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (both) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2500.0 | 162.4 / 162.5 | 155 / 196 / 201 / 296 | 160.8 | 9.24 | 4.66 / 4.58 | 7.06 | 62,630 | 28.7 | 17.9 |
| 3000 | 3000.1 | 167.1 / 167.1 | 156 / 198 / 203 / 382 | 164.0 | 10.89 | 5.49 / 5.40 | 8.96 | 75,006 | 33.0 | 23.0 |
| 3500 | 3500.0 | 190.0 / 189.5 | 161 / 268 / 363 / 661 | 183.9 | 12.44 | 6.28 / 6.15 | 10.94 | 87,668 | 38.2 | 29.3 |
| 4000 | 3607.3 | 852.8 / 852.7 | 820 / 1507 / 1593 / 2589 | 801.1 | 13.24 | 6.86 / 6.37 | 11.60 | 90,540 | 47.0 | 34.8 |
| 4500 | 3693.3 | 836.6 / 836.3 | 813 / 1498 / 1583 / 2342 | 782.8 | 13.51 | 7.01 / 6.51 | 11.84 | 92,321 | 46.7 | 30.1 |
| 5000 | 3661.9 | 845.0 / 844.9 | 820 / 1509 / 1595 / 2621 | 789.9 | 13.37 | 6.95 / 6.43 | 11.76 | 91,632 | 43.1 | 28.0 |

3500/s held at 190 ms; from 4000/s throughput stays at 3607–3693/s with latency at about 845 ms.

**size0 with the WAL settings and group commit (6000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (both) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2500.0 | 165.7 / 165.7 | 155 / 197 / 203 / 352 | 163.0 | 9.16 | 4.63 / 4.53 | 7.12 | 62,661 | 32.9 | 20.9 |
| 3000 | 3000.0 | 172.8 / 172.5 | 157 / 201 / 248 / 409 | 168.4 | 10.80 | 5.45 / 5.35 | 8.96 | 75,052 | 38.2 | 23.9 |
| 3500 | 3505.2 | 221.7 / 221.4 | 176 / 372 / 446 / 745 | 213.3 | 12.49 | 6.29 / 6.19 | 10.96 | 87,869 | 45.6 | 30.9 |
| 4000 | 3453.8 | 892.0 / 891.8 | 820 / 1530 / 1619 / 2858 | 833.3 | 12.72 | 6.59 / 6.12 | 11.03 | 86,145 | 51.9 | 36.0 |
| 4500 | 3540.9 | 872.9 / 872.8 | 770 / 1513 / 1600 / 2690 | 818.2 | 12.97 | 6.73 / 6.24 | 11.26 | 88,797 | 51.3 | 36.2 |
| 5000 | 3511.1 | 881.3 / 881.2 | 813 / 1523 / 1610 / 2796 | 826.0 | 12.89 | 6.69 / 6.20 | 11.21 | 87,726 | 50.9 | 30.7 |

3500/s held, but slower than in the other two runs (221 ms against 184–190); from 4000/s throughput stays at 3454–3541/s with latency at about 880 ms.

## Cost per cycle

| | ateapi CPU per cycle | Postgres CPU per cycle | Commits per cycle |
|---|---|---|---|
| size10, 3500/s | 3.56 ms | 3.16 ms | 25.0 |
| size10, 3652/s | 3.67 ms | 3.24 ms | 25.0 |
| size0 + WAL, 3500/s | 3.55 ms | 3.12 ms | 25.0 |
| size0 + WAL, 3607/s | 3.67 ms | 3.22 ms | 25.1 |
| + group commit, 3505/s | 3.56 ms | 3.13 ms | 25.1 |
| + group commit, 3454/s | 3.68 ms | 3.19 ms | 24.9 |
| size0 baseline, 3500/s (`summary.md`) | 3.58 ms | 3.04 ms | 25.0 |

The cost per cycle is the same in every setup. None of these Postgres changes makes a cycle cheaper for either side; they change only how commits are flushed.

## What would raise the 2 × 8 ceiling

The levers are on ate-api-server's side.

1. **Fewer write commits per cycle.** `dee31f7f` (branch `actor-update-in-worker-txn`) commits the actor's RESUMING update with the Worker bind and its PAUSED update with the release: 20 commits per cycle instead of 25, 8 writes instead of 10. On size10 it raised the 1M-actor ceiling 8% (`summary-size10.md`). The 2 × 8 test on that branch is the next run.
2. **Less ate-api-server CPU per cycle.** At about 3.7 ms per cycle and 85% of 16 cores, ate-api-server's CPU bounds the commits in flight. A Go CPU profile at about 3500/s would show where the time goes.
3. **More replicas only with CPU to match.** On size0, 4 × 4 did worse than 2 × 8 (`summary.md`). The 10 × 8 size10 run kept about 120 connections active and reached about 6900/s, so more throughput needs more replicas and CPUs together.
4. **`synchronous_commit = off`**, as a diagnostic only (it gives up durability), would show the ceiling with no flush wait at all. Untested.

## Confidence and limits

- **One run per setup.** The three ceilings without a clear change (3670, 3680, 3655) are within 1.5%, inside the run-to-run variation seen elsewhere. Group commit's 4.5% drop is larger than that, and its mechanism, fewer fsyncs per second, is measured.
- **Measured directly:** the flush's busy share, fsync latency and commits per fsync. Here `track_wal_io_timing` was on, and the sampler covered the steps through 4000/s in every run and every step of the group-commit run.
- **Inferred:** that the batch per fsync is bounded by ate-api-server's in-flight work, from the active connection counts here and in the 10-replica size10 runs. No run varied the in-flight work while holding the replica count fixed.
- **One delay value.** `commit_delay` was tried at 100 µs only.
- **Scope:** isolate mode with a 150 ms fake call, 6000 users, pause rather than suspend, c3-highcpu-88 nodes, and the code at `2a04ee26`, before `dee31f7f`.
