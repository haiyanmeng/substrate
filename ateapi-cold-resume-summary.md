# ate-api-server cold resume capacity, 2026-10-14 to -16

How many cold resume and pause cycles per second ate-api-server and Postgres sustain on the fake data plane, from one 2 CPU replica on size0 Postgres to nine 8 CPU replicas on size10, with fake-atelet answering each call after 150 ms. Every run here uses the `resumepaused` workload on code based on upstream main of 2026-10-14: the 2026-10-14 size10 runs at `e68233a7` on branch `fake-dataplane-test-1`, the rest at `5857fd53` or `6f6f44c1` on branch `ateapi-cold-resume-capacity`, 22 upstream commits later. The size0 runs come first, from `ateapi-cold-size0-5tests-20261015/summary.md` and `ateapi-cold-6tests-20261014/summary.md`; the size10 runs follow in size10 Postgres, from `ateapi-cold-6tests-20261014/summary.md` and `ateapi-cold-4to9x8-size10-20261015/summary.md`. Those sources keep the per-step tables of the size10 runs.

## Overview

Every ceiling below is in cycles/s, one resume plus one pause each. Ranges are a ceiling's saturated steps; a second figure is a rerun.

| Postgres | ate-api-server (replicas x CPUs/replica) | Ceiling (cycles/s) | Bound by |
|---|---|---|---|
| size0 | 1 × 2 | ~790 | ate-api-server CPU |
| size0 | 1 × 4 | ~1530 | ate-api-server CPU |
| size0 | 1 × 8 | ~2633 | ate-api-server CPU |
| size0 | 1 × 10 | ~2908 | mostly ate-api-server CPU |
| size0 | 2 × 4 | ~2560 | ate-api-server CPU |
| size0 | 2 × 8 | ~3630 | Postgres's WAL flush |
| size10 | 2 × 8 | ~3745 | Postgres's WAL flush |
| size10 | 3 × 8 | ~4960 | Postgres's WAL flush |
| size10 | 4 × 8 | ~5950 | Postgres's WAL flush |
| size10 | 5 × 8 | ~6870 | Postgres's WAL flush |
| size10 | 6 × 8 | ~7400 | Postgres's WAL flush |
| size10 | 7 × 8 | ~7500 | Postgres's WAL flush |
| size10 | 8 × 8 | ~7400 | Postgres's WAL flush |
| size10 | 9 × 8 | ~7450 | Postgres's WAL flush |

- **On size0** ate-api-server's CPU sets the ceiling up to about 2900/s at 10 CPUs; at 2 × 8 CPUs Postgres's WAL flush holds it to about 3630/s, the fsync busy 95% of the time at every step.
- **size10** lifts that cap, though not for two replicas: 2 × 8 reaches about 3745/s on size10 against 3630 on size0, as both carry about 50 commits per fsync. Replicas of 8 CPUs keep adding throughput, about 4960/s at 3 × 8, up to 6, about 7400/s, and 7, 8 and 9 replicas stay there, at about 178,000–180,000 commits/s: each WAL fsync carries more commits as replicas are added, until slower fsyncs cancel the gain.
- **Every cycle costs 24 Postgres commits.**
- **ate-api-server's CPU per cycle grows** with the cycle rate and the replica count: 2.5 ms at 2 CPUs, 3.7 ms at 2 × 8, 5.6–5.8 ms at 8 × 8 and 6.1 ms at 9 × 8.
- **The first 7 and 8 × 8 runs** fell to about 6930 and 7100/s, with Postgres spending about 55% more CPU per commit after a warm-up surge swamped their first steps; reruns the same evening had neither.
- **Against the 2026-10-14 runs** of the same workload at an earlier commit (`ateapi-cold-6tests-20261014/summary.md`), the 2 and 4 CPU ceilings are 7% lower and a cycle costs about 7% more ate-api-server CPU; whether the 22 upstream commits between them or day-to-day variation causes that is not shown.

The sections from size0 runs to Reading the saturated steps cover size0; size10 Postgres covers the rest. Confidence and limits, and Cost per cycle and memory, every run, cover both.

## size0 runs

- `ateapi_cold_resume_pause_fake_2x4cpu`, `_8cpu`, `_10cpu`, `_4cpu` and `_2cpu`, at `6f6f44c1` with the local orchestrator. The first four ran back to back on 2026-10-15, 22:26–01:38 UTC; `_2cpu` was relaunched alone on 2026-10-16, 01:42–02:14 UTC, after the first orchestrator stopped with the session that ran it. Every runner Job complete, no request failed. Run IDs and logs in `ateapi-cold-size0-5tests-20261015/summary.md`, local copies in `ateapi-cold-size0-5tests-20261015/results/<test>/`. The orchestrator reported `timeout` for `_10cpu` because the workstation suspended while it waited; the runner finished and uploaded every file.
- `ateapi_cold_resume_pause_fake_2x8cpu` with `track_wal_io_timing = on`, on 2026-10-15 at `6f6f44c1`, 20:23–20:52 UTC: complete, no request failed. Results under run_ts 1792096192, local copy in `ateapi-cold-6tests-20261014/results/repeat-20261015/2x8cpu/`, WAL samples in `ateapi-cold-6tests-20261014/results/repeat-20261015/wal-io.csv`, log in `~/bench-runs/ateapi-cold-2x8cpu-waltiming-20261015/orchestrator.log`.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool).

## Setup

- Isolate mode (fake data plane): fake-atelet on every atelet node answers each call after 150 ms and mints the actor certificate on each restore; 200 fake Workers with room for 1000 actors each, so placement never binds.
- `resumepaused`: each user owns one actor, creates it and makes it cold in the step that adds the user, then resumes and pauses it every iteration. One cycle is one resume plus one pause.
- Users and actors: 1000 for 2 CPU, 2000 for 4 CPU, 3000 for 8 CPU, 4000 for 10 CPU and 2 × 4 CPU, 6000 for 2 × 8 CPU.
- Ladders of 3-minute steps: 2 CPU 100–1500/s, 4 CPU 250–2500/s, 8 CPU 2400–2900/s in steps of 100, 10 CPU 2400–3400/s in steps of 200, 2 × 4 CPU 500–4000/s, 2 × 8 CPU 2500–5000/s in steps of 500.
- Placement: Postgres, ate-api-server and the runner each on their own node; every ate-api-server replica on one node; fake-atelet on all three.
- The test definitions are from https://github.com/agent-substrate/substrate/pull/2686

## Resources

Each test installed substrate fresh with `hack/install-ate.sh` at `size0` (the size10 tests at `size10`), then `pre_test` applied the test's `ateapi:` block.

**ate-api-server**: CPU request = limit = the test's CPUs per replica, so the pod is Guaranteed and `GOMAXPROCS` follows the limit; 4Gi per replica. The read/write pool is set through `ATE_API_POSTGRES_POOL_MAX_CONNS` in the `ate-api-server-envvars` ConfigMap, sized so every replica's pool plus its 5 owner and watch connections fits Postgres's `max_connections`:

| | 1 replica | 2 replicas, size0 | 4–6 replicas, size10 | 7 replicas, size10 | 8 replicas, size10 |
|---|---|---|---|---|---|
| Read/write pool per replica | 64 | 40 | 40 | 35 | 30 |
| Postgres connections, pools plus owner and watch | 69 | 90 | 180–270 | 280 | 280 |

**Postgres**

| | size0 | size10 |
|---|---|---|
| CPU request / limit | 2 / 16 | 80 / none |
| Memory request / limit | 1Gi / 2Gi | 140Gi / 140Gi |
| `max_connections` | 100 | 300 |
| `shared_buffers` | 128MB | 35GB |
| WAL | PostgreSQL defaults | `wal_buffers = 64MB`, `max_wal_size = 16GB`, `wal_compression = on` |
| Volume | 500Gi pd-balanced | 500Gi pd-balanced |

`pre_test` adds `shared_preload_libraries=pg_stat_statements` as a command-line flag and a postgres_exporter sidecar, which restarts Postgres once before ate-api-server rolls out; the 2 × 8 and size10 tests also pass `track_wal_io_timing = on`.

## Result

| ate-api-server CPUs | Ceiling (cycles/s) | Bound by | ateapi CPU per cycle | Postgres CPU per cycle |
|---|---|---|---|---|
| 2 | about 790 (785–799) | ate-api-server CPU: 2.00 of 2, throttled 26–30% | 2.50–2.54 ms | 2.82–2.84 ms |
| 4 | about 1530 (1517–1536) | ate-api-server CPU: 3.98 of 4, throttled 32–35% | 2.59–2.63 ms | 2.62–2.80 ms |
| 8 | about 2633 (2631–2637) | ate-api-server CPU: 7.82–7.84 of 8, throttled 13–16% | 2.97–2.98 ms | 2.81–2.82 ms |
| 10 | about 2908 (2904–2912) | Mostly ate-api-server: 9.38–9.45 of 10 cores (94%), unthrottled | 3.22–3.25 ms | 2.86–2.87 ms |
| 2 × 4 | about 2560 (2547–2574) | ate-api-server CPU: one replica at 3.91–3.96 of 4, the other at 3.67–3.79 | 2.99 ms | 2.71–2.90 ms |
| 2 × 8 | about 3630 (3626–3645) | Postgres's WAL flush: busy 94–95% at every step; ate-api-server at 13.5 of 16 cores, unthrottled | 3.70–3.72 ms | 3.04–3.06 ms |

| ate-api-server CPUs | Ceiling (cycles/s) | Gain over the previous size |
|---|---|---|
| 2 | about 790 | |
| 4 | about 1530 | 1.94× for 2× the CPU |
| 8 | about 2633 | 1.72× for 2× the CPU |
| 10 | about 2908 | 1.10× for 1.25× the CPU |
| 16 (2 × 8) | about 3630 | 1.25× for 1.6× the CPU |

**ate-api-server's CPU sets every single-replica ceiling.** Each replica sits at its CPU limit in every saturated step, and each ceiling is that limit divided by ate-api-server's CPU per cycle, within 2%: 2.00 ÷ 2.52 ms = 794/s at 2 CPU, 3.98 ÷ 2.61 ms = 1525/s at 4 CPU, 7.83 ÷ 2.97 ms = 2636/s at 8 CPU. That cost grows with the rate, 2.5 ms at 790/s to 3.2 ms at 2900/s, and accounts for the falling gain per CPU. At 10 CPUs ate-api-server reached 94% of its cores unthrottled, so part of that limit is not CPU. Postgres used 2.2–8.4 of its 16 cores, with lock waits at 0–2.5 connections.

**Replicas versus CPU.** 2 × 4 CPU reached about 2560/s against 2633/s for one 8 CPU replica, 3% less, at the same CPU per cycle: splitting the CPU does not help. One replica sat at its 4-core limit while the other had about 0.1–0.3 cores spare.

**The WAL flush, timed.** The 2 × 8 run turned on `track_wal_io_timing`, with the 15 s WAL sampler:

| Target | Resume/s | ate-api-server cores (per replica) | Postgres cores | Commits/s | fsyncs/s | Mean fsync | Flush busy | WAL write busy | Commits per fsync | `WALWrite` waits |
|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2500 | 9.5 (4.76 / 4.71) | 6.6 | 59,929 | 1,864 | 0.51 ms | 95% | 3% | 32 | 22 |
| 3000 | 3000 | 11.1 (5.54 / 5.52) | 8.4 | 72,079 | 1,830 | 0.52 ms | 95% | 3% | 40 | 23 |
| 3500 | 3501 | 12.3 (6.33 / 5.95) | 10.3 | 84,049 | 1,766 | 0.53 ms | 94% | 3% | 47 | 34 |
| 4000 | 3645 | 13.5 (6.96 / 6.54) | 11.1 | 87,576 | 1,764 | 0.54 ms | 95% | 3% | 50 | 31 |
| 4500 | 3626 | 13.5 (6.95 / 6.53) | 11.0 | 86,930 | 1,735 | 0.55 ms | 95% | 3% | 50 | 27 |
| 5000 | 3626 | 13.5 (6.93 / 6.53) | 11.1 | 87,134 | 1,737 | 0.55 ms | 95% | 3% | 50 | 24 |

- The fsync runs back to back at every step, busy 94–95% of the time even at 2500/s, while writing the WAL takes 3%. The limit is the fsync itself, not the rest of the commit path.
- Commits per fsync rise with load, 32 to 50, and stop at 50 at the ceiling. A flush covers only the commits already waiting, and two replicas keep about 40–46 connections active, so the cap is how many commits ate-api-server keeps in flight.
- ate-api-server used 13.5 of its 16 cores, unthrottled, and Postgres 11 of its 16; lock waits stayed at 1–2 connections.
- In the single-replica and 2 × 4 tests, with fsync timing off, the sampler counted about 2,000–2,300 fsyncs/s at every step, carrying 9–30 commits each at the ceilings: the flush runs back to back there too, but ate-api-server's CPU runs out first.

**Against the 2026-10-14 runs.** The same workload and test entries at `e68233a7` (`ateapi-cold-6tests-20261014/summary.md`) reached about 850, 1645, 2690 and 3850/s at 2, 4, 8 and 2 × 8 CPUs: 7%, 7%, 2% and 6% above these. At 2 and 4 CPUs ate-api-server spent about 7% less CPU per cycle then, 2.34–2.36 and 2.41–2.47 ms, with the same 24 commits and Postgres CPU per cycle within 2%. At 2 × 8 the gap is in the flush: the same 50 commits per fsync at 1,871–1,888 fsyncs/s then against 1,735–1,764 now. `6f6f44c1` carries 22 more upstream commits, among them #2620 (a paused actor is defined by its snapshots) and #2536 (snapshots stamped with SandboxRuntime), and the runs were on different days, so code and day-to-day variation are not separated.

**Latency.** Well below each ceiling a resume takes the fake's 150 ms plus 6–8 ms, and 164–200 ms just below it. Past it, latency rises until it absorbs the users: about 660 ms at 2 CPU, 690–700 ms at 4, 590–600 ms at 8, 690–720 ms at 10, 825–837 ms at 2 × 4 and 870–880 ms at 2 × 8.

## Per step

Columns as in `analyze_ladder.py`, over each step's window (30 s after it starts to 5 s before it ends). Server p50 / p90 / p95 / p99 are quantiles of ate-api-server's resume handler time, interpolated within doubling buckets; the means are precise. fsyncs/s and commits per fsync are from the WAL sampler over the same window, where it ran. The 2 × 8 run is in The WAL flush, timed above.

**2 CPU (1,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | `WALWrite` waits | fsyncs/s | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 100 | 100.0 | 156.5 / 156.7 | 154 / 195 / 200 / 204 | 155.7 | 0.36 | 0.0% | 3.58 ms | 0.21 | 2,432 | 2.9 |  | 1,007 | 2 |
| 200 | 200.0 | 156.9 / 157.1 | 154 / 195 / 200 / 204 | 156.2 | 0.67 | 0.0% | 3.33 ms | 0.41 | 4,795 | 3.8 | 1.2 | 1,729 | 3 |
| 400 | 400.0 | 157.9 / 158.1 | 154 / 195 / 200 / 204 | 157.0 | 1.18 | 0.0% | 2.95 ms | 0.81 | 9,609 | 4.8 | 2.1 | 2,203 | 4 |
| 600 | 600.0 | 163.6 / 163.4 | 154 / 195 / 200 / 204 | 163.4 | 1.62 | 0.0% | 2.69 ms | 1.36 | 14,430 | 9.3 | 4.6 | 2,173 | 7 |
| 800 | 798.2 | 196.5 / 195.6 | 160 / 243 / 539 / 763 | 188.1 | 1.83 | 11.2% | 2.29 ms | 1.94 | 19,195 | 11.8 | 6.3 | 2,136 | 9 |
| 1000 | 784.9 | 668.1 / 664.2 | 614 / 778 / 799 / 815 | 583.7 | 2.00 | 29.7% | 2.54 ms | 2.23 | 18,898 | 8.9 | 5.1 | 1,976 | 10 |
| 1500 | 799.1 | 663.1 / 660.5 | 614 / 778 / 799 / 815 | 581.8 | 2.00 | 26.3% | 2.50 ms | 2.26 | 19,226 | 9.2 | 5.4 | 2,039 | 9 |

**4 CPU (2,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | `WALWrite` waits | fsyncs/s | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 250 | 250.0 | 157.1 / 157.3 | 154 / 195 / 200 / 204 | 156.2 | 0.85 | 0.0% | 3.40 ms | 0.51 | 6,021 | 3.7 | 1.0 | 2,035 | 3 |
| 500 | 500.0 | 157.5 / 157.7 | 154 / 195 / 200 / 204 | 156.7 | 1.60 | 0.0% | 3.20 ms | 1.01 | 12,008 | 5.3 | 2.0 | 2,281 | 5 |
| 1000 | 1,000.0 | 158.6 / 158.7 | 154 / 195 / 200 / 204 | 157.6 | 2.85 | 0.0% | 2.85 ms | 2.10 | 24,086 | 10.9 | 5.6 | 2,238 | 11 |
| 1500 | 1,500.1 | 201.4 / 200.4 | 163 / 282 / 355 / 474 | 195.5 | 3.86 | 10.6% | 2.57 ms | 4.04 | 36,094 | 19.7 | 12.1 | 2,066 | 18 |
| 1750 | 1,517.3 | 698.6 / 697.2 | 614 / 778 / 799 / 815 | 607.8 | 3.98 | 35.3% | 2.63 ms | 4.24 | 36,465 | 15.5 | 8.8 | 2,034 | 18 |
| 2000 | 1,530.5 | 694.3 / 692.8 | 614 / 778 / 799 / 815 | 602.8 | 3.98 | 35.0% | 2.60 ms | 4.28 | 36,785 | 17.2 | 10.1 | 2,096 | 18 |
| 2500 | 1,536.4 | 692.8 / 691.2 | 615 / 779 / 800 / 816 | 599.3 | 3.98 | 31.8% | 2.59 ms | 4.02 | 36,840 | 18.6 | 10.3 | 1,991 | 19 |

**8 CPU (3,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | `WALWrite` waits | fsyncs/s | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 2400 | 2,400.1 | 164.4 / 164.4 | 155 / 197 / 202 / 333 | 161.7 | 7.15 | 0.2% | 2.98 ms | 6.16 | 57,522 | 25.4 | 17.7 | 2,159 | 27 |
| 2500 | 2,501.1 | 167.6 / 167.5 | 156 / 199 / 205 / 374 | 164.2 | 7.40 | 0.2% | 2.96 ms | 6.58 | 59,987 | 22.4 | 16.1 | 2,150 | 28 |
| 2600 | 2,600.1 | 172.0 / 171.8 | 158 / 203 / 292 / 386 | 168.2 | 7.56 | 2.4% | 2.91 ms | 7.02 | 62,450 | 25.0 | 13.2 | 2,102 | 30 |
| 2700 | 2,631.2 | 591.6 / 591.1 | 615 / 780 / 800 / 817 | 519.2 | 7.84 | 16.3% | 2.98 ms | 7.40 | 63,161 | 27.1 | 16.2 | 2,119 | 30 |
| 2800 | 2,631.7 | 600.5 / 600.0 | 615 / 780 / 801 / 817 | 526.1 | 7.84 | 14.8% | 2.98 ms | 7.41 | 63,267 | 23.6 | 16.4 | 2,154 | 29 |
| 2900 | 2,637.1 | 601.0 / 600.5 | 616 / 781 / 802 / 818 | 525.8 | 7.82 | 12.7% | 2.97 ms | 7.42 | 63,285 | 26.7 | 14.7 | 2,125 | 30 |

**10 CPU (4,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | `WALWrite` waits | fsyncs/s | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 2400 | 2,400.0 | 164.4 / 164.4 | 155 / 197 / 202 / 356 | 161.4 | 7.80 | 0.0% | 3.25 ms | 6.14 | 57,549 | 22.4 | 15.3 | 1,961 | 29 |
| 2600 | 2,600.1 | 163.7 / 163.7 | 155 / 197 / 202 / 341 | 161.2 | 7.80 | 0.1% | 3.00 ms | 6.87 | 62,366 | 25.2 | 15.6 | not sampled | |
| 2800 | 2,800.0 | 168.1 / 168.1 | 156 / 199 / 204 / 376 | 164.7 | 8.79 | 0.0% | 3.14 ms | 7.63 | 67,227 | 34.1 | 19.9 | not sampled | |
| 3000 | 2,907.3 | 689.4 / 689.0 | 619 / 787 / 808 / 1,271 | 608.0 | 9.45 | 0.0% | 3.25 ms | 8.35 | 69,820 | 28.2 | 19.1 | not sampled | |
| 3200 | 2,912.1 | 717.2 / 716.7 | 619 / 787 / 808 / 1,295 | 630.9 | 9.39 | 0.0% | 3.22 ms | 8.32 | 69,952 | 30.4 | 14.2 | not sampled | |
| 3400 | 2,904.2 | 719.5 / 718.7 | 621 / 790 / 811 / 1,365 | 631.9 | 9.38 | 0.1% | 3.23 ms | 8.34 | 69,619 | 32.0 | 18.8 | not sampled | |

**2 × 4 CPU (4,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (per replica) | Throttled | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | `WALWrite` waits | fsyncs/s | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 500 | 500.0 | 157.7 / 157.8 | 154 / 195 / 200 / 204 | 156.7 | 1.89 (0.95 / 0.94) | 0.0% | 3.77 ms | 1.02 | 12,074 | 6.1 | 2.6 | 2,301 | 5 |
| 1000 | 1,000.0 | 158.0 / 158.2 | 154 / 195 / 200 / 204 | 157.1 | 3.52 (1.79 / 1.73) | 0.0% | 3.52 ms | 2.11 | 24,160 | 7.4 | 3.4 | 2,292 | 11 |
| 2000 | 1,999.9 | 162.1 / 162.0 | 154 / 196 / 201 / 279 | 160.2 | 6.30 (3.17 / 3.12) | 0.0% | 3.15 ms | 4.97 | 48,107 | 19.6 | 11.3 | 2,208 | 22 |
| 2500 | 2,499.9 | 178.5 / 178.2 | 156 / 200 / 216 / 375 | 175.4 | 7.32 (3.70 / 3.62) | 1.9% | 2.93 ms | 7.01 | 60,016 | 25.7 | 13.6 | 2,155 | 28 |
| 3000 | 2,573.7 | 825.7 / 825.0 | 819 / 1,477 / 1,560 / 1,625 | 717.9 | 7.70 (3.91 / 3.79) | 17.8% | 2.99 ms | 6.98 | 61,844 | 27.0 | 17.6 | 2,143 | 29 |
| 3500 | 2,564.8 | 831.4 / 830.7 | 820 / 1,481 / 1,564 / 1,630 | 722.8 | 7.66 (3.95 / 3.71) | 16.5% | 2.99 ms | 7.44 | 61,616 | 24.0 | 11.9 | 2,175 | 28 |
| 4000 | 2,546.6 | 837.8 / 837.1 | 819 / 1,485 / 1,568 / 1,634 | 729.2 | 7.62 (3.96 / 3.67) | 14.8% | 2.99 ms | 7.37 | 61,207 | 27.2 | 14.8 | 2,146 | 29 |

## Reading the saturated steps

In every step past the ceiling, cycles/s equals users ÷ cycle time (2 CPU at 1500/s: 1000 ÷ (0.663 + 0.582 s) = 803/s, measured 799): every user is busy, and latency rises until it absorbs the extra demand. That is what a closed-loop load shows when the server is the bottleneck. Adding users would raise latency, not throughput, and the saturated steps' latency is set by the user count. At 8 CPUs throttling stays at 13–16% even at saturation, because `GOMAXPROCS` of 8 keeps ate-api-server from bursting past its quota; its 7.83 of 8 cores, not throttling, is the saturation signal there.

## size10 Postgres

size10 (`--cluster-size=size10`) gives Postgres 80 CPUs requested with no CPU limit, 140Gi, `shared_buffers = 35GB`, `max_connections = 300` and size10's WAL settings; commit settings are PostgreSQL's defaults. Data and WAL share one 500Gi pd-balanced volume. Postgres, ate-api-server and the runner each had their own c3-highcpu-88 node, and every ate-api-server replica shared the ateapi node.

### 2 × 8 and 3 × 8 CPUs, 2026-10-14

From `ateapi-cold-6tests-20261014/summary.md`; local copies in `ateapi-cold-6tests-20261014/results/2x8cpu_size10/` and `results/3x8cpu_size10/`, and the WAL samples in its `results/wal-io.csv`. The figures below were re-measured with `analyze_ladder.py`, which reproduced that summary's rates and added memory and per-replica CPU.

- **Tests:** `ateapi_cold_resume_pause_fake_2x8cpu_size10` and `_3x8cpu_size10`, at `e68233a7` on branch `fake-dataplane-test-1`, 15:54–16:12 and 16:24–16:47 UTC (run_ts 1791993251 and 1791995026): both complete, no request failed.
- **Load:** 6000 users and actors at 2 × 8 and 8000 at 3 × 8; ladders of 3-minute steps of 500, 2500–5000/s and 3000–6500/s; runners of 8 and 12 CPUs. Pools of 40 per replica.
- **WAL timing:** `track_wal_io_timing = on`, with the 15 s sampler.

| ate-api-server | Ceiling | ate-api-server cores used | ateapi CPU per cycle | Postgres cores | Commits/s | fsyncs/s | Mean fsync | Flush busy | Commits per fsync | Lock waits |
|---|---|---|---|---|---|---|---|---|---|---|
| 2 × 8 | about 3745 (3737–3757) | 12.9–13.5 of 16 (80–84%) | 3.44–3.58 ms | 11.6–11.7 | about 90,000 | 1,785–1,835 | 0.51–0.53 ms | 94–95% | 49–50 | 1–2 |
| 3 × 8 | about 4960 (4931–4979) | 19.4–19.9 of 24 (81–83%) | 3.90–4.01 ms | 20.4–20.6 | about 119,000 | 1,793–1,814 | 0.52 ms | 94–95% | 66 | 2–3 |

- **The WAL flush sets both ceilings.** It runs back to back at every step, busy 93–95% of the time from the lowest step, at about 1,800–1,970 fsyncs/s of 0.48–0.53 ms. Commits per fsync rise with load and stop at 50 at 2 × 8 and 66 at 3 × 8, as a third replica keeps more connections active: 3 × 8 still met 5000/s, at a 271 ms mean, and levelled off at about 119,000 commits/s.
- **size10 does not help two replicas.** 2 × 8 on size10, about 3745/s, is about 3% from 2 × 8 on size0 at `6f6f44c1` (about 3630) and from 2 × 8 on size0 the same day (about 3850, `ateapi-cold-6tests-20261014/summary.md`): the same 49–50 commits per fsync at about the same fsync rate. A larger Postgres raises the cap only once more replicas put more commits behind each flush.
- **ate-api-server is below its CPU limit** at both, 80–84% of its cores, none throttled; the busiest replica ran 0.1–0.5 cores above the others, 1.0 at 2 × 8's first saturated step.
- **Postgres** used 11.6 and 20.6 of its 80 cores, every block from memory, with 88 and 131 connections open and 1–3 waiting on locks.

### Replica scaling, 4 to 9 × 8 CPUs, 2026-10-15

From `ateapi-cold-4to9x8-size10-20261015/summary.md`; local copies and per-step analysis in `ateapi-cold-4to9x8-size10-20261015/results/`, and the WAL samples in its `results/wal-io.csv` and `results/rerun/wal-io.csv`.

- **Tests:** `ateapi_cold_resume_pause_fake_4x8cpu_size10` to `_8x8cpu_size10`, back to back at `5857fd53`, 13:21–17:04 UTC. Every test complete; 5 of about 59.3 million cold resumes failed.
- **Reruns:** `_7x8cpu_size10` and `_8x8cpu_size10` again at `6f6f44c1` (which adds only a summary) with the same test entries, 18:01–19:36 UTC: both complete; 1 of about 28.1 million cold resumes and 3 warm-up resumes failed.
- **9 × 8:** `_9x8cpu_size10`, added to `tests.yaml` for this run, at `6f6f44c1`, 21:11–22:18 UTC (run_ts 1792099178): complete, none of about 16.1 million cold resumes failed. 72 CPUs on the ateapi node, pools of 26, 9 × (26 + 5) = 279 connections. The workstation suspended from 21:34 to 22:17 UTC, so the WAL sampler, which runs there, has no samples from its 7500/s step on; the runner and Prometheus, in the cluster, ran throughout.
- **Load:** 10,000 users and actors at 4 × 8, rising by 2000 per replica to 20,000 at 9 × 8; ladders of 3-minute steps of 500 from 4000–5000/s to 8000–11000/s; runners of 16 CPUs.
- **WAL timing:** `track_wal_io_timing = on`, with WAL, commit and wait counters sampled every 15 s.

| ate-api-server | Ceiling | ate-api-server cores used | ateapi CPU per cycle | Postgres cores | Commits/s | Mean fsync | Commits per fsync | Lock waits |
|---|---|---|---|---|---|---|---|---|
| 4 × 8 | about 5950 (5938–5988) | 27.1–27.9 of 32 (86%) | 4.56–4.66 ms | 22.1–22.3 | about 143,000 | 0.52–0.54 ms | 79–83 | 3–6 |
| 5 × 8 | about 6870 (6817–6899) | 33.4–33.9 of 40 (84%) | 4.84–4.98 ms | 27.3–27.8 | about 165,000 | 0.55–0.57 ms | 97–101 | 7–19 |
| 6 × 8 | about 7400 (7358–7434) | 36.7–37.7 of 48 (77%) | 4.94–5.07 ms | 31.6–32.4 | about 178,000 | 0.66–0.68 ms | 127–130 | 19–27 |
| 7 × 8, rerun | about 7500 (7419–7578) | 40.3–40.8 of 56 (72%) | 5.38–5.42 ms | 30.5–32.9 | about 180,000 | 0.70–0.72 ms | 135–140 | 22–32 |
| 8 × 8, rerun | about 7400 (7345–7446) | 41.3–43.1 of 64 (66%) | 5.61–5.80 ms | 32.2–33.4 | about 178,000 | 0.73–0.76 ms | 140–144 | 21–30 |
| 9 × 8 | about 7450 (7373–7517) | 44.8–45.9 of 72 (63%) | 6.05–6.12 ms | 32.5–33.8 | about 179,000 | not sampled | | 25–34 |
| 7 × 8, first run | about 6930 (6843–6961) | 37.9–38.6 of 56 (68%) | 5.50–5.59 ms | 45.8–46.9 | about 166,000 | 0.66–0.72 ms | 119–127 | 22–32 |
| 8 × 8, first run | about 7100 (6844–7156) | 40.8–43.2 of 64 (66%) | 5.94–6.06 ms | 44.4–47.0 | about 170,000 | 0.59–0.70 ms | 111–124 | 22–31 |

**Postgres's WAL flush sets every ceiling.** The flush is busy 92–94% of the time at every ceiling, so commits/s is fsyncs/s times the commits each fsync covers. Up to 6 × 8 more replicas pack more commits into each fsync, from 50 at 2 × 8 and 66 at 3 × 8 (2026-10-14) to 80 at 4 × 8 and 129 at 6 × 8, faster than fsyncs slow, 0.52 to 0.68 ms, and the ceiling rises. In the reruns 7 and 8 × 8 pack a few more, 135–144, but fsyncs slow to 0.70–0.76 ms, so commits stay at about 178,000–180,000/s and the ceiling at 7400–7500/s. 9 × 8 levels off at the same commits from 7500/s on; at its last sampled step, 7000/s, the flush was busy 93% at about 1,400 fsyncs/s of 0.67 ms carrying 120 commits each. A ninth replica adds connections and CPU but no commits.

**ate-api-server is below its CPU limit, and more so with each replica.** Its share of CPU in use falls from 86% at 4 × 8 to 66% at 8 × 8 and 63% at 9 × 8, no replica reaching its 8-core limit and none throttled. Its CPU per cycle rises with the replica count, 4.6 ms at 4 × 8 to 5.4 ms at 7 × 8, 5.6–5.8 ms at 8 × 8 and 6.05–6.12 ms at 9 × 8; at the same 5000/s it is 4.61, 5.06, 5.17, 5.29, 5.50 and 5.68 ms from 4 to 9 replicas, about 0.2 ms per added replica.

**The first 7 × 8 and 8 × 8 runs.** Same commit, test entries and Postgres settings as the reruns, but Postgres spent far more CPU on each commit, and commits and the ceiling fell:

| | Postgres cores | Commits/s | Postgres CPU per commit | Lock waits | First steps |
|---|---|---|---|---|---|
| 6 × 8 | 32.1 | 176,600 | 0.18 ms | 19–27 | held |
| 7 × 8, first run | 46.8 | 166,500 | 0.28 ms | 22–32 | swamped by the warm-up |
| 8 × 8, first run | 46.9 | 172,400 | 0.27 ms | 22–31 | swamped by the warm-up |
| 7 × 8, rerun | 32.2 | 180,200 | 0.18 ms | 22–32 | held |
| 8 × 8, rerun | 32.9 | 177,600 | 0.19 ms | 21–30 | held |

The extra CPU per commit, about 55%, came only in the two runs whose first steps the warm-up surge swamped, and lasted to their last step; they also began with a larger Postgres working set (see Cost per cycle and memory, every run). Lock waits were the same in the first runs and the reruns, so they do not explain it, and cost no throughput at 6 × 8 or in the reruns. What made each commit dearer is not shown; no statement- or lock-level data was collected.

**The warm-up surge.** Each user creates and warms its actor in the first step, and `--ladder-spawn-rate` equals the users, so at 7 and 8 × 8, 16,000 and 18,000 actors are made cold within seconds. In the first runs Postgres jumped to 44–64 cores with 50–77 connections waiting on locks, and throughput fell to 4422/s in 7 × 8's first step and to 2786 and 3072/s in 8 × 8's first two. The reruns, with the same spawn rate, held their first steps (5000/s, and 5017 and 5500/s) with Postgres at 19–22 cores, and so did 9 × 8 with 20,000 users (5005/s, Postgres at 20 cores). The ceilings above leave the swamped steps out.

**Failures.** The first runs had 5 cold resumes fail, 3 at 7 × 8 and 2 at 8 × 8, each `ResourceExhausted: no worker has room for the actor`; the 8 × 8 rerun had 1 more, and 3 warm-up resumes failed with `Aborted: concurrent update conflict, please retry`. 200 Workers hold 200,000 actors against at most 18,000, so this is not exhaustion. 9 × 8, with 20,000 users, had none. None fell inside a measured window, and the 8 × 8 rerun had them without the surge: a placement race at the largest user counts.

**Postgres's resources have room.** At most 47 of its 80 cores at a ceiling (33–34 in the reruns and at 9 × 8; 64 during the first 8 × 8 run's warm-up), every block from memory, at most 277 of 300 connections open.

### Next steps for size10

1. **Explain the first 7 × 8 and 8 × 8 runs.** Read `pg_stat_statements` for their saturated steps against the reruns', while Prometheus holds them, to see which statements cost more after the warm-up surge, and sample `pg_locks` during a 7 × 8 run to find what the lock waits are on.
2. **Fewer write commits per cycle.** With the flush capping commits per second, fewer per cycle is the most direct way past about 7500 cycles/s. The flush is busy 92–94% of the time, which also makes group commit (`commit_delay` with `commit_siblings`) worth a run.
3. **What sets fsync latency.** The volume's own latency from Cloud Monitoring next to Postgres's fsync time would show whether the disk varies; if it does, lower-latency storage at 7 × 8 is worth a run, with several repeats.
4. **Spread the warm-up out.** Run 7 × 8 and 8 × 8 with `--ladder-spawn-rate` of about 2000, so no run starts with the surge, and see whether the `ResourceExhausted` failures go away.
5. **Profile ate-api-server**: a Go CPU profile at 5000/s at 4 × 8 and 8 × 8, to see what each added replica costs per cycle.

## Confidence and limits

**Solid**

- ate-api-server's CPU as the limit of every single-replica size0 ceiling: each replica at its CPU limit in every saturated step, and the ceilings follow its CPU per cycle within 2%.
- Postgres's WAL flush as the limit at 2 × 8 on size0, measured directly, and on size10, busy 92–94% at every sampled ceiling.
- 24 Postgres commits per cycle at every step of every test.

**Directional**

- Each size0 ceiling and 4 to 6 × 8 on size10: one run at this code. 2 × 8 and 3 × 8 on size10: one run each on 2026-10-14, at `e68233a7`, 22 upstream commits before the rest; they are not repeated at the later code, and the size0 runs show that code or day can move a ceiling by up to 7%.
- The level ceiling past 6 × 8 on size10, about 7400–7500/s: one rerun each at 7 and 8 × 8 and one run at 9 × 8, consistent with each other and with 6 × 8. That the WAL flush sets the 9 × 8 ceiling is inferred from its commits levelling off at the same rate; its saturated steps have no WAL samples.
- That the first 7 and 8 × 8 runs are outliers: their extra Postgres CPU per commit and swamped first steps came together and did not recur, but why is not shown.
- The 7% higher ate-api-server CPU per cycle than on 2026-10-14 at 2 and 4 CPUs: consistent across both sizes, one run each; code or day is not separated.

**Not conclusive**

- Server latency quantiles: doubling histogram buckets, interpolated within one; the means are precise.
- The cause of the `ResourceExhausted` and `Aborted` failures, and their timing, inferred from the windows.

**Scope**

- Isolate mode: a fixed 150 ms per call with no tail, no Worker churn, no router traffic, and pause rather than suspend.
- size0 and size10 Postgres with their default commit settings on pd-balanced volumes; c3-highcpu-88 nodes.

**To make it conclusive**

1. Run `_2cpu` and `_4cpu` at `e68233a7` and at `6f6f44c1` back to back on one day, to tell whether the code raised ate-api-server's CPU per cycle.
2. The 2 × 8 CPU test once with `synchronous_commit = off` (benchmark only: it gives up durability) to see the ceiling without flush waits.
3. A Go CPU profile of ate-api-server at 2500–3000/s, to see where its CPU per cycle grows with the rate.
4. Repeat the size0 tests and 4 to 6 × 8 on size10, and keep the workstation awake through each series, so the orchestrator's verdicts and the WAL sampler cover every test.

## Cost per cycle and memory, every run

Every run in this summary, from each run's `steps.jsonl`. CPU per cycle is the container's cores over a step's window divided by its successful resumes per second, one resume and one pause per cycle. The ceiling columns cover the saturated steps; the 5000/s columns are the step targeting 5000/s, with a dash where the ladder has no such step or it did not hold.

**Cost per cycle**

| Run | Postgres, date | Ceiling (cycles/s) | ateapi CPU per cycle at the ceiling | Postgres CPU per cycle at the ceiling | Commits per cycle | ateapi CPU per cycle at 5000/s | Postgres CPU per cycle at 5000/s |
|---|---|---|---|---|---|---|---|
| 2 × 8 CPU, WAL timing | size0, 10-15 | 3626–3645 | 3.70–3.72 ms | 3.04–3.06 ms | 24.0 | — | — |
| 2 CPU | size0, 10-16 | 785–799 | 2.50–2.54 ms | 2.82–2.84 ms | 24.1 | — | — |
| 4 CPU | size0, 10-16 | 1517–1536 | 2.59–2.63 ms | 2.62–2.80 ms | 24.0 | — | — |
| 8 CPU | size0, 10-15 | 2631–2637 | 2.97–2.98 ms | 2.81–2.82 ms | 24.0 | — | — |
| 10 CPU | size0, 10-15 | 2904–2912 | 3.22–3.25 ms | 2.86–2.87 ms | 24.0 | — | — |
| 2 × 4 CPU | size0, 10-15 | 2547–2574 | 2.99 ms | 2.71–2.90 ms | 24.0 | — | — |
| 2 × 8 CPU | size10, 10-14 | 3737–3757 | 3.44–3.58 ms | 3.10–3.12 ms | 24.0 | — | — |
| 3 × 8 CPU | size10, 10-14 | 4931–4979 | 3.90–4.01 ms | 4.10–4.16 ms | 23.9–24.1 | 3.85 ms | 4.06 ms |
| 4 × 8 CPU | size10, 10-15 | 5938–5988 | 4.56–4.66 ms | 3.72–3.73 ms | 23.9–24.0 | 4.61 ms | 3.55 ms |
| 5 × 8 CPU | size10, 10-15 | 6817–6899 | 4.84–4.98 ms | 4.01–4.03 ms | 24.0–24.1 | 5.06 ms | 3.66 ms |
| 6 × 8 CPU | size10, 10-15 | 7358–7434 | 4.94–5.07 ms | 4.25–4.40 ms | 24.0–24.1 | 5.17 ms | 3.60 ms |
| 7 × 8 CPU, first run | size10, 10-15 | 6843–6961 | 5.50–5.59 ms | 6.67–6.77 ms | 24.0–24.1 | — | — |
| 7 × 8 CPU, rerun | size10, 10-15 | 7419–7578 | 5.38–5.42 ms | 4.11–4.35 ms | 23.9–24.1 | 5.29 ms | 3.77 ms |
| 8 × 8 CPU, first run | size10, 10-15 | 6844–7156 | 5.92–6.06 ms | 6.49–6.58 ms | 23.9–24.1 | — | — |
| 8 × 8 CPU, rerun | size10, 10-15 | 7345–7446 | 5.61–5.80 ms | 4.33–4.54 ms | 24.0–24.1 | 5.50 ms | 3.84 ms |
| 9 × 8 CPU | size10, 10-15 | 7373–7517 | 6.05–6.12 ms | 4.37–4.56 ms | 24.0–24.1 | 5.68 ms | 4.03 ms |

- **Commits per cycle** are 24 in every run.
- **ate-api-server's CPU per cycle** at the ceiling grows from 2.5 ms at 2 CPUs to 5.6–5.8 ms at 8 × 8 (5.9–6.1 ms in the first 8 × 8 run) and 6.1 ms at 9 × 8, as both the rate and the replica count rise.
- **Postgres's CPU per cycle** is 2.6–3.1 ms on size0 and 3.1–4.5 ms at the size10 ceilings, rising with the replica count; the first 7 × 8 and 8 × 8 runs, at 6.5–6.8 ms, are the outliers.

**Memory**

Peak per step's window, from cAdvisor and postgres-exporter. Steps that held met their target; saturated steps are the ceiling's. Limits: ate-api-server 4Gi per replica; Postgres 2Gi on size0, 140Gi on size10.

| Run | Postgres, date | ate-api-server per replica, peak: steps that held / saturated steps | Postgres working set, first → highest step | Postgres RSS, peak | Buffer cache hit rate, lowest step | Blocks read/s, highest step |
|---|---|---|---|---|---|---|
| 2 × 8 CPU, WAL timing | size0, 10-15 | 145 / 355 MiB | 500 MiB → 626 MiB | 235 MiB | 99.794% | 1,089 |
| 2 CPU | size0, 10-16 | 98 / 120 MiB | 247 MiB → 432 MiB | 186 MiB | 99.995% | 5 |
| 4 CPU | size0, 10-16 | 127 / 184 MiB | 285 MiB → 466 MiB | 184 MiB | 99.943% | 122 |
| 8 CPU | size0, 10-15 | 178 / 249 MiB | 434 MiB → 510 MiB | 185 MiB | 99.888% | 413 |
| 10 CPU | size0, 10-15 | 199 / 310 MiB | 442 MiB → 545 MiB | 186 MiB | 99.803% | 809 |
| 2 × 4 CPU | size0, 10-15 | 103 / 252 MiB | 408 MiB → 578 MiB | 237 MiB | 99.898% | 378 |
| 2 × 8 CPU | size10, 10-14 | 167 / 339 MiB | 1.7 GiB → 5.5 GiB | 244 MiB | 100.000% | 0 |
| 3 × 8 CPU | size10, 10-14 | 170 / 417 MiB | 2.2 GiB → 8.5 GiB | 367 MiB | 100.000% | 0 |
| 4 × 8 CPU | size10, 10-15 | 183 / 513 MiB | 2.4 GiB → 12.9 GiB | 486 MiB | 100.000% | 0 |
| 5 × 8 CPU | size10, 10-15 | 197 / 599 MiB | 2.8 GiB → 14.5 GiB | 609 MiB | 100.000% | 0 |
| 6 × 8 CPU | size10, 10-15 | 168 / 422 MiB | 3.0 GiB → 16.8 GiB | 747 MiB | 100.000% | 0 |
| 7 × 8 CPU, first run | size10, 10-15 | 157 / 383 MiB | 6.0 GiB → 19.3 GiB | 764 MiB | 100.000% | 0 |
| 7 × 8 CPU, rerun | size10, 10-15 | 152 / 416 MiB | 3.1 GiB → 19.3 GiB | 751 MiB | 100.000% | 0 |
| 8 × 8 CPU, first run | size10, 10-15 | 131 / 371 MiB | 7.2 GiB → 25.7 GiB | 791 MiB | 100.000% | 0 |
| 8 × 8 CPU, rerun | size10, 10-15 | 143 / 353 MiB | 3.2 GiB → 20.6 GiB | 746 MiB | 100.000% | 0 |
| 9 × 8 CPU | size10, 10-15 | 134 / 349 MiB | 3.2 GiB → 20.3 GiB | 776 MiB | 100.000% | 0 |

- **Memory limited nothing in any run.** ate-api-server peaked at 599 MiB of its 4Gi, and memory pressure stalls stayed at 0.01% of the time or less in ate-api-server and Postgres.
- **ate-api-server's memory follows queueing, not the rate.** Steps that held peaked at 98–199 MiB per replica; once a step saturates, calls queue and each holds its goroutine and buffers, to 120–599 MiB.
- **Postgres's working set grows with time,** as it touches more of its shared buffers and page cache: to 432–626 MiB on size0 and 5.5–26 GiB on size10. Its RSS grows with the replica count, and so the connections: 184–237 MiB on size0 and 244–791 MiB on size10, from 2 × 8 to 9 × 8.
- **size10 serves every block from memory**; size0's 128MB `shared_buffers` misses more as the rate rises, up to 1,089 blocks/s read.
- **The first 7 × 8 and 8 × 8 runs began with a larger Postgres working set,** 6.0 and 7.2 GiB at their first step against 3.0–3.2 GiB in every other run of 6 to 8 replicas.

## How results are measured

`benchmarking/automation/analyze_ladder.py` measures each step from 30 s after it starts to 5 s before it ends, 145 s of each 3-minute step, against the benchmarking Prometheus: boomer's client and server latency, cAdvisor's CPU and memory, and postgres-exporter's commits, connections and waits. A sampler on the workstation read Postgres's WAL I/O, commit and wait counters every 15 s; it missed the periods the workstation was suspended.
