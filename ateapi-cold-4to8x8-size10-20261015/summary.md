# ate-api-server replica scaling on size10 Postgres, 4 to 8 × 8 CPUs, 2026-10-15

- Tests `ateapi_cold_resume_pause_fake_4x8cpu_size10` to `_8x8cpu_size10`, run back to back with the local orchestrator on branch `ateapi-cold-resume-capacity` at `5857fd53`, 13:21–17:04 UTC. Every test complete; 5 of about 59.3 million cold resumes failed (see Failures).
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_<n>x8cpu_size10/run_date=2026-10-15/`; local copies and per-step analysis (`steps.jsonl`, from `benchmarking/automation/analyze_ladder.py`) in `results/<n>x8cpu_size10/`. Postgres's WAL, commit and wait counters, sampled every 15 s across the series, are in `results/wal-io.csv`. Orchestrator log: `~/bench-runs/ateapi-cold-4to8x8-size10-20261015/orchestrator.log`.
- The earlier series is `../ateapi-cold-2-4-8cpu-20261009/summary-size10-replicas.md`, run on 2026-10-11 and repeated on 2026-10-12.

| Test | Ladder (UTC) | run_ts | Local copy |
|---|---|---|---|
| `4x8cpu_size10` | 13:29–13:56 | 1792070954 | `results/4x8cpu_size10/` |
| `5x8cpu_size10` | 14:08–14:38 | 1792073296 | `results/5x8cpu_size10/` |
| `6x8cpu_size10` | 14:50–15:23 | 1792075811 | `results/6x8cpu_size10/` |
| `7x8cpu_size10` | 15:35–16:08 | 1792078551 | `results/7x8cpu_size10/` |
| `8x8cpu_size10` | 16:23–16:59 | 1792081385 | `results/8x8cpu_size10/` |

**Result: the ceiling rises up to 6 replicas, as before, then falls.**

- **Ceilings:** about 5950, 6870, 7400, 6930 and 7100 cycles/s from 4 to 8 replicas. Up to 6 × 8 they match the earlier series within 2%; 7 × 8 and 8 × 8 are 15% and 13% below its 8200 and 8150.
- **Postgres's WAL flush sets the cap in every test**: busy 92–94% of the time at each ceiling. ate-api-server is never at its CPU limit, and size10 Postgres has none.
- **At 7 and 8 replicas Postgres works harder for fewer commits.** Commits level off at about 166,000–172,000/s against 178,000 at 6 × 8; each fsync carries fewer of them; lock waits rise to about 30 connections; and Postgres's CPU per commit is about 55% higher than at 6 × 8 and than in the earlier 7 × 8 and 8 × 8 runs.
- **Lock waits are 2–3× the earlier series' at every size.** The workload and the code both changed since then (see What differs from the earlier series), so which one adds them is not shown.

## What differs from the earlier series

- **Workload.** `resumepaused`: each user owns one actor, creates it and makes it cold in the step that adds the user, then resumes and pauses it every iteration. The earlier series ran `resumecold`, where users took paused actors from a shared fleet. One cycle is still one resume plus one pause; it now costs 24 Postgres commits instead of 25.
- **Code.** `5857fd53` is `ateapi-cold-resume-capacity`, based on upstream main of 2026-10-14. Against the earlier series' `fake-dataplane-test` branch it brings in, among others, #2620 (a paused actor is defined by its snapshots; the PAUSED and PAUSING states are gone) and #2536 (snapshots stamped with SandboxRuntime; restores scheduled on runtime-compatible workers), both on the resume and pause path.
- **Fake data plane install.** `ate-setup --fake-data-plane` and an `isolate` block, upstream's version of the earlier branch's `fakeDataPlane` block. fake-atelet also mints the actor certificate on each restore.

The test entries are otherwise the same: users, pools, ladders and runner sizes below.

## Setup

- **Data plane:** isolate mode, fake-atelet on every atelet node answering each call after 150 ms, 200 fake Workers of 1000 actors each.
- **Postgres:** size10 (80 CPU requested, no CPU limit, 140Gi, `shared_buffers = 35GB`, `max_connections = 300`, size10's WAL settings) with `track_wal_io_timing = on`.
- **ate-api-server:** 8 / 8 CPU per replica (request = limit), 4Gi, `GOMAXPROCS` 8, every replica on one node.
- **Placement:** Postgres, ate-api-server and the runner each on their own node. Postgres was on `…-9v65` and ate-api-server on `…-4d75` for 4, 5 and 8 × 8, and the other way round for 6 and 7 × 8; the runner was on `…-d1sf` throughout. The 6 × 8 and 7 × 8 runs, which bracket the drop, had the same layout.

| | 4 × 8 | 5 × 8 | 6 × 8 | 7 × 8 | 8 × 8 |
|---|---|---|---|---|---|
| Total ate-api-server CPUs | 32 | 40 | 48 | 56 | 64 |
| Read/write pool per replica | 40 | 40 | 40 | 35 | 30 |
| Postgres connections, pools plus 5 owner and watch per replica | 180 | 225 | 270 | 280 | 280 |
| Users and actors | 10,000 | 12,000 | 14,000 | 16,000 | 18,000 |
| Ladder, 3-minute steps of 500 | 4000–8000/s | 4500–9000/s | 5000–10000/s | 5000–10000/s | 5000–10500/s |
| Runner CPU / memory | 16 / 4Gi | 16 / 5Gi | 16 / 6Gi | 16 / 6Gi | 16 / 7Gi |

## Result

| ate-api-server | Ceiling (cycles/s) | Earlier series | Change | ate-api-server cores used | ateapi CPU per cycle | Postgres cores | Commits/s | fsyncs/s | Mean fsync | Commits per fsync | Lock waits |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 4 × 8 | about 5950 (5938–5988) | about 5930 | same | 27.1–27.9 of 32 (86%) | 4.56–4.66 ms | 22.1–22.3 | about 143,000 | 1,750–1,818 | 0.52–0.54 ms | 79–83 | 3–6 |
| 5 × 8 | about 6870 (6817–6899) | about 6750 | +2% | 33.4–33.9 of 40 (84%) | 4.84–4.98 ms | 27.3–27.8 | about 165,000 | 1,630–1,697 | 0.55–0.57 ms | 97–101 | 7–19 |
| 6 × 8 | about 7400 (7358–7434) | about 7340 | +1% | 36.7–37.7 of 48 (77%) | 4.94–5.07 ms | 31.6–32.4 | about 178,000 | 1,370–1,411 | 0.66–0.68 ms | 127–130 | 19–27 |
| 7 × 8 | about 6930 (6843–6961) | about 8200 | −15% | 37.9–38.6 of 56 (68%) | 5.50–5.59 ms | 45.8–46.9 | about 166,000 | 1,293–1,402 | 0.66–0.72 ms | 119–127 | 22–32 |
| 8 × 8 | about 7100 (6844–7156) | about 8150 | −13% | 40.8–43.2 of 64 (66%) | 5.94–6.06 ms | 44.4–47.0 | about 170,000 | 1,314–1,556 | 0.59–0.70 ms | 111–124 | 22–31 |

Ceilings are the saturated steps' range; the WAL columns are from the sampler over the same windows.

**Postgres's commit path.** In every test the WAL flush runs back to back, busy 92–94% of each saturated step, so commits/s is fsyncs/s times commits per fsync. Up to 6 × 8 more replicas pack more commits into each fsync, 80 to 129, faster than fsyncs slow, 0.52 to 0.68 ms, and the ceiling rises, as in the earlier series. At 7 and 8 × 8 commits per fsync fall back, to 111–127, while fsyncs stay as slow, and commits drop to about 166,000–172,000/s. The earlier series' 7 × 8 and 8 × 8 reached about 205,000.

**Contention inside Postgres.** At the drop Postgres's CPU rises while its commits fall:

| | Postgres cores | Commits/s | Postgres CPU per commit | Lock waits |
|---|---|---|---|---|
| 6 × 8, now | 32.1 | 176,600 | 0.18 ms | 19–27 |
| 7 × 8, now | 46.8 | 166,500 | 0.28 ms | 22–32 |
| 8 × 8, now | 46.9 | 172,400 | 0.27 ms | 22–31 |
| 6 × 8, earlier | 29.2–31.8 | about 183,000 | about 0.17 ms | 6.9–10.3 |
| 7 × 8, earlier | 34.0–37.0 | about 205,000 | about 0.17 ms | not sampled |
| 8 × 8, earlier | 33.4–36.7 | about 205,000 | about 0.17 ms | 8.6 |

Each commit costs about 55% more Postgres CPU at 7 and 8 × 8 than at 6 × 8 or in the earlier runs, with no CPU limit in the way: more CPU spent per commit while more connections wait on locks is the pattern of contention on a shared row or lock. Lock waits were already 2–3× the earlier series' at 6 × 8 and below, without costing throughput there. Which statements and locks are involved, this data does not show; `pg_stat_statements` in the cluster's Prometheus, or `pg_locks` sampled during a 7 × 8 run, would.

**ate-api-server.** Its share of CPU in use falls from 86% at 4 × 8 to 66% at 8 × 8, no replica reaching its 8-core limit (busiest 7.33 at 4 × 8, 5.6 at 8 × 8), so it does not set the ceilings. Its CPU per cycle rises with the replica count, 4.6 ms at 4 × 8 to 6.0 ms at 8 × 8, about 7% above the earlier series at 7 and 8 × 8 and within 3% of it below.

## The warm-up surge

Each user creates and warms its actor in the first step, and `--ladder-spawn-rate` equals the users, so at 7 × 8 and 8 × 8, 16,000 and 18,000 actors are created and made cold within seconds. Postgres jumped to 44–64 cores with 50–77 connections waiting on locks, and throughput fell to 4422/s in 7 × 8's first step and to 2786 and 3072/s in 8 × 8's first two, with server means of 1.1–3.4 s. Those steps measure the warm-up, not the ladder; the ceilings above leave them out. At 4 to 6 × 8 the first step held its target. A spawn rate of a few thousand per second would spread the warm-up out.

## Failures

5 cold resumes failed, 3 at 7 × 8 and 2 at 8 × 8, each with `ResourceExhausted: no worker has room for the actor`. 200 fake Workers hold 200,000 actors against at most 18,000, so this is not exhaustion. None fell inside a measured window, and only the two tests with the warm-up surge had any, so they most likely came during it: a placement race under load, worth a look in ate-api-server's logs.

## Per step

Columns as in `analyze_ladder.py`, over each step's window (30 s after it starts to 5 s before it ends). Server p50 / p90 / p99 are quantiles of ate-api-server's resume handler time and are bucket-limited (doubling buckets: 204.8, 409.6, …, 6553.6 ms). fsyncs/s, mean fsync, flush busy and commits per fsync are from `results/wal-io.csv` over the same window.

**4 × 8 (10,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 4000 | 4,000 | 171 | 156 / 199 / 448 | 18.8 (4.72) | 4.69 ms | 13.2 | 96,067 | 61 | 3.7 | 38.7 | 1,798 | 0.52 ms | 93% | 53 |
| 4500 | 4,501 | 178 | 157 / 201 / 610 | 21.0 (5.29) | 4.66 ms | 15.4 | 108,130 | 54 | 2.8 | 32.3 | 1,787 | 0.52 ms | 93% | 60 |
| 5000 | 5,000 | 188 | 160 / 224 / 691 | 23.1 (5.81) | 4.61 ms | 17.7 | 119,818 | 61 | 2.7 | 40.8 | 1,764 | 0.52 ms | 93% | 68 |
| 5500 | 5,509 | 220 | 170 / 400 / 793 | 25.3 (6.39) | 4.60 ms | 20.2 | 132,252 | 63 | 8.3 | 30.5 | 1,750 | 0.53 ms | 93% | 75 |
| 6000 | 5,988 | 407 | 364 / 740 / 950 | 27.9 (7.06) | 4.66 ms | 22.3 | 143,654 | 80 | 5.9 | 42.4 | 1,750 | 0.54 ms | 94% | 83 |
| 6500 | 5,964 | 888 | 205 / 2,667 / 3,701 | 27.4 (7.33) | 4.59 ms | 22.2 | 143,167 | 74 | 5.1 | 38.4 | 1,774 | 0.53 ms | 94% | 81 |
| 7000 | 5,938 | 895 | 197 / 2,663 / 3,331 | 27.7 (7.30) | 4.66 ms | 22.1 | 142,762 | 75 | 6.4 | 40.8 | 1,788 | 0.53 ms | 94% | 80 |
| 7500 | 5,956 | 895 | 200 / 2,679 / 4,196 | 27.7 (7.31) | 4.65 ms | 22.2 | 143,152 | 87 | 6.3 | 43.7 | 1,818 | 0.52 ms | 94% | 79 |
| 8000 | 5,947 | 898 | 234 / 2,679 / 4,192 | 27.1 (7.27) | 4.56 ms | 22.2 | 142,354 | 71 | 2.7 | 40.8 | 1,787 | 0.52 ms | 93% | 79 |

**5 × 8 (12,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 4500 | 4,500 | 174 | 156 / 200 / 604 | 22.6 (4.60) | 5.02 ms | 15.9 | 108,053 | 80 | 4.2 | 48.2 | 1,799 | 0.52 ms | 93% | 60 |
| 5000 | 5,002 | 180 | 158 / 202 / 635 | 25.3 (5.09) | 5.06 ms | 18.3 | 120,168 | 78 | 5.4 | 48.2 | 1,768 | 0.52 ms | 92% | 68 |
| 5500 | 5,497 | 189 | 159 / 204 / 740 | 27.2 (5.53) | 4.94 ms | 21.1 | 132,101 | 96 | 6.4 | 57.0 | 1,728 | 0.54 ms | 93% | 77 |
| 6000 | 6,000 | 202 | 162 / 311 / 796 | 29.5 (5.99) | 4.91 ms | 21.6 | 144,146 | 86 | 6.1 | 43.7 | 1,707 | 0.54 ms | 92% | 84 |
| 6500 | 6,509 | 291 | 177 / 694 / 1,444 | 31.6 (6.43) | 4.86 ms | 25.4 | 155,424 | 109 | 11.7 | 66.7 | 1,650 | 0.57 ms | 94% | 95 |
| 7000 | 6,819 | 881 | 950 / 1,732 / 3,122 | 33.9 (7.00) | 4.98 ms | 27.3 | 164,248 | 113 | 13.3 | 55.4 | 1,630 | 0.57 ms | 93% | 101 |
| 7500 | 6,882 | 925 | 194 / 4,906 / 6,389 | 33.9 (7.14) | 4.93 ms | 27.6 | 165,076 | 109 | 12.4 | 49.9 | 1,658 | 0.56 ms | 93% | 99 |
| 8000 | 6,899 | 928 | 193 / 4,920 / 6,390 | 33.4 (7.16) | 4.84 ms | 27.8 | 165,718 | 111 | 13.1 | 51.2 | 1,679 | 0.55 ms | 93% | 98 |
| 8500 | 6,889 | 931 | 197 / 4,910 / 6,389 | 33.5 (7.17) | 4.87 ms | 27.8 | 165,390 | 109 | 18.8 | 54.1 | 1,697 | 0.55 ms | 93% | 97 |
| 9000 | 6,817 | 941 | 195 / 4,898 / 6,388 | 33.7 (7.13) | 4.95 ms | 27.4 | 163,507 | 103 | 6.8 | 48.1 | 1,666 | 0.56 ms | 94% | 99 |

**6 × 8 (14,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5,000 | 182 | 157 / 201 / 727 | 25.8 (4.37) | 5.17 ms | 18.0 | 120,203 | 96 | 21.8 | 46.2 | 1,554 | 0.59 ms | 92% | 77 |
| 5500 | 5,500 | 186 | 158 / 202 / 720 | 27.7 (4.77) | 5.03 ms | 21.3 | 132,056 | 66 | 3.1 | 35.2 | 1,535 | 0.60 ms | 93% | 86 |
| 6000 | 6,000 | 191 | 159 / 204 / 771 | 30.6 (5.15) | 5.09 ms | 22.2 | 144,187 | 100 | 12.0 | 63.2 | 1,441 | 0.64 ms | 92% | 100 |
| 6500 | 6,501 | 229 | 165 / 475 / 976 | 32.9 (5.56) | 5.07 ms | 27.4 | 156,303 | 128 | 18.6 | 74.0 | 1,401 | 0.67 ms | 93% | 112 |
| 7000 | 7,003 | 261 | 175 / 599 / 1,180 | 34.7 (5.93) | 4.95 ms | 30.1 | 167,618 | 130 | 18.3 | 53.0 | 1,448 | 0.64 ms | 93% | 116 |
| 7500 | 7,397 | 902 | 471 / 2,654 / 3,215 | 37.2 (6.50) | 5.02 ms | 31.8 | 177,792 | 165 | 26.7 | 83.3 | 1,386 | 0.67 ms | 93% | 128 |
| 8000 | 7,430 | 1,002 | 283 / 2,793 / 3,238 | 37.7 (6.57) | 5.07 ms | 32.4 | 178,792 | 153 | 20.6 | 75.4 | 1,411 | 0.66 ms | 93% | 127 |
| 8500 | 7,377 | 1,010 | 309 / 2,798 / 3,247 | 36.9 (6.53) | 5.00 ms | 32.4 | 177,340 | 149 | 18.6 | 79.1 | 1,391 | 0.67 ms | 93% | 128 |
| 9000 | 7,358 | 1,015 | 313 / 2,795 / 3,242 | 37.3 (6.51) | 5.07 ms | 32.1 | 176,597 | 164 | 20.3 | 72.0 | 1,376 | 0.68 ms | 93% | 129 |
| 9500 | 7,434 | 1,005 | 304 / 2,796 / 3,243 | 36.7 (6.54) | 4.94 ms | 31.6 | 178,298 | 156 | 25.5 | 72.4 | 1,387 | 0.67 ms | 93% | 128 |
| 10000 | 7,401 | 1,009 | 285 / 2,787 / 3,230 | 37.0 (6.54) | 5.00 ms | 31.9 | 177,292 | 143 | 20.6 | 68.4 | 1,370 | 0.68 ms | 93% | 130 |

**7 × 8 (16,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 4,422 | 1,097 | 203 / 3,096 / 6,135 | 24.2 (3.54) | 5.47 ms | 44.0 | 102,100 | 192 | 49.8 | 81.2 | 1,258 | 0.72 ms | 91% | 80 |
| 5500 | 5,501 | 193 | 160 / 243 / 749 | 29.7 (4.29) | 5.39 ms | 33.6 | 132,117 | 122 | 10.5 | 56.7 | 1,432 | 0.64 ms | 92% | 92 |
| 6000 | 6,000 | 239 | 168 / 516 / 1,029 | 32.2 (4.69) | 5.36 ms | 35.3 | 144,212 | 124 | 22.1 | 48.5 | 1,312 | 0.70 ms | 91% | 110 |
| 6500 | 6,503 | 311 | 180 / 727 / 1,482 | 35.3 (5.11) | 5.43 ms | 41.6 | 155,474 | 146 | 31.4 | 52.8 | 1,386 | 0.66 ms | 92% | 112 |
| 7000 | 6,893 | 1,153 | 1,111 / 2,695 / 3,219 | 38.5 (5.63) | 5.59 ms | 46.0 | 165,550 | 177 | 31.5 | 83.7 | 1,348 | 0.69 ms | 92% | 122 |
| 7500 | 6,961 | 1,231 | 1,067 / 2,795 / 3,249 | 38.3 (5.72) | 5.50 ms | 46.7 | 167,357 | 179 | 28.5 | 83.3 | 1,381 | 0.67 ms | 92% | 121 |
| 8000 | 6,928 | 1,235 | 1,201 / 2,892 / 3,249 | 38.1 (5.68) | 5.50 ms | 46.8 | 166,536 | 175 | 32.4 | 83.0 | 1,381 | 0.67 ms | 92% | 120 |
| 8500 | 6,929 | 1,236 | 1,104 / 2,846 / 3,261 | 38.4 (5.68) | 5.55 ms | 46.9 | 166,231 | 157 | 24.5 | 62.2 | 1,387 | 0.66 ms | 92% | 119 |
| 9000 | 6,950 | 1,235 | 1,103 / 2,823 / 3,235 | 38.6 (5.72) | 5.55 ms | 46.8 | 167,102 | 170 | 21.8 | 76.5 | 1,402 | 0.66 ms | 92% | 119 |
| 9500 | 6,855 | 1,247 | 712 / 2,902 / 4,129 | 38.1 (5.64) | 5.55 ms | 46.0 | 164,541 | 170 | 28.6 | 70.7 | 1,313 | 0.70 ms | 92% | 125 |
| 10000 | 6,843 | 1,253 | 556 / 2,894 / 5,641 | 37.9 (5.65) | 5.54 ms | 45.8 | 164,744 | 183 | 25.7 | 96.5 | 1,293 | 0.72 ms | 93% | 127 |

**8 × 8 (18,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 2,786 | 3,395 | 3,291 / 6,263 / 12,031 | 17.6 (2.26) | 6.32 ms | 64.2 | 65,009 | 211 | 77.1 | 57.9 | 1,142 | 0.81 ms | 92% | 57 |
| 5500 | 3,072 | 2,482 | 332 / 9,826 / 13,077 | 18.3 (2.43) | 5.95 ms | 61.6 | 72,160 | 167 | 54.0 | 40.9 | 1,270 | 0.73 ms | 92% | 56 |
| 6000 | 6,008 | 212 | 162 / 346 / 814 | 35.1 (4.47) | 5.84 ms | 37.0 | 144,618 | 149 | 22.5 | 71.5 | 1,607 | 0.57 ms | 92% | 90 |
| 6500 | 6,504 | 293 | 181 / 662 / 1,084 | 37.6 (4.88) | 5.78 ms | 41.5 | 156,239 | 164 | 29.3 | 62.9 | 1,517 | 0.60 ms | 92% | 103 |
| 7000 | 7,052 | 608 | 564 / 1,308 / 1,605 | 41.7 (5.33) | 5.92 ms | 45.9 | 168,332 | 156 | 31.3 | 51.0 | 1,549 | 0.60 ms | 92% | 109 |
| 7500 | 7,064 | 1,362 | 1,388 / 2,913 / 3,255 | 42.8 (5.53) | 6.06 ms | 46.2 | 169,292 | 167 | 30.7 | 64.7 | 1,516 | 0.61 ms | 92% | 112 |
| 8000 | 7,156 | 1,347 | 1,251 / 2,941 / 4,110 | 43.0 (5.61) | 6.01 ms | 46.7 | 172,056 | 161 | 28.1 | 71.6 | 1,556 | 0.59 ms | 92% | 111 |
| 8500 | 7,137 | 1,349 | 691 / 2,961 / 5,585 | 43.2 (5.63) | 6.05 ms | 47.0 | 171,526 | 152 | 26.4 | 56.0 | 1,555 | 0.59 ms | 92% | 111 |
| 9000 | 7,153 | 1,349 | 496 / 3,162 / 6,141 | 42.9 (5.59) | 6.00 ms | 46.9 | 172,444 | 152 | 26.7 | 61.4 | 1,532 | 0.60 ms | 92% | 112 |
| 9500 | 7,063 | 1,366 | 524 / 3,221 / 6,191 | 41.9 (5.47) | 5.94 ms | 46.4 | 169,459 | 171 | 26.0 | 67.6 | 1,492 | 0.62 ms | 92% | 114 |
| 10000 | 6,844 | 1,409 | 580 / 4,459 / 6,344 | 40.8 (5.37) | 5.97 ms | 44.4 | 164,784 | 167 | 21.7 | 71.7 | 1,314 | 0.70 ms | 92% | 124 |
| 10500 | 6,935 | 1,392 | 560 / 3,729 / 6,271 | 41.9 (5.45) | 6.03 ms | 45.4 | 167,203 | 160 | 26.7 | 66.9 | 1,371 | 0.67 ms | 92% | 121 |

## Memory

Peak per replica over each step's window; per step in `steps.jsonl`. Limits: ate-api-server 4Gi per replica; size10 Postgres 140Gi.

- ate-api-server peaked at 371–599 MiB per replica across the five tests, far below its 4Gi.
- Postgres's working set grows through each run as it fills its 35GB `shared_buffers`, from about 2.4 GiB in 4 × 8's first step to 25.7 GiB in 8 × 8's last, not with demand.
- Memory limited nothing.

## Confidence and limits

**Solid**

- Every test complete, with throughput tracking the target at every step below the ceiling, except the warm-up steps at 7 and 8 × 8.
- Postgres's WAL flush is busy 92–94% at every ceiling, and ate-api-server is below its CPU limit at every one.
- 24 Postgres commits per cycle.

**Directional**

- The ceilings: one run each. In the earlier series run-to-run variation was about ±2%, up to 6.7% when fsync latency differed. The 13–15% drops at 7 and 8 × 8 are larger, and their fsync latency, 0.59–0.72 ms, is within the earlier runs' 0.6–0.8 ms; the difference is in commits per fsync.
- Contention as the cause of the drop: rising Postgres CPU per commit together with rising lock waits point at it, but no statement- or lock-level data was collected.
- The workload or the code as the source of the extra lock waits: both changed, and these runs cannot separate them.

**Not conclusive**

- Server quantiles are interpolated within doubling buckets; the means are precise.
- The `ResourceExhausted` failures' timing: inferred from the windows, not observed.

**Scope**

- Isolate mode: a fixed 150 ms per call, pause rather than suspend, no router traffic.
- One configuration: size10 Postgres on a pd-balanced volume, one run per replica count.

## To make it conclusive

1. Sample `pg_locks` and `pg_stat_activity` wait events every few seconds during a 7 × 8 run, and read `pg_stat_statements` for its saturated steps, to find the contended rows or locks.
2. Repeat 6 × 8 and 7 × 8, to confirm the drop and the peak at 6 replicas.
3. Run 7 × 8 with the earlier `resumecold` workload at `5857fd53`, or `resumepaused` at the earlier commit, to separate the workload from the code.
4. Rerun 7 × 8 and 8 × 8 with `--ladder-spawn-rate` of about 2000, to keep the warm-up out of the first steps and see whether the failures go away.

## How results are measured

`analyze_ladder.py` measures each step from 30 s after it starts to 5 s before it ends, 145 s of each 3-minute step, against the benchmarking Prometheus while it still held the run. The WAL columns diff the first and last sampler rows inside the same window, five or six samples 15 s apart.
