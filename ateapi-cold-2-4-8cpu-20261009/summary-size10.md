# ate-api-server cold resume at 1M actors on size10 Postgres, 1000 and 10,000 Workers, 2026-10-10

- `ateapi_cold_resume_pause_fake_1m_actors_size10`, run twice with the local orchestrator on branch `fake-dataplane-test`, cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88). Both complete, no request failed, no `FleetExhausted`.
- `ateapi_cold_resume_pause_fake_1m_actors_size10` with 10 ate-api-server replicas at 4 CPUs, at `e45c46a6` (which adds that test), 2026-10-10 00:48–01:45 UTC: complete, no request failed. Results under run_ts 1791593872, local copy in `results/1m-size10-10x4cpu/`, log in `~/bench-runs/ateapi-cold-1m-size10-20261009/orchestrator.log`.
- The same test with the replicas at 8 CPUs, at `7a343cef` (which changes only that test's CPU and ladder), 2026-10-10 01:55–02:50 UTC: complete, no request failed. Results under run_ts 1791597772, local copy in `results/1m-size10-10x8cpu/`, log in `~/bench-runs/ateapi-cold-1m-size10-10x8cpu-20261010/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_1m_actors_10k_workers_size10`, the 10 × 8 CPU test with 10,000 fake Workers instead of 1000, at `5af49ce9`, 2026-10-10 11:52–13:00 UTC: complete, 7,008,692 requests, none failed, no `FleetExhausted`. Results under run_ts 1791633612, local copy in `results/1m-size10-10x8cpu-10kworkers/`, log in `~/bench-runs/ateapi-cold-1m-10kworkers-size10-20261010/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_1m_actors_size10` (10 × 8 CPU, 1000 Workers) again with Postgres on pd-ssd, at `6b1dbd96` on branch `fake-dataplane-test-pdssd`, which puts the Postgres volume on `premium-rwo` and adds `track_wal_io_timing = on`; the test entry is unchanged. 2026-10-10 13:16–14:15 UTC: complete, 18,986,447 requests, none failed. Results under run_ts 1791638643, local copy in `results/1m-size10-10x8cpu-pdssd/`, with `wal-io.csv`, `pg_stat_io` and commit counters sampled from Postgres about every 15–40 s; log in `~/bench-runs/ateapi-cold-1m-size10-pdssd-20261010/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_1m_actors_size10` (10 × 8 CPU, 1000 Workers, pd-balanced) at `dee31f7f` on branch `actor-update-in-worker-txn`, which commits the actor's RESUMING update with the worker bind and its PAUSED update with the release (see Result); the test entry is unchanged. 2026-10-10 15:46–18:19 UTC: the runner Job completed in 29 minutes with 19,868,518 requests, none failed, and uploaded its results under run_ts 1791647785; local copy in `results/1m-size10-10x8cpu-txnfold/`, with `wal-io.csv` (fsync counts only: `track_wal_io_timing` was off); log in `~/bench-runs/ateapi-cold-1m-size10-txnfold-20261010/orchestrator.log`. The orchestrator reports exit 1 and a Job timeout because the gcloud credentials kubectl uses expired while it waited, so it never saw the Job complete; the results are unaffected.
- The same test repeated at `a2dc6e49` on branch `actor-update-in-worker-txn-waltiming`, which is `dee31f7f` plus `track_wal_io_timing = on`, on pd-balanced, 2026-10-10 18:37–19:17 UTC: complete, no request failed. Results under run_ts 1791657933, local copy in `results/1m-size10-10x8cpu-txnfold-waltiming/`, with `wal-io.csv` sampled every 15 s from `pg_stat_io`, `pg_stat_wal`, `pg_stat_database` and `pg_stat_activity` and covering every ladder step; log in `~/bench-runs/ateapi-cold-1m-size10-txnfold-waltiming-20261010/orchestrator.log`.
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_1m_actors_size10/run_date=2026-10-10/` and `…/ateapi_cold_resume_pause_fake_1m_actors_10k_workers_size10/run_date=2026-10-10/`.
- The 2 to 16 CPU runs on size0 Postgres, which these extend, are in `summary.md`; its "How results are measured" applies here too. Replica scaling on size10 with that smaller workload (2 to 8 × 8 CPUs) is in `summary-size10-replicas.md`.

## Setup

- Isolate mode (fake data plane): 3 fake-atelets answering every AteomHerder call with no delay; 1000 fake Workers of 1000 actors each, or 10,000 in the 10,000-Worker run (about 3,333 per node). Only actors mid-cycle hold a Worker slot, so placement never binds.
- resumecold: a fleet of 1,000,000 actors, warmed by create, resume and pause before the ladder, driven by 4000 users. Each user takes a paused actor from the fleet, resumes it, pauses it, and returns it; one cycle is one resume plus one pause. The fleet lives in one atespace.
- Ladders of 3-minute steps of 1000: 3000–10000/s at 10 × 4 CPU, 5000–12000/s at 10 × 8 CPU, 1000–7000/s at 10 × 8 CPU with 10,000 Workers.
- Placement: Postgres, ate-api-server and the runner each on their own node; fake-atelet on all three.

## Resources

Each run installed substrate fresh with `hack/install-ate.sh --cluster-size=size10`, then `pre_test` applied the test's `ateapi:` block.

| ate-api-server | 10 × 4 CPU run | 10 × 8 CPU run | 10 × 8 CPU, 10,000 Workers |
|---|---|---|---|
| Replicas | 10 | 10 | 10 |
| CPU request / limit, `GOMAXPROCS` | 4 / 4 each, 4 | 8 / 8 each, 8 | 8 / 8 each, 8 |
| Memory request / limit | 10Gi / 10Gi each | 10Gi / 10Gi each | 10Gi / 10Gi each |
| Read/write DB pool per replica | 20 | 20 | 20 |
| Owner and watch pools per replica | 2 + 3 | 2 + 3 | 2 + 3 |

All 10 replicas share the ateapi node, which holds the 80 CPUs of the 10 × 8 run. Ten replicas at 20 + 5 connections hold 250 of size10's 300; 226–232 were open.

| size10 Postgres container | CPU request / limit | Memory request / limit |
|---|---|---|
| `postgres` | 80 / none | 140Gi / 140Gi |

- `postgresql.conf` is size10's: `max_connections = 300`, `shared_buffers = 35GB`, `wal_buffers = 64MB`, `wal_compression = on`, `max_wal_size = 16GB`; commit settings (`synchronous_commit`, `commit_delay`) are PostgreSQL's defaults. `pre_test` added `pg_stat_statements` and the exporter as in the other tests.
- Peak use was 24.7 cores (10 × 4 CPU) and 29.6 (10 × 8 CPU), about a third of the 80 requested, with CPU pressure stalls at 9% or less.
- Storage: one 500Gi volume holds the data and the WAL. Its claim (`manifests/ate-install/postgres/postgres.yaml`) names no storage class, so it got the cluster default, `standard-rwo`, which is pd-balanced. That is inferred from the manifest and the cluster: teardown deleted the volume before it was checked.
- Placement: Postgres on `…-9v65` in the 10 × 4 run and `…-d1sf` in the 10 × 8 run, ate-api-server on `…-4d75` in both, the runner on the third node. In the 10,000-Worker run ate-api-server was on `…-9v65`, the runner on `…-d1sf` and Postgres on `…-4d75`; all three are the same machine type.

## Result

| ate-api-server CPUs | Ceiling (cycles/s) | Bound by |
|---|---|---|
| 10 × 4 | about 5800 (5826 at its peak step, 5490–5557 past it) | ate-api-server CPU: 34 of 40 cores, each replica at 3.2–3.8 of 4, so saturated for `GOMAXPROCS` of 4; WAL write waits growing behind it (30–60 connections) |
| 10 × 8 | about 6920 (6835–6940) | Postgres's commit path: commits flat at about 173,000/s, 66–92 connections waiting on the WAL write; ate-api-server at 52 of 80 cores and Postgres at 29 of 80 |
| 10 × 8, actor update in the bind and release, repeat with WAL timing | about 7540 (7521–7567) | Postgres's WAL flush: fsyncs busy 96% of wall time at every step, about 1,290 fsyncs/s of 0.70–0.74 ms at the ceiling, about 117 commits (47 writes) per fsync |
| 10 × 8, actor update in the bind and release | about 7500 (7464–7566) | Postgres's commit path still: about 1,250 fsyncs/s at saturation and 57–70 connections waiting on the WAL write, with commits at about 150,000/s; ate-api-server at 54–55 of 80 cores, never throttled |
| 10 × 8, pd-ssd | about 6985 (6908–7010) | Postgres's commit path, as on pd-balanced: commits flat at about 175,000/s, 74–82 connections waiting on the WAL write, ate-api-server at 51–52 of 80 cores |
| 10 × 8, 10,000 Workers | about 3385 (3381–3388) | ate-api-server CPU: 71–72 of 80 cores, each replica at 6.6–7.7 of 8, unthrottled; commits at about 85,000/s, half the commit path's cap, with about 20 connections waiting on the WAL write |

Doubling ate-api-server's CPU from 40 to 80 cores raised the ceiling 1.19×. On size0 the 2 × 8 CPU run stopped at about 3670/s on the same commit path (`summary.md`); size10 moves that limit but does not remove it:

- size10 nearly doubled what the commit path sustains, from about 92,000 commits/s on size0 to about 173,000, and the cycle ceiling with it, from about 3670 to about 6920/s. Commits per cycle stayed at 25.
- At 10 × 4 CPUs ate-api-server ran out first: 34 of its 40 cores, each replica near its 4, as in the 10 CPU run on size0. Doubling to 10 × 8 raised the ceiling only 19%, to about 6920/s, where commits stopped at about 173,000/s at every step from 7000 to 12000/s while 66–92 connections waited on `LWLock: WALWrite`. ate-api-server then had 28 of its 80 cores spare and Postgres 50 of its 80.
- The limit is flush latency, not bandwidth or buffering: Postgres wrote 110–166 MiB/s with I/O pressure stalls at 2%, and size10 already sets `wal_buffers = 64MB`. A commit returns only once its WAL is flushed, and one flush runs at a time, covering every commit queued behind it, so commits/s = flushes/s × commits per flush; the disk's write latency sets flushes/s. The pd-ssd run measured that directly (fsyncs take about 94% of wall time at the ceiling) but did not raise the ceiling, so pd-ssd's fsync latency is no better than pd-balanced's here. That leaves group commit, `synchronous_commit = off` as a diagnostic, storage with much lower fsync latency, or fewer commits per cycle.
- ate-api-server's CPU per cycle was much higher than in the smaller runs: 5.7–5.8 ms at 10 × 4 and 6.9–7.1 ms at 10 × 8 below their ceilings, against 3.6–3.7 ms at 2 × 8 and similar rates. These runs change the Worker count (1000 against 200), the fleet (1M actors against at most 6000), the replica count and the fake delay at once, so they cannot say which costs it. That the cost rose from 4 to 8 CPUs per replica at the same rates suggests part of it is contention that grows with the cores.
- With no fake delay, a resume took 12 ms at 3000/s and 25 ms at 5000/s, all of it ate-api-server and Postgres. Past the ceiling it rose to about 300 ms (10 × 8) and 375 ms (10 × 4).
- The fleet of 1,000,000 actors warmed, create then resume then pause, in 3.6 minutes at 10 × 4 and 2.9 minutes at 10 × 8 (about 4650 and 5800 actors/s), with no failures.

**10,000 Workers halve the ceiling.** With everything else as in the 10 × 8 run, ten times the Workers cut the ceiling from about 6920 to about 3385 cycles/s, and moved the limit from Postgres back to ate-api-server:

- ate-api-server's CPU per cycle roughly tripled, from 6.9–7.1 ms to 16.9 ms at 1000/s and 21.1–21.4 ms at the ceiling, while Postgres's fell slightly (3.7 ms at the ceiling against 4.3) and commits per cycle stayed at 25. The extra cost is in ate-api-server alone and grows with the Worker count, as expected if each resume scans every cached Worker (`cmd/ateapi/internal/scheduling/scheduling.go`). This settles most of the open question in the bullet above: the Worker count, not the fleet or the replica count, accounts for most of ate-api-server's higher CPU per cycle at this scale.
- Unlike the size0 runs, its CPU per cycle also rises with the rate, from 16.9 ms at 1000/s to 19.7 ms at 3000/s, which suggests part of the scan's cost is contention.
- Postgres was not the limit: commits stopped at about 85,000/s, half of the 173,000/s the 10 × 8 run reached, with 19–22 connections waiting on `WALWrite` against 66–92 there, and 12.7 of its 80 cores busy.
- Below the ceiling a resume took 20–31 ms and a pause 9–14 ms, against 23–28 ms and 17–20 ms at 5000–6000/s with 1000 Workers. Past it, a resume rose to about 620 ms, twice the 1000-Worker run's 300 ms, with a bucket-limited p99 of about 6.2 s.
- Warming the fleet of 1,000,000 actors took 8.7 minutes (about 1,900 actors/s), three times the 2.9 minutes with 1000 Workers, since each warm-up resume pays the same scan.
- 10,000 fake Workers registered and reported capacity within the deploy's 900 s timeout, and fake-atelet used at most 2.5 cores across the three nodes.

**Committing the actor update with the bind and release raises the ceiling 8%.** At `dee31f7f`, a resume records RESUMING in the same transaction as the worker bind, and a pause records PAUSED in the same transaction as the release, reading the actor under its row lock instead of re-reading it first. On the same 10 × 8 CPU, 1000-Worker, pd-balanced test:

| | `5af49ce9` | `dee31f7f` |
|---|---|---|
| Ceiling (cycles/s) | about 6920 | about 7500 (7464–7566) |
| Commits per cycle | 25.0 | 20.0 |
| Statements per cycle (`pg_stat_statements`) | 35.1 | 34.0 |
| Commits/s at the ceiling | about 173,000 | about 150,000 |
| Resume server mean at 5000/s | 23.0 ms | 19.6 ms |
| ate-api-server / Postgres cores at the ceiling | 52 / 29.4 | 54–55 / 32.4 |

- Each cycle lost 5 commits: the resume's separate actor read and update (2) and the pause's re-read, actor read and update (3). Of the 10 write commits per cycle, 8 remain.
- The ceiling rose 8%, short of the up to 25% that 8 write commits instead of 10 would allow if flushes covered as many writes as before. A repeat with `track_wal_io_timing = on` (`a2dc6e49`) reached about 7540/s, 0.6% from the first run, and shows why:

  | Step | fsyncs/s | Mean fsync | Flush busy (fsync + write) | Commits per fsync | Waiting on `WALWrite` | Waiting on a lock |
  |---|---|---|---|---|---|---|
  | 5000/s | 1,567 | 0.59 ms | 0.97 s/s (0.93 + 0.04) | 64 | 46 | 0.3 |
  | 6000/s | 1,390 | 0.66 ms | 0.96 s/s | 86 | 61 | 0.3 |
  | 7000/s | 1,292 | 0.71 ms | 0.97 s/s | 107 | 80 | 1.0 |
  | 8000–12000/s (ceiling) | 1,253–1,322 | 0.70–0.74 ms | 0.96–0.97 s/s | 114–119 | 78–103 | 1.0–2.1 |

  - The WAL flush runs back to back at every step, even at 5000/s, well below the ceiling: fsyncs take 93% of wall time and WAL writes 4%. Throughput is therefore fsyncs/s, about 1,250–1,300 at pd-balanced's 0.7 ms per fsync, times the commits each fsync covers, which grows with load until it levels off at about 117 at the ceiling.
  - Each fsync now covers about 47 write commits (117 × 8/20) against about 54 before (135 × 10/25). The flush sustains about 60,000 write commits/s instead of about 69,000, so the ceiling gained the 25% of fewer writes per cycle less the 13% of fewer writes per flush: 1.25 × 0.87 ≈ 1.09. Removing write commits shrinks the batch each flush carries; why, this data does not show. A flush covers only the write commits queued while the previous one ran, and each cycle now spends less of its time in a write commit, which would leave fewer queued at once.
  - Row locks are not the cause: 1–2 connections wait on a lock at the ceiling and 0.3 below it, so the longer bind and release transactions do not queue on the Worker's or the Actor's row.
- Below the ceiling each call is faster, with fewer round trips: a resume took 19.6 ms at 5000/s against 23.0, and 32.5 ms at 7000/s, where the old code was already saturating at 91 ms.

**pd-ssd does not lift the commit cap.** The same 10 × 8 CPU, 1000-Worker test with the Postgres volume on pd-ssd instead of pd-balanced reached about 6985 cycles/s against 6920, 1% higher, with commits flat at 174,000–175,000/s against 172,000–174,000. Everything else matched: ate-api-server at 51–52 of its 80 cores, Postgres at 29.5, 74–82 connections waiting on `WALWrite`, a resume at about 297 ms past the ceiling, and 25 commits per cycle.

The WAL counters, the first measured here, show why:

| Step | fsyncs/s | Mean fsync | Commits per fsync | WAL written |
|---|---|---|---|---|
| 5000/s | 1,590 | 0.59 ms | 79 | 65 MiB/s |
| 6000/s | 1,385 | 0.67 ms | 109 | 77 MiB/s |
| 7000/s (ceiling) | 1,248 | 0.75 ms | 138 | 62 MiB/s |
| 8000/s | 1,259 | 0.74 ms | 139 | 46 MiB/s |
| 9000/s | 1,299 | 0.73 ms | 134 | 45 MiB/s |

- At the ceiling, fsyncs/s × mean fsync time is 0.93–0.94 s per second: WAL fsyncs, which run one at a time, take about 94% of wall time. Every commit waits for one, and each covers about 135 commits, so 1,250–1,300 fsyncs/s × about 135 is the about 175,000 commits/s cap. WAL write time was small beside fsync time (60 s against 1,563 s over the run).
- pd-ssd's fsync takes 0.59–0.75 ms. That the ceiling did not move suggests pd-balanced's is about the same for these small sequential writes, but the pd-balanced run has no WAL counters, so this is inferred, not measured.
- IOPS is not the limit either: about 1,300 fsyncs/s plus as many writes is far below what a 500Gi volume of either type is provisioned for.
- Commits per fsync grow with load, from 79 to about 138, but stop growing at the ceiling, so batching alone does not raise it.
- The sampler, a `kubectl exec psql` loop, missed the 10000–12000/s steps under load; the three saturated steps it covered agree.

**Memory is not a limit.** In every run ate-api-server peaked at 372 MiB per replica of its 10Gi and Postgres at an 18 GiB working set of its 140Gi, with no memory pressure stalls, and Postgres served every block from `shared_buffers`; see Memory.

## Per step

Columns as in `summary.md`. Server p50 / p90 / p95 / p99 are bucket-limited: buckets start at 0.1 ms and double (… 6.4, 12.8, 25.6, 51.2 ms …), so each places a value within a factor of two. Past each ceiling the mean resume time is far above the p50: most resumes stay fast and a tail of slow ones, seconds at p95 and p99, sets the mean.

**10 × 4 CPU, 1M actors, size10 (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all 10) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 3000 | 3000.0 | 12.4 / 12.4 | 9.8 / 12.5 / 12.8 / 66.9 | 9.3 | 17.49 | 1.72–1.78 | 9.31 | 75,370 | 36.9 | 18.3 |
| 4000 | 4000.0 | 16.0 / 16.0 | 10.2 / 19.3 / 25.1 / 177 | 11.7 | 23.00 | 2.16–2.37 | 14.97 | 100,147 | 54.7 | 33.2 |
| 5000 | 4993.2 | 25.2 / 25.1 | 12.6 / 24.8 / 73.2 / 368 | 18.9 | 28.65 | 2.68–2.92 | 20.23 | 125,039 | 59.7 | 29.9 |
| 6000 | 5825.8 | 122.5 / 122.2 | 20.7 / 202 / 722 / 2703 | 106.0 | 33.76 | 3.32–3.46 | 24.68 | 145,803 | 80.3 | 41.3 |
| 7000 | 5515.0 | 374.8 / 374.4 | 21.8 / 1649 / 3132 / 5828 | 344.9 | 34.18 | 3.34–3.73 | 22.87 | 138,320 | 83.9 | 46.0 |
| 8000 | 5490.4 | 379.5 / 379.1 | 21.1 / 805 / 4326 / 6108 | 347.5 | 33.79 | 3.31–3.70 | 22.65 | 137,511 | 96.7 | 59.2 |
| 9000 | 5538.9 | 377.7 / 377.4 | 20.5 / 1659 / 4057 / 6054 | 343.0 | 34.00 | 3.16–3.78 | 22.86 | 138,779 | 63.0 | 32.2 |
| 10000 | 5557.4 | 376.6 / 376.2 | 20.9 / 1649 / 3577 / 5958 | 345.0 | 34.03 | 3.19–3.76 | 23.03 | 139,267 | 73.8 | 36.9 |

5000/s held, with latency starting to rise; 6000/s reached 5826/s, and from 7000/s throughput settled lower, at 5490–5557/s, with latency at about 377 ms: 4000 users ÷ the cycle time equals the achieved rate. At the saturated steps the replicas used 3.2–3.8 of their 4 cores each and were throttled in under 0.3% of periods, as `GOMAXPROCS` of 4 keeps them under quota. Postgres's commits followed throughput, down from 146,000/s at the peak step to about 138,000/s. The runner used 1.5–2.5 cores and about 410 MiB.

**10 × 8 CPU, 1M actors, size10 (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all 10) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 4999.0 | 23.1 / 23.0 | 11.8 / 25.0 / 48.4 / 355 | 16.8 | 34.56 | 3.31–3.54 | 18.56 | 125,224 | 68.8 | 41.4 |
| 6000 | 5950.2 | 28.1 / 27.9 | 18.6 / 30.3 / 56.6 / 497 | 20.0 | 41.04 | 3.80–4.22 | 24.72 | 148,845 | 96.6 | 56.9 |
| 7000 | 6918.7 | 91.0 / 90.8 | 33.1 / 295 / 366 / 613 | 77.4 | 49.30 | 4.69–5.07 | 29.59 | 173,151 | 132.0 | 91.7 |
| 8000 | 6909.9 | 300.6 / 300.4 | 65.8 / 1136 / 1536 / 2836 | 274.2 | 52.97 | 5.17–5.55 | 29.43 | 172,984 | 119.8 | 78.9 |
| 9000 | 6916.4 | 301.6 / 301.5 | 21.5 / 815 / 2462 / 3122 | 276.5 | 51.94 | 4.86–5.64 | 29.37 | 172,840 | 116.7 | 71.1 |
| 10000 | 6906.5 | 301.0 / 300.9 | 21.3 / 683 / 2462 / 3125 | 276.3 | 51.75 | 4.84–5.65 | 29.45 | 172,737 | 112.7 | 68.3 |
| 11000 | 6835.2 | 305.0 / 304.8 | 24.5 / 1476 / 2462 / 3122 | 278.9 | 51.14 | 4.43–5.56 | 29.35 | 172,386 | 128.1 | 82.2 |
| 12000 | 6939.7 | 301.2 / 301.1 | 21.7 / 1644 / 2464 / 3119 | 274.0 | 52.16 | 4.83–5.67 | 29.55 | 173,655 | 107.7 | 66.4 |

6000/s held at 28 ms; 7000/s reached 6919/s, and every step from 8000 to 12000/s stayed at 6835–6940/s with latency at about 301 ms. Unlike the 10 × 4 run, throughput did not fall past the ceiling. Commits held at 172,000–174,000/s from 7000/s on while ate-api-server stayed at 51–53 of its 80 cores, never throttled: Postgres's commit path, not ate-api-server, sets this ceiling. The runner used 2.4–3.0 cores and about 410 MiB.

**10 × 8 CPU, 1M actors, size10, actor update in the bind and release, `dee31f7f` (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all 10) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 4991 | 19.6 / 19.6 | 10.1 / 17.8 / 37.5 / 334 | 13.0 | 32.64 | 3.24–3.32 | 18.76 | 100,240 | 63.2 | 33.8 |
| 6000 | 5959 | 24.5 / 24.4 | 11.4 / 24.9 / 47.1 / 470 | 15.8 | 39.19 | 3.66–3.99 | 23.75 | 119,323 | 85.8 | 56.3 |
| 7000 | 6907 | 32.7 / 32.5 | 18.7 / 39.9 / 113 / 582 | 20.7 | 45.91 | 4.57–4.62 | 29.27 | 138,096 | 96.9 | 61.1 |
| 8000 | 7499 | 290.8 / 290.5 | 255 / 715 / 775 / 995 | 236.4 | 54.65 | 5.36–5.57 | 32.38 | 149,765 | 103.7 | 58.3 |
| 9000 | 7566 | 289.8 / 289.6 | 122 / 787 / 1476 / 2864 | 235.5 | 54.81 | 5.38–5.72 | 32.70 | 151,398 | 106.9 | 57.7 |
| 10000 | 7464 | 295.1 / 294.9 | 48.0 / 899 / 2335 / 3095 | 240.9 | 53.92 | 5.34–5.74 | 32.21 | 149,503 | 117.3 | 68.6 |
| 11000 | 7507 | 294.5 / 294.1 | 141 / 824 / 1429 / 2775 | 238.8 | 54.38 | 5.37–5.73 | 30.46 | 149,822 | 108.9 | 57.2 |
| 12000 | 7512 | 293.6 / 293.2 | 128 / 843 / 1640 / 2949 | 238.0 | 53.96 | 4.98–5.74 | 32.58 | 150,484 | 116.0 | 69.5 |

7000/s held at 33 ms, where the run at `5af49ce9` was at its ceiling; from 8000/s throughput stayed at 7464–7566/s with latency at about 292 ms, never throttled. WAL fsyncs, from the sampler, were 1,606/s at 5000/s, 1,395 at 6000, 1,279 at 7000 and 1,249 at 8000, covering 62, 85, 108 and 121 commits each; the sampler missed the later steps.

**10 × 8 CPU, 1M actors, size10 on pd-ssd (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all 10) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5000.0 | 17.4 / 17.1 | 11.1 / 24.0 / 39.9 / 167 | 13.3 | 32.90 | 3.15–3.37 | 19.74 | 125,305 | 66.1 | 36.3 |
| 6000 | 5999.0 | 28.9 / 28.9 | 19.0 / 40.6 / 145 / 355 | 23.4 | 40.71 | 4.01–4.11 | 25.23 | 150,426 | 112.8 | 80.7 |
| 7000 | 6908.0 | 150.6 / 150.5 | 24.6 / 553 / 780 / 1466 | 135.6 | 49.43 | 4.88–5.12 | 29.39 | 172,868 | 129.8 | 81.0 |
| 8000 | 6973.0 | 296.4 / 296.3 | 117 / 716 / 1718 / 2965 | 271.4 | 51.96 | 4.76–5.50 | 29.48 | 175,041 | 127.2 | 74.1 |
| 9000 | 7010.0 | 296.1 / 296.0 | 23.9 / 735 / 2454 / 3112 | 273.1 | 51.56 | 4.81–5.57 | 29.47 | 174,659 | 126.4 | 76.6 |
| 10000 | 6987.0 | 298.8 / 298.7 | 21.4 / 1642 / 2460 / 3113 | 272.4 | 50.89 | 4.68–5.59 | 29.48 | 175,107 | 130.3 | 79.2 |
| 11000 | 6971.0 | 298.1 / 298.0 | 22.0 / 732 / 2455 / 3112 | 274.4 | 51.68 | 5.08–5.56 | 29.53 | 173,925 | 120.2 | 82.1 |
| 12000 | 6988.0 | 297.5 / 297.4 | 22.4 / 654 / 2455 / 3112 | 273.6 | 51.72 | 5.08–5.58 | 29.50 | 174,962 | 123.0 | 77.0 |

The same shape as on pd-balanced: 6000/s held at 29 ms, and from 7000/s throughput stayed at 6908–7010/s with latency at about 297 ms, never throttled. Below the ceiling it was slightly faster than pd-balanced (17 against 23 ms at 5000/s), but not at the ceiling.

**10 × 8 CPU, 1M actors, 10,000 Workers, size10 (4000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all 10) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 1000 | 1000.0 | 21.3 / 20.5 | 20.4 / 35.3 / 43.4 / 49.8 | 9.5 | 16.86 | 1.60–1.71 | 2.81 | 25,233 | 12.6 | 6.5 |
| 2000 | 2000.0 | 23.1 / 22.6 | 20.5 / 36.4 / 44.3 / 50.7 | 10.2 | 38.64 | 3.83–3.89 | 6.22 | 50,169 | 20.3 | 7.9 |
| 3000 | 2999.9 | 31.7 / 31.2 | 26.8 / 47.6 / 50.1 / 226 | 14.2 | 59.17 | 5.88–5.94 | 10.61 | 75,191 | 38.5 | 18.0 |
| 4000 | 3387.1 | 617.0 / 614.9 | 48.0 / 1352 / 4888 / 6221 | 547.2 | 71.40 | 6.63–7.23 | 12.72 | 84,856 | 46.7 | 22.2 |
| 5000 | 3387.7 | 623.7 / 621.8 | 45.1 / 818 / 4912 / 6225 | 555.6 | 71.92 | 6.74–7.70 | 12.69 | 84,910 | 41.5 | 21.6 |
| 6000 | 3381.2 | 627.2 / 625.3 | 45.6 / 3286 / 4920 / 6227 | 553.6 | 71.30 | 6.67–7.70 | 12.70 | 84,577 | 41.8 | 19.2 |
| 7000 | 3383.2 | 625.2 / 623.3 | 44.8 / 3279 / 4917 / 6226 | 554.2 | 72.37 | 7.14–7.71 | 12.59 | 84,753 | 44.3 | 19.7 |

3000/s held, with latency starting to rise; from 4000/s throughput stayed at 3381–3388/s with latency at about 620 ms, and 4000 users ÷ the cycle time equals the achieved rate. ate-api-server ran at 71–72 of its 80 cores at every saturated step, throttled in about 0.1% of periods, as `GOMAXPROCS` of 8 keeps each replica under quota; its 90% of cores, not throttling, is the saturation signal. The load split evenly: at 3000/s the replicas used 5.88–5.94 cores each. Open Postgres connections were 229–233.

## Cost per cycle

| | ateapi CPU per cycle | Postgres CPU per cycle | Commits per cycle |
|---|---|---|---|
| 10 × 4 CPU, size10, 3000/s | 5.83 ms | 3.10 ms | 25.1 |
| 10 × 4 CPU, size10, 5000/s | 5.74 ms | 4.05 ms | 25.0 |
| 10 × 4 CPU, size10, 5826/s | 5.80 ms | 4.24 ms | 25.0 |
| 10 × 8 CPU, size10, 5000/s | 6.91 ms | 3.71 ms | 25.0 |
| 10 × 8 CPU, size10, 6000/s | 6.90 ms | 4.16 ms | 25.0 |
| 10 × 8 CPU, size10, 6919/s | 7.13 ms | 4.28 ms | 25.0 |
| 10 × 8 CPU, `dee31f7f`, 5000/s | 6.54 ms | 3.76 ms | 20.1 |
| 10 × 8 CPU, `dee31f7f`, 6907/s | 6.65 ms | 4.24 ms | 20.0 |
| 10 × 8 CPU, `dee31f7f`, 7499/s | 7.29 ms | 4.32 ms | 20.0 |
| 10 × 8 CPU, pd-ssd, 5000/s | 6.58 ms | 3.95 ms | 25.1 |
| 10 × 8 CPU, pd-ssd, 6000/s | 6.79 ms | 4.20 ms | 25.1 |
| 10 × 8 CPU, pd-ssd, 6908/s | 7.16 ms | 4.25 ms | 25.0 |
| 10 × 8 CPU, 10,000 Workers, 1000/s | 16.86 ms | 2.81 ms | 25.2 |
| 10 × 8 CPU, 10,000 Workers, 2000/s | 19.32 ms | 3.11 ms | 25.1 |
| 10 × 8 CPU, 10,000 Workers, 3000/s | 19.72 ms | 3.54 ms | 25.1 |
| 10 × 8 CPU, 10,000 Workers, 3387/s | 21.08 ms | 3.75 ms | 25.1 |

- Every cycle costs 25 Postgres commits, as on size0, and 20 at `dee31f7f`.
- ate-api-server's CPU per cycle is flat across rates within each 1000-Worker run, about 5.8 ms at 10 × 4 and 6.9–7.1 ms at 10 × 8, but well above the size0 runs' 3.6–3.7 ms at 2 × 8; see Result. Postgres's rises with the rate, from 3.1 ms at 3000/s to 4.3 ms at about 6900/s.
- With 10,000 Workers ate-api-server's CPU per cycle is about three times the 1000-Worker run's and rises with the rate, from 16.9 to 21.1 ms; Postgres's is the same or lower at the same rates.

## Memory

Peak memory over each step's window, from cAdvisor and postgres-exporter; per step in each run's `steps.jsonl`. Limits: ate-api-server 10Gi per replica, Postgres 140Gi.

| Run | ate-api-server per replica, peak: steps that held / saturated steps | Postgres working set, lowest → highest step | Postgres RSS | Buffer cache hit rate | Blocks read/s |
|---|---|---|---|---|---|
| 10 × 4 CPU | 72 / 282 MiB | 4.9 → 13.2 GiB | 663 MiB | 100% | 0 |
| 10 × 8 CPU | 85 / 281 MiB | 5.4 → 17.0 GiB | 660 MiB | 100% | 0 |
| 10 × 8 CPU, `dee31f7f` | 81 / 268 MiB | 5.5 → 16.2 GiB | 670 MiB | 100% | 0 |
| 10 × 8 CPU, `a2dc6e49` | 85 / 256 MiB | 5.5 → 17.7 GiB | 659 MiB | 100% | 0 |
| 10 × 8 CPU, pd-ssd | 168 / 272 MiB | 5.5 → 14.5 GiB | 665 MiB | 100% | 0 |
| 10 × 8 CPU, 10,000 Workers | 142 / 372 MiB | 4.4 → 8.9 GiB | 666 MiB | 100% | 0 |

- Memory limited nothing: ate-api-server used at most 372 MiB of its 10Gi and Postgres at most an 18 GiB working set of its 140Gi, with no memory pressure stalls and no OOM kills.
- Postgres served every block from `shared_buffers` (35GB): a 100% hit rate and no disk reads in any run. Its working set grew through each run as shared buffers and WAL pages filled, while its RSS stayed at about 660 MiB; it grew the same way while throughput stayed flat, so it tracks time, not demand.
- ate-api-server's memory jumps once a step saturates, from about 85 to 280 MiB per replica in the 10 × 8 run, as calls queue in the server. With 10,000 Workers it peaked highest, at 372 MiB, holding ten times the Workers in each replica's cache.
- The 10Gi per replica is far more than needed; 1Gi would still be about 2.75 times the highest peak.

## Confidence and limits

**Directional**

- The ceilings, about 5800/s at 10 × 4 and 6920/s at 10 × 8 CPUs: one run each. The 10 × 8 cause, Postgres's commit path, is clear from commits flat at about 173,000/s across six steps with CPU, disk bandwidth, I/O stalls and memory below saturation; the 10 × 4 one, ate-api-server's CPU, rests on replicas at 80–95% of their cores, with the WAL queue already growing.
- ate-api-server's higher CPU per cycle, 5.8–7.1 ms against 3.6–3.7 on size0: measured, and the 10,000-Worker run, which changes only the Worker count, shows that count drives it: ten times the Workers tripled the cost to 17–21 ms. That fits each resume scanning every cached Worker (`cmd/ateapi/internal/scheduling/scheduling.go`), though without a profile the scan is inferred, not observed. How much of the gap to size0 the fleet, replica count and fake delay still account for is open.
- The actor update in the bind and release, about 7500 and 7540 against 6920/s: two runs of the change, 0.6% apart, against one of the old code, and the 5 fewer commits per cycle are exact. The WAL flush's busy share (96–97%) and its fsync latency are measured; why each flush carries fewer write commits is inferred.
- pd-ssd against pd-balanced, about 6985 against 6920/s: one run each, and the 1% gap is within the ±1% run-to-run variation seen at 4 CPUs, so the two are the same. The WAL fsync share of wall time (about 94%) and the fsync latency (0.6–0.75 ms) are measured for pd-ssd only; pd-balanced's latency is inferred from its identical ceiling.
- The 10,000-Worker ceiling, about 3385/s: one run, but four saturated steps within 0.2% of each other, with ate-api-server at 90% of its cores and Postgres's commit path at half its cap.
- pgxpool's wait for a free connection is not measured: in the 10 × 8 run 108–132 of the 200 pool connections were active on average, so brief waits in bursts cannot be ruled out.

**Scope**

- Isolate mode with no fake delay, so latencies are ate-api-server and Postgres alone; no Worker churn, no router traffic, and pause rather than suspend.
- size10 Postgres (80 CPUs, 140Gi, `wal_buffers = 64MB`, default commit settings) on a pd-balanced volume, ten ate-api-server replicas at pool 20, c3-highcpu-88 nodes.
- Fake Workers are rows in ate-api-server, not pods, and report capacity but no churn: the 10,000-Worker run measures what the Worker count costs on the resume path, not the cost of Workers joining and leaving.
- No repeat run.

**To make it conclusive**

1. The 10 × 8 CPU test with Postgres group commit (`commit_delay`, `commit_siblings`), and once with `synchronous_commit = off` (benchmark only: it gives up durability) to see the ceiling without flush waits. size10's 64MB `wal_buffers` did not lift the cap, so larger buffers are not worth a run.
2. Lower-latency storage: pd-ssd is done and did not move the ceiling (see Result). To confirm that pd-balanced's fsync latency is the same, rerun the pd-balanced test with `track_wal_io_timing = on` and the WAL sampler. Hyperdisk or Local SSD would only help if their fsync latency is well under pd-ssd's 0.6–0.75 ms; measure that with a small fsync benchmark before another full run.
3. Fewer write commits per cycle in ate-api-server; read-only commits do not wait for the WAL flush (`postgres-commits-per-cycle.md`). Committing the actor update with the bind and release is done (`dee31f7f`, see Result): 25 commits to 20, 10 writes to 8, 8% more throughput. The 4 lease writes per cycle are next, but replacing the lease changes how every actor workflow is serialized, and removing write commits also shrinks the batch each flush carries, so they would likely gain less than their share. The WAL-timing repeat (`a2dc6e49`, see Result) found the flush busy 96–97% of the time with about 47 writes per fsync, which makes group commit (item 1, `commit_delay` with `commit_siblings`) worth a run on `dee31f7f`: a short wait before each fsync would let more commits share it.
4. A Go CPU profile of ate-api-server in a saturated step of the 10,000-Worker test, where its CPU per cycle reaches 21 ms, to confirm the Worker scan and size what an indexed lookup would save.
5. Finish separating the extra CPU per cycle: the 10 × 8 test with 200 Workers, and with a 6000-actor fleet. The 10,000-Worker run is done (see Result).
6. Repeat both runs two or three times.
