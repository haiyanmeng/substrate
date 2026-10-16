# ate-api-server replica scaling on size10 Postgres, 4 to 9 × 8 CPUs, 2026-10-15

- Tests `ateapi_cold_resume_pause_fake_4x8cpu_size10` to `_8x8cpu_size10`, run back to back with the local orchestrator on branch `ateapi-cold-resume-capacity` at `5857fd53`, 13:21–17:04 UTC. Every test complete; 5 of about 59.3 million cold resumes failed (see Failures).
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_<n>x8cpu_size10/run_date=2026-10-15/`; local copies and per-step analysis (`steps.jsonl`, from `benchmarking/automation/analyze_ladder.py`) in `results/<n>x8cpu_size10/`. Postgres's WAL, commit and wait counters, sampled every 15 s across the series, are in `results/wal-io.csv`. Orchestrator log: `~/bench-runs/ateapi-cold-4to8x8-size10-20261015/orchestrator.log`.
- Reruns of `_7x8cpu_size10` and `_8x8cpu_size10`, same commit (`6f6f44c1`, which adds only this summary) and test entries, 18:01–19:36 UTC: both complete; 1 of about 28.1 million cold resumes and 3 warm-up resumes failed. Local copies in `results/rerun/<n>x8cpu_size10/`, WAL samples in `results/rerun/wal-io.csv`, log in `~/bench-runs/ateapi-cold-7x8-8x8-size10-rerun-20261015/orchestrator.log`.
- `ateapi_cold_resume_pause_fake_9x8cpu_size10`, added to `tests.yaml` for this run (uncommitted at the time), at `6f6f44c1`, 21:11–22:18 UTC: complete, none of about 16.1 million cold resumes failed. Local copy in `results/9x8cpu_size10/`, WAL samples in `results/9x8cpu_size10/wal-io.csv` (none from 7500/s on; see Per step), log in `~/bench-runs/ateapi-cold-9x8-size10-20261015/orchestrator.log`.
- The earlier series is `../ateapi-cold-2-4-8cpu-20261009/summary-size10-replicas.md`, run on 2026-10-11 and repeated on 2026-10-12.

| Test | Ladder (UTC) | run_ts | Local copy |
|---|---|---|---|
| `4x8cpu_size10` | 13:29–13:56 | 1792070954 | `results/4x8cpu_size10/` |
| `5x8cpu_size10` | 14:08–14:38 | 1792073296 | `results/5x8cpu_size10/` |
| `6x8cpu_size10` | 14:50–15:23 | 1792075811 | `results/6x8cpu_size10/` |
| `7x8cpu_size10` | 15:35–16:08 | 1792078551 | `results/7x8cpu_size10/` |
| `8x8cpu_size10` | 16:23–16:59 | 1792081385 | `results/8x8cpu_size10/` |
| `7x8cpu_size10`, rerun | 18:09–18:42 | 1792087782 | `results/rerun/7x8cpu_size10/` |
| `8x8cpu_size10`, rerun | 18:55–19:31 | 1792090522 | `results/rerun/8x8cpu_size10/` |
| `9x8cpu_size10` | 21:19–21:58 | 1792099178 | `results/9x8cpu_size10/` |

**Result: the ceiling rises up to 6 replicas, as before, then levels off.**

- **Ceilings:** about 5950, 6870 and 7400 cycles/s at 4, 5 and 6 replicas, within 2% of the earlier series. The reruns of 7 × 8 and 8 × 8 reached about 7500 and 7400, the same as 6 × 8 and 9% below the earlier series' 8200 and 8150, and 9 × 8 about 7450: from 6 to 9 replicas the ceiling stays at 7400–7500/s.
- **Postgres's WAL flush sets the cap in every test**: busy 92–94% of the time at each ceiling it was sampled at, every one but 9 × 8's. ate-api-server is never at its CPU limit, and size10 Postgres has none. Past 6 × 8 commits stay at about 178,000–182,000/s; the gap to the earlier series' 205,000 is in fsync latency, 0.70–0.76 ms against about 0.65 ms, within the run-to-run variation the earlier series measured.
- **The first 7 × 8 and 8 × 8 runs fell to about 6930 and 7100/s, and the reruns did not repeat it.** In the first runs Postgres spent about 55% more CPU per commit, 46–47 cores against 31–33, after a warm-up surge swamped their first steps; the reruns, with the same settings, had no surge and no extra cost. What put Postgres in that state is not shown (see The first 7 × 8 and 8 × 8 runs).
- **Lock waits are 2–3× the earlier series' at every size**, about 20–30 connections from 6 × 8 up, in the first runs and the reruns alike, so they did not cause the drop. The workload and the code both changed since the earlier series (see What differs from the earlier series), so which one adds them is not shown.

## What differs from the earlier series

- **Workload.** `resumepaused`: each user owns one actor, creates it and makes it cold in the step that adds the user, then resumes and pauses it every iteration. The earlier series ran `resumecold`, where users took paused actors from a shared fleet. One cycle is still one resume plus one pause; it now costs 24 Postgres commits instead of 25.
- **Code.** `5857fd53` is `ateapi-cold-resume-capacity`, based on upstream main of 2026-10-14. Against the earlier series' `fake-dataplane-test` branch it brings in, among others, #2620 (a paused actor is defined by its snapshots; the PAUSED and PAUSING states are gone) and #2536 (snapshots stamped with SandboxRuntime; restores scheduled on runtime-compatible workers), both on the resume and pause path.
- **Fake data plane install.** `ate-setup --fake-data-plane` and an `isolate` block, upstream's version of the earlier branch's `fakeDataPlane` block. fake-atelet also mints the actor certificate on each restore.

The test entries are otherwise the same: users, pools, ladders and runner sizes below.

## Setup

- **Data plane:** isolate mode, fake-atelet on every atelet node answering each call after 150 ms, 200 fake Workers of 1000 actors each.
- **Postgres:** size10 (80 CPU requested, no CPU limit, 140Gi, `shared_buffers = 35GB`, `max_connections = 300`, size10's WAL settings) with `track_wal_io_timing = on`.
- **ate-api-server:** 8 / 8 CPU per replica (request = limit), 4Gi, `GOMAXPROCS` 8, every replica on one node.
- **Placement:** Postgres, ate-api-server and the runner each on their own node. Postgres was on `…-9v65` and ate-api-server on `…-4d75` for 4, 5 and 8 × 8, and the other way round for 6 and 7 × 8; the runner was on `…-d1sf` throughout. The 6 × 8 and 7 × 8 runs had the same layout. Both reruns and 9 × 8 had Postgres on `…-4d75` and ate-api-server on `…-9v65`; at 9 × 8 its 72 CPUs fit that node's 88.

| | 4 × 8 | 5 × 8 | 6 × 8 | 7 × 8 | 8 × 8 | 9 × 8 |
|---|---|---|---|---|---|---|
| Total ate-api-server CPUs | 32 | 40 | 48 | 56 | 64 | 72 |
| Read/write pool per replica | 40 | 40 | 40 | 35 | 30 | 26 |
| Postgres connections, pools plus 5 owner and watch per replica | 180 | 225 | 270 | 280 | 280 | 279 |
| Users and actors | 10,000 | 12,000 | 14,000 | 16,000 | 18,000 | 20,000 |
| Ladder, 3-minute steps of 500 | 4000–8000/s | 4500–9000/s | 5000–10000/s | 5000–10000/s | 5000–10500/s | 5000–11000/s |
| Runner CPU / memory | 16 / 4Gi | 16 / 5Gi | 16 / 6Gi | 16 / 6Gi | 16 / 7Gi | 16 / 8Gi |

## Result

| ate-api-server | Ceiling (cycles/s) | Earlier series | Change | ate-api-server cores used | ateapi CPU per cycle | Postgres cores | Commits/s | fsyncs/s | Mean fsync | Commits per fsync | Lock waits |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 4 × 8 | about 5950 (5938–5988) | about 5930 | same | 27.1–27.9 of 32 (86%) | 4.56–4.66 ms | 22.1–22.3 | about 143,000 | 1,750–1,818 | 0.52–0.54 ms | 79–83 | 3–6 |
| 5 × 8 | about 6870 (6817–6899) | about 6750 | +2% | 33.4–33.9 of 40 (84%) | 4.84–4.98 ms | 27.3–27.8 | about 165,000 | 1,630–1,697 | 0.55–0.57 ms | 97–101 | 7–19 |
| 6 × 8 | about 7400 (7358–7434) | about 7340 | +1% | 36.7–37.7 of 48 (77%) | 4.94–5.07 ms | 31.6–32.4 | about 178,000 | 1,370–1,411 | 0.66–0.68 ms | 127–130 | 19–27 |
| 7 × 8, rerun | about 7500 (7419–7578) | about 8200 | −9% | 40.3–40.8 of 56 (72%) | 5.38–5.42 ms | 30.5–32.9 | about 180,000 | 1,291–1,320 | 0.70–0.72 ms | 135–140 | 22–32 |
| 8 × 8, rerun | about 7400 (7345–7446) | about 8150 | −9% | 41.3–43.1 of 64 (66%) | 5.61–5.80 ms | 32.2–33.4 | about 178,000 | 1,223–1,274 | 0.73–0.76 ms | 140–144 | 21–30 |
| 9 × 8 | about 7450 (7373–7517) | | | 44.8–45.9 of 72 (63%) | 6.05–6.12 ms | 32.5–33.8 | about 179,000 | not sampled | | | 25–34 |
| 7 × 8, first run | about 6930 (6843–6961) | about 8200 | −15% | 37.9–38.6 of 56 (68%) | 5.50–5.59 ms | 45.8–46.9 | about 166,000 | 1,293–1,402 | 0.66–0.72 ms | 119–127 | 22–32 |
| 8 × 8, first run | about 7100 (6844–7156) | about 8150 | −13% | 40.8–43.2 of 64 (66%) | 5.94–6.06 ms | 44.4–47.0 | about 170,000 | 1,314–1,556 | 0.59–0.70 ms | 111–124 | 22–31 |

Ceilings are the saturated steps' range; the WAL columns are from the sampler over the same windows. The reruns stand for 7 and 8 × 8; the first runs are kept for The first 7 × 8 and 8 × 8 runs below.

**Postgres's commit path.** In every test the WAL flush runs back to back, busy 92–94% of each saturated step, so commits/s is fsyncs/s times commits per fsync. Up to 6 × 8 more replicas pack more commits into each fsync, 80 to 129, faster than fsyncs slow, 0.52 to 0.68 ms, and the ceiling rises, as in the earlier series. In the reruns, 7 and 8 × 8 pack a few more, 135–144, but fsyncs slow to 0.70–0.76 ms, so commits stay at about 178,000–182,000/s and the ceiling at 7400–7500/s. 9 × 8 levels off at the same commits, about 178,000–180,000/s, from 7500/s on; at its last sampled step, 7000/s, the flush was busy 93% at about 1,400 fsyncs/s of 0.67 ms, carrying 120 commits each. A ninth replica adds connections and CPU but no commits. The earlier series' 7 × 8 and 8 × 8 reached about 205,000 commits/s with about 143 commits per fsync at about 0.65 ms (8 × 8): the same batching on a faster flush. That series measured fsync latency varying by up to 30% between runs at the same configuration.

**The first 7 × 8 and 8 × 8 runs.** Same commit, test entries and Postgres settings as the reruns, but Postgres spent far more CPU on each commit, and commits and the ceiling fell:

| | Postgres cores | Commits/s | Postgres CPU per commit | Lock waits | First steps |
|---|---|---|---|---|---|
| 6 × 8 | 32.1 | 176,600 | 0.18 ms | 19–27 | held |
| 7 × 8, first run | 46.8 | 166,500 | 0.28 ms | 22–32 | swamped by the warm-up |
| 8 × 8, first run | 46.9 | 172,400 | 0.27 ms | 22–31 | swamped by the warm-up |
| 7 × 8, rerun | 32.2 | 180,200 | 0.18 ms | 22–32 | held |
| 8 × 8, rerun | 32.9 | 177,600 | 0.19 ms | 21–30 | held |
| 6 × 8, earlier series | 29.2–31.8 | about 183,000 | about 0.17 ms | 6.9–10.3 | |
| 8 × 8, earlier series | 33.4–36.7 | about 205,000 | about 0.17 ms | 8.6 | |

- The extra CPU per commit, about 55%, came only in the two runs whose first steps the warm-up surge swamped (see The warm-up surge), and it lasted to their last step. The reruns had neither.
- Lock waits were the same in the first runs and the reruns, so they do not explain it.
- What made each commit dearer is not shown: no statement- or lock-level data was collected. One candidate is a plan choice from table statistics that a burst of 16,000–18,000 inserts left stale; `pg_stat_statements` for the first runs' saturated steps, if Prometheus still holds them, would show which statements grew.

**ate-api-server.** Its share of CPU in use falls from 86% at 4 × 8 to 66% at 8 × 8 and 63% at 9 × 8, no replica reaching its 8-core limit (busiest 7.33 at 4 × 8, 5.2–6.1 at 7 to 9 × 8) and none throttled, so it does not set the ceilings. Its CPU per cycle rises with the replica count, 4.6 ms at 4 × 8 to 5.4 ms at 7 × 8, 5.6–5.8 ms at 8 × 8 in the reruns, within 4% of the earlier series, and 6.05–6.12 ms at 9 × 8. At the same 5000/s it is 5.29 ms at 7 × 8, 5.50 at 8 × 8 and 5.68 at 9 × 8, about 0.2 ms per added replica. The first runs' 5.5 and 6.0 ms were higher still.

## The warm-up surge

Each user creates and warms its actor in the first step, and `--ladder-spawn-rate` equals the users, so at 7 × 8 and 8 × 8, 16,000 and 18,000 actors are created and made cold within seconds. In the first runs Postgres jumped to 44–64 cores with 50–77 connections waiting on locks, and throughput fell to 4422/s in 7 × 8's first step and to 2786 and 3072/s in 8 × 8's first two, with server means of 1.1–3.4 s. Those steps measure the warm-up, not the ladder; the ceilings above leave them out.

The reruns, with the same spawn rate, held their first steps: 5000/s at 7 × 8 and 5017 and 5500/s at 8 × 8, with Postgres at 19–22 cores and 3–22 connections waiting on locks. The same burst of creates swamped Postgres in one run and not the other, and the runs that it swamped kept the higher cost per commit to the end. At 4 to 6 × 8 the first step held its target, and so did 9 × 8's (5005/s, Postgres at 20 cores, 7 connections waiting on locks) with 20,000 users at the same spawn rate. A spawn rate of a few thousand per second would spread the warm-up out.

## Failures

In the first runs 5 cold resumes failed, 3 at 7 × 8 and 2 at 8 × 8, each with `ResourceExhausted: no worker has room for the actor`. In the reruns 7 × 8 had none, and 8 × 8 had 1 more `ResourceExhausted` cold resume and 3 warm-up resumes that failed with `Aborted: concurrent update conflict, please retry`. 200 fake Workers hold 200,000 actors against at most 18,000, so `ResourceExhausted` is not exhaustion. 9 × 8, with 20,000 users, had no failures. None of these fell inside a measured window. The 8 × 8 rerun had them without the warm-up surge, so they are not only a product of it: a placement race at the largest user counts, worth a look in ate-api-server's logs.

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

**7 × 8, rerun (16,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5,000 | 177 | 157 / 200 / 614 | 26.5 (3.87) | 5.29 ms | 18.9 | 120,181 | 85 | 6.9 | 52.0 | 1,452 | 0.64 ms | 92% | 83 |
| 5500 | 5,501 | 188 | 158 / 203 / 740 | 29.4 (4.26) | 5.34 ms | 21.5 | 132,223 | 141 | 22.4 | 85.6 | 1,376 | 0.68 ms | 93% | 97 |
| 6000 | 6,000 | 205 | 161 / 282 / 818 | 31.7 (4.62) | 5.28 ms | 24.2 | 144,261 | 130 | 10.6 | 67.8 | 1,317 | 0.70 ms | 92% | 109 |
| 6500 | 6,515 | 197 | 161 / 273 / 777 | 34.4 (5.01) | 5.28 ms | 26.4 | 156,217 | 109 | 12.6 | 48.1 | 1,352 | 0.69 ms | 93% | 115 |
| 7000 | 7,002 | 281 | 178 / 657 / 1,380 | 37.3 (5.39) | 5.33 ms | 30.2 | 168,926 | 167 | 28.5 | 84.2 | 1,317 | 0.71 ms | 94% | 129 |
| 7500 | 7,419 | 879 | 872 / 1,593 / 3,057 | 40.2 (5.98) | 5.42 ms | 30.5 | 178,425 | 170 | 32.0 | 90.5 | 1,319 | 0.71 ms | 93% | 135 |
| 8000 | 7,455 | 1,145 | 1,045 / 2,751 / 3,229 | 40.4 (5.95) | 5.42 ms | 32.2 | 179,226 | 158 | 21.9 | 73.7 | 1,291 | 0.72 ms | 93% | 139 |
| 8500 | 7,497 | 1,141 | 405 / 2,773 / 3,267 | 40.5 (6.01) | 5.40 ms | 32.6 | 180,412 | 167 | 25.2 | 79.9 | 1,311 | 0.71 ms | 94% | 138 |
| 9000 | 7,507 | 1,138 | 394 / 2,772 / 3,267 | 40.5 (6.01) | 5.39 ms | 32.5 | 179,750 | 179 | 27.6 | 81.8 | 1,297 | 0.72 ms | 93% | 139 |
| 9500 | 7,505 | 1,140 | 393 / 2,783 / 4,727 | 40.5 (6.02) | 5.40 ms | 32.5 | 180,804 | 181 | 25.2 | 90.2 | 1,291 | 0.72 ms | 93% | 140 |
| 10000 | 7,578 | 1,129 | 453 / 2,863 / 5,670 | 40.7 (6.08) | 5.38 ms | 32.9 | 182,673 | 168 | 22.7 | 80.7 | 1,320 | 0.70 ms | 93% | 138 |

**8 × 8, rerun (18,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5,017 | 180 | 157 / 201 / 668 | 27.6 (3.55) | 5.50 ms | 19.3 | 120,173 | 62 | 2.9 | 29.6 | 1,439 | 0.64 ms | 92% | 83 |
| 5500 | 5,500 | 184 | 158 / 202 / 694 | 30.7 (3.89) | 5.58 ms | 21.5 | 132,175 | 96 | 11.9 | 54.4 | 1,411 | 0.66 ms | 93% | 94 |
| 6000 | 6,001 | 199 | 160 / 243 / 804 | 33.5 (4.24) | 5.58 ms | 24.2 | 144,243 | 190 | 29.2 | 115.4 | 1,353 | 0.68 ms | 93% | 107 |
| 6500 | 6,501 | 204 | 162 / 323 / 795 | 36.5 (4.62) | 5.61 ms | 27.2 | 155,794 | 146 | 19.3 | 82.0 | 1,340 | 0.70 ms | 94% | 117 |
| 7000 | 7,001 | 272 | 179 / 626 / 1,023 | 39.4 (4.97) | 5.63 ms | 30.4 | 168,178 | 144 | 24.5 | 68.6 | 1,273 | 0.73 ms | 93% | 132 |
| 7500 | 7,345 | 1,180 | 1,157 / 2,550 / 3,204 | 42.4 (5.49) | 5.77 ms | 32.6 | 176,599 | 172 | 29.6 | 82.5 | 1,260 | 0.74 ms | 93% | 140 |
| 8000 | 7,353 | 1,310 | 844 / 2,923 / 4,752 | 42.5 (5.51) | 5.78 ms | 32.7 | 176,486 | 167 | 24.4 | 78.3 | 1,223 | 0.76 ms | 93% | 144 |
| 8500 | 7,435 | 1,294 | 860 / 2,904 / 3,536 | 43.1 (5.57) | 5.80 ms | 32.2 | 179,076 | 163 | 26.7 | 83.6 | 1,257 | 0.74 ms | 93% | 142 |
| 9000 | 7,446 | 1,293 | 860 / 2,955 / 4,839 | 43.1 (5.58) | 5.78 ms | 32.6 | 178,628 | 157 | 20.9 | 75.3 | 1,274 | 0.73 ms | 94% | 141 |
| 9500 | 7,351 | 1,312 | 848 / 2,956 / 5,240 | 42.7 (5.51) | 5.80 ms | 33.2 | 177,227 | 176 | 25.1 | 85.1 | 1,247 | 0.74 ms | 93% | 141 |
| 10000 | 7,367 | 1,309 | 840 / 2,941 / 5,306 | 41.3 (5.52) | 5.61 ms | 33.4 | 177,646 | 178 | 27.3 | 91.3 | 1,252 | 0.74 ms | 93% | 141 |
| 10500 | 7,372 | 1,305 | 626 / 2,937 / 5,373 | 42.5 (5.51) | 5.77 ms | 33.3 | 177,414 | 165 | 24.9 | 73.8 | 1,235 | 0.75 ms | 93% | 142 |

**9 × 8 (20,000 users)**

| Target | Resume/s | Resume mean server (ms) | Server p50 / p90 / p99 | ateapi cores (busiest replica) | ateapi CPU per cycle | Postgres cores | Commits/s | Active PG conns | Lock waits | `WALWrite` waits | fsyncs/s | Mean fsync | Flush busy | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5,005 | 178 | 157 / 200 / 629 | 28.4 (3.26) | 5.68 ms | 20.1 | 120,948 | 85 | 7.1 | 41.9 | 1,534 | 0.60 ms | 92% | 78 |
| 5500 | 5,499 | 186 | 158 / 203 / 702 | 31.9 (3.60) | 5.81 ms | 22.7 | 132,097 | 135 | 24.1 | 65.1 | 1,475 | 0.63 ms | 93% | 90 |
| 6000 | 6,001 | 200 | 160 / 262 / 801 | 34.8 (3.94) | 5.79 ms | 25.5 | 144,302 | 124 | 13.1 | 55.5 | 1,435 | 0.65 ms | 93% | 101 |
| 6500 | 6,521 | 211 | 164 / 360 / 802 | 38.2 (4.31) | 5.86 ms | 27.1 | 156,725 | 160 | 33.6 | 72.9 | 1,421 | 0.66 ms | 93% | 110 |
| 7000 | 6,975 | 278 | 173 / 645 / 1,379 | 41.0 (4.63) | 5.88 ms | 30.4 | 167,500 | 150 | 28.3 | 65.3 | 1,397 | 0.67 ms | 93% | 120 |
| 7500 | 7,453 | 1,235 | 1,178 / 2,623 / 3,211 | 45.6 (5.18) | 6.11 ms | 32.9 | 179,258 | 173 | 31.7 | 76.8 | not sampled | | | |
| 8000 | 7,373 | 1,445 | 1,184 / 2,878 / 4,918 | 44.8 (5.16) | 6.08 ms | 32.7 | 177,604 | 168 | 33.8 | 88.4 | not sampled | | | |
| 8500 | 7,417 | 1,443 | 1,024 / 2,903 / 5,473 | 45.4 (5.20) | 6.12 ms | 33.8 | 178,071 | 175 | 31.9 | 78.9 | not sampled | | | |
| 9000 | 7,509 | 1,428 | 984 / 2,881 / 5,123 | 45.4 (5.25) | 6.05 ms | 32.9 | 180,490 | 148 | 25.3 | 64.8 | not sampled | | | |
| 9500 | 7,517 | 1,426 | 1,104 / 2,900 / 5,434 | 45.9 (5.24) | 6.10 ms | 32.9 | 180,381 | 150 | 27.6 | 62.5 | not sampled | | | |
| 10000 | 7,429 | 1,444 | 841 / 2,990 / 5,899 | 45.0 (5.21) | 6.06 ms | 32.5 | 178,331 | 173 | 31.6 | 97.7 | not sampled | | | |
| 10500 | 7,463 | 1,436 | 995 / 2,909 / 5,536 | 45.2 (5.22) | 6.06 ms | 33.0 | 179,351 | 157 | 29.7 | 66.8 | not sampled | | | |
| 11000 | 7,434 | 1,444 | 1,097 / 2,929 / 5,628 | 45.4 (5.20) | 6.11 ms | 33.0 | 178,586 | 153 | 28.5 | 64.4 | not sampled | | | |

The WAL sampler, which runs on the workstation, stopped while it was suspended from 21:34 to 22:17 UTC, so it has no samples from 7500/s on; the runner and Prometheus, in the cluster, ran throughout.

## Memory

Peak per replica over each step's window; per step in `steps.jsonl`. Limits: ate-api-server 4Gi per replica; size10 Postgres 140Gi.

- ate-api-server peaked at 349–599 MiB per replica across the six tests and the two reruns, far below its 4Gi.
- Postgres's working set grows through each run as it fills its 35GB `shared_buffers`, from about 2.4 GiB in 4 × 8's first step to 25.7 GiB in 8 × 8's last, not with demand.
- Memory limited nothing.

## Confidence and limits

**Solid**

- Every test complete, with throughput tracking the target at every step below the ceiling, except the warm-up steps of the first 7 and 8 × 8 runs.
- Postgres's WAL flush is busy 92–94% at every ceiling, and ate-api-server is below its CPU limit at every one.
- 24 Postgres commits per cycle.

**Directional**

- The ceilings at 4 to 6 × 8: one run each. In the earlier series run-to-run variation was about ±2%, up to 6.7% when fsync latency differed.
- The level ceiling past 6 × 8, about 7400–7500/s: one rerun each at 7 and 8 × 8 and one run at 9 × 8, consistent with each other and with 6 × 8. Their 9% gap to the earlier series lines up with slower fsyncs, a known source of run-to-run variation, but no repeat at the earlier series' fsync latency shows it.
- That the first 7 and 8 × 8 runs are outliers: their extra Postgres CPU per commit and their swamped first steps came together and did not recur, but why is not shown.
- The workload or the code as the source of the extra lock waits: both changed, and these runs cannot separate them. They did not cost throughput.

**Not conclusive**

- Server quantiles are interpolated within doubling buckets; the means are precise.
- The `ResourceExhausted` and `Aborted` failures' timing: inferred from the windows, not observed.
- What made Postgres's commits dearer in the first 7 and 8 × 8 runs.
- That the WAL flush sets the 9 × 8 ceiling: inferred from commits levelling off at the same 178,000–180,000/s as 7 and 8 × 8, with the flush busy 93% at the last sampled step; its saturated steps have no WAL samples.

**Scope**

- Isolate mode: a fixed 150 ms per call, pause rather than suspend, no router traffic.
- One configuration: size10 Postgres on a pd-balanced volume; one run at 4 to 6 and 9 × 8, two at 7 and 8 × 8.

## To make it conclusive

1. Read `pg_stat_statements` for the first 7 × 8 and 8 × 8 runs' saturated steps against the reruns', while Prometheus holds them, to see which statements cost more after the warm-up surge.
2. Run 7 × 8 and 8 × 8 with `--ladder-spawn-rate` of about 2000, so no run starts with the surge, and see whether the failures go away.
3. Repeat 6 to 9 × 8 with the WAL sampler running for every step, to confirm the level ceiling past 6 × 8 and how much of the gap to the earlier series follows fsync latency.
4. Run 7 × 8 with the earlier `resumecold` workload at `5857fd53`, or `resumepaused` at the earlier commit, to separate the workload from the code, and sample `pg_locks` to find what the extra lock waits are on.

## How results are measured

`analyze_ladder.py` measures each step from 30 s after it starts to 5 s before it ends, 145 s of each 3-minute step, against the benchmarking Prometheus while it still held the run. The WAL columns diff the first and last sampler rows inside the same window, five or six samples 15 s apart.
