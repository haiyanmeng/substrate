# ate-api-server cold resume capacity at 8 CPUs with a longer data-plane delay, 2026-10-15

- Tests `ateapi_cold_resume_pause_fake_8cpu_500ms` and `_8cpu_1s`, run back to back with the local orchestrator on branch `ateapi-cold-resume-capacity` at `a1c6acd0`, 11:51–12:59 UTC. Both complete, no request failed.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_<test>/run_date=2026-10-15/`; local copies and per-step analysis (`steps.jsonl`, from `benchmarking/automation/analyze_ladder.py`) in `results/<test>/`. Orchestrator log: `~/bench-runs/ateapi-cold-8cpu-delay-20261015/orchestrator.log`.
- The test entries are the `tests.yaml` working copy on that branch, uncommitted at the time of the run: the 8 CPU fine ladder with fake-atelet's delay raised from 150 ms to 500 ms and 1 s, and the users raised to keep every step supplied.
- The 150 ms baseline is the `8cpu` test of `../ateapi-cold-6tests-20261014/summary.md`, run the day before at a different commit; see What differs from the baseline.

| Test | Delay | Users | Steps (UTC) | run_ts | Local copy |
|---|---|---|---|---|---|
| `8cpu_500ms` | 500 ms | 5000 | 11:59–12:17 | 1792065554 | `results/8cpu_500ms/` |
| `8cpu_1s` | 1 s | 8000 | 12:37–12:55 | 1792067842 | `results/8cpu_1s/` |
| `8cpu` (baseline, 2026-10-14) | 150 ms | 3000 | 14:57–15:14 | 1791989799 | `../ateapi-cold-6tests-20261014/results/8cpu/` |

## What differs from the baseline

- **The delay.** fake-atelet answers each call after 500 ms or 1 s instead of 150 ms, so each cold resume and each pause holds its server-side work open about 3× or 7× as long, and ate-api-server carries that many more calls in flight at the same rate: about 2,700 at 2500/s with 500 ms and about 5,400 with 1 s, against about 800 with 150 ms (rate × the resume and pause server means).
- **The users.** 5000 and 8000 instead of 3000, so a user is free for every step below the ceiling.
- **The code.** `a1c6acd0` carries 22 upstream commits from 2026-10-14 that the baseline's `e68233a7` does not, among them #2620 (a paused actor is defined by its snapshots; the PAUSED and PAUSING states are gone) and #2536 (snapshots stamped with SandboxRuntime; restores scheduled on runtime-compatible workers), both on the resume and pause path. Commits per cycle stayed at 24.0.

These results cannot separate the effect of the delay from that of the code. A 150 ms run at `a1c6acd0` would.

## Setup

- Isolate mode (fake data plane): fake-atelet on every atelet node answers each call after the test's delay and mints the actor certificate on each restore; 200 fake Workers of 1000 actors each, so placement never binds.
- resumepaused: each user owns one actor, creates it and makes it cold in the step that adds the user, then resumes and pauses it every iteration. One cycle is one resume plus one pause.
- Ladder: 2400–2900/s in steps of 100, 3 minutes each, all users from the first step (`--ladder-spawn-rate` equal to the users).
- ate-api-server: one replica, 8 CPU request = limit, 4Gi, `GOMAXPROCS` 8, read/write pool 64.
- Postgres: size0, as in `../ateapi-cold-2-4-8cpu-20261009/summary.md`, with `pg_stat_statements` and the postgres_exporter sidecar.
- Placement, the same in both tests: Postgres on `…-9v65`, ate-api-server on `…-4d75`, the runner (8 CPU, 2Gi) on `…-d1sf`.

## Result

| Delay | Ceiling (cycles/s) | Against 150 ms | Bound by |
|---|---|---|---|
| 150 ms (baseline) | about 2690 (2679–2696) | | ate-api-server CPU: 7.8 of 8 cores, throttled 9% |
| 500 ms | about 2540 (2530–2550) | −5.6% | ate-api-server CPU: 7.74–7.76 of 8 cores, throttled 5–6% |
| 1 s | about 2495 (2483–2504) | −7.2% | ate-api-server CPU: 7.28–7.81 of 8 cores, throttled 7–8% |

The ceiling falls as the delay grows, and ate-api-server's CPU sets it in all three: each saturates at about 7.75–7.8 of its 8 cores, and Postgres, at 7.1–7.3 of its 16, is not CPU-bound. What changes is ate-api-server's CPU per cycle:

| Delay | ate-api-server CPU per cycle at 2400–2500/s | At the ceiling | 7.8 cores ÷ CPU per cycle | Measured ceiling |
|---|---|---|---|---|
| 150 ms | 2.88–2.91 ms | 2.90–2.92 ms | about 2690 | about 2690 |
| 500 ms | 2.97–2.99 ms | 3.03–3.07 ms | about 2560 | about 2540 |
| 1 s | 3.05–3.06 ms | 3.11–3.15 ms | about 2490 | about 2495 |

Each cycle costs 5% more ate-api-server CPU at 500 ms and 8% more at 1 s, and that accounts for the lower ceilings. The cost per cycle is the same at every rate below the ceiling, so it follows the delay, that is, the calls in flight, not the rate; why, this data does not show. Its memory (see Memory) grows the same way.

Below the ceiling the server adds more on top of the delay as the delay grows: a resume's server mean is the delay plus 15–16 ms at 150 ms, plus 32–35 ms at 500 ms, and plus 75–95 ms at 1 s, at the same 2400–2500/s.

## Reading the saturated steps

Past the ceiling every user is busy, and cycles/s equals users ÷ cycle time: 5000 ÷ (1.008 + 0.929 s) = 2580/s at 500 ms (measured 2530–2550), and 8000 ÷ (1.621 + 1.526 s) = 2540/s at 1 s (measured 2483–2504). Latency rises until it absorbs the demand the server cannot serve, so the saturated steps' latency is set by the user count: the jump from about 535 to 1008 ms at 500 ms, and from about 1095 to 1620 ms at 1 s, is queuing, not a fixed wait in ate-api-server. More users would raise latency, not throughput. The ceiling is still the server's: ate-api-server was at its CPU limit before the users ran out.

## Per step

Columns as in `analyze_ladder.py`: rates are successful calls over the step's window ÷ its length; latencies are resume means unless marked; cores are cAdvisor CPU over the window; commits and connections come from postgres-exporter. Server p50 / p90 / p95 / p99 are bucket-limited; see Confidence and limits.

**500 ms (5000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns | WALWrite waits |
|---|---|---|---|---|---|---|---|---|---|---|
| 2400 | 2403.6 | 532.5 / 532.2 | 615 / 780 / 801 / 817 | 526.9 | 7.20 | 1.0% | 6.50 | 57,819 | 23.0 | 13.8 |
| 2500 | 2500.0 | 535.7 / 535.3 | 614 / 778 / 799 / 815 | 529.6 | 7.43 | 1.0% | 6.87 | 60,122 | 27.7 | 17.3 |
| 2600 | 2529.7 | 1008.3 / 1007.6 | 1229 / 1556 / 1597 / 1630 | 929.4 | 7.76 | 5.8% | 7.20 | 60,615 | 28.2 | 15.7 |
| 2700 | 2543.4 | 1007.6 / 1006.7 | 1228 / 1556 / 1597 / 1630 | 928.2 | 7.75 | 6.0% | 7.27 | 61,082 | 21.8 | 15.0 |
| 2800 | 2536.6 | 1011.3 / 1010.2 | 1229 / 1556 / 1597 / 1630 | 930.7 | 7.74 | 4.8% | 7.23 | 60,964 | 24.1 | 14.7 |
| 2900 | 2550.1 | 1008.9 / 1008.1 | 1229 / 1556 / 1597 / 1630 | 929.2 | 7.74 | 4.9% | 7.30 | 61,156 | 28.7 | 18.0 |

2500/s held at 535 ms; from 2600/s throughput stays at 2530–2550/s with ate-api-server at 7.74–7.76 cores.

**1 s (8000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s | Active PG conns | WALWrite waits |
|---|---|---|---|---|---|---|---|---|---|---|
| 2400 | 2399.5 | 1075.8 / 1075.4 | 1229 / 1557 / 1598 / 1631 | 1067.7 | 7.34 | 4.3% | 6.60 | 57,717 | 33.7 | 26.7 |
| 2500 | 2500.4 | 1095.2 / 1094.6 | 1229 / 1556 / 1597 / 1630 | 1083.2 | 7.62 | 7.6% | 7.03 | 60,051 | 21.1 | 11.2 |
| 2600 | 2491.1 | 1620.9 / 1619.9 | 1410 / 2744 / 3010 / 3223 | 1526.2 | 7.81 | 7.5% | 7.15 | 59,860 | 26.5 | 15.5 |
| 2700 | 2504.2 | 1615.6 / 1614.7 | 1388 / 2693 / 2985 / 3218 | 1522.1 | 7.28 | 7.6% | 7.19 | 60,082 | 31.5 | 20.6 |
| 2800 | 2503.4 | 1621.4 / 1620.4 | 1397 / 2714 / 2995 / 3221 | 1526.2 | 7.78 | 6.8% | 7.19 | 60,080 | 27.1 | 17.4 |
| 2900 | 2483.4 | 1639.0 / 1638.3 | 1541 / 2898 / 3087 / 3239 | 1542.1 | 7.81 | 7.2% | 7.12 | 59,697 | 25.6 | 15.3 |

2500/s held at 1095 ms; from 2600/s throughput stays at 2483–2504/s. ate-api-server read 7.28 cores at 2700/s against 7.78–7.81 at the other saturated steps, at the same rate and throttling; this data does not explain the dip. Past the ceiling the latency spread widens: the server p50 stays near 1.4 s while the p90 reaches 2.7–2.9 s, so some calls waited far longer than others.

**150 ms baseline (3000 users, 2026-10-14, from `../ateapi-cold-6tests-20261014/`)**

| Target | Resume/s | Resume mean client / server (ms) | Pause server mean | ateapi cores | Throttled | Postgres cores | Commits/s |
|---|---|---|---|---|---|---|---|
| 2400 | 2403.1 | 165.9 / 165.8 | 162.9 | 6.99 | 0.2% | 6.20 | 57,583 |
| 2500 | 2500.0 | 164.8 / 164.8 | 162.0 | 7.20 | 0.1% | 6.58 | 59,995 |
| 2600 | 2600.1 | 174.6 / 174.5 | 170.0 | 7.41 | 0.4% | 7.06 | 62,441 |
| 2700 | 2695.6 | 201.9 / 201.5 | 191.7 | 7.09 | 1.8% | 7.44 | 64,680 |
| 2800 | 2678.8 | 587.9 / 587.5 | 517.6 | 7.81 | 8.7% | 7.72 | 64,528 |
| 2900 | 2689.9 | 587.4 / 586.9 | 516.8 | 7.80 | 9.1% | 7.70 | 64,498 |

That run's analysis predates the p90 / p95 quantiles, the WAL wait columns and the memory columns.

## Cost per cycle

| | ateapi CPU per cycle | Postgres CPU per cycle | Commits per cycle |
|---|---|---|---|
| 150 ms, 2500/s | 2.88 ms | 2.63 ms | 24.0 |
| 150 ms, 2690/s (ceiling) | 2.90 ms | 2.86 ms | 24.0 |
| 500 ms, 2500/s | 2.97 ms | 2.75 ms | 24.0 |
| 500 ms, 2540/s (ceiling) | 3.03–3.07 ms | 2.85–2.86 ms | 24.0 |
| 1 s, 2500/s | 3.05 ms | 2.81 ms | 24.0 |
| 1 s, 2495/s (ceiling) | 3.11–3.15 ms (2.91 at 2700/s) | 2.87 ms | 24.0 |

- Every cycle costs 24 Postgres commits at every rate and delay, and Postgres's CPU per cycle barely moves with the delay (2.85–2.87 ms at each ceiling). The extra cost is ate-api-server's.
- Lock waits stayed at 1–2 connections and 11–27 connections waited on `WALWrite` at a time, with commits at about 60,000/s, well below the about 92,000/s where Postgres's commit path stopped the 2 × 8 CPU tests. Postgres is not the limit here.

## Memory

Peak over each step's window, from cAdvisor and postgres-exporter; per step in `steps.jsonl` (`ateapi_mem_*`, `postgres_mem_*`, `pg_cache_hit_ratio`, `pg_blks_read_per_s`). Limits: ate-api-server 4Gi, Postgres 2Gi.

| Test | ate-api-server working set, peak: steps that held / saturated steps | Postgres working set, lowest → highest step | Postgres RSS | Buffer cache hit rate, lowest step | Blocks read/s, highest step |
|---|---|---|---|---|---|
| 500 ms | 326–330 / 387–398 MiB | 468 → 546 MiB | 184–187 MiB | 99.827% | 622 |
| 1 s | 573–579 / 599–615 MiB | 493 → 541 MiB | 180–185 MiB | 99.796% | 755 |

- ate-api-server's memory follows the calls in flight: at the same 2400–2500/s it holds about 330 MiB with 500 ms and about 575 MiB with 1 s, about twice the calls in flight and about 1.75× the memory. The baseline's analysis has no memory columns; the 2026-10-09 8 CPU fine run, at 150 ms, peaked at 162 MiB on steps that held.
- Memory limited nothing: at most 615 MiB of 4Gi for ate-api-server and 546 MiB of 2Gi for Postgres, memory pressure stalls under 0.01% of the time.

## Confidence and limits

**Solid**

- Both tests complete with no failed request, and throughput tracked the target at every step below the ceiling.
- ate-api-server's CPU limit sets both ceilings: 7.74–7.81 of 8 cores at the saturated steps, with Postgres at under half its CPU and its commit rate well below where it saturates.
- 24 Postgres commits per cycle at every step.

**Directional**

- The ceilings, about 2540/s at 500 ms and 2495/s at 1 s: one run each. Run-to-run variation at 8 CPUs was within 2% in earlier repeats, smaller than the 5.6% and 7.2% drops.
- That the delay causes the drop: ate-api-server's higher CPU per cycle predicts each ceiling within 1%, but the baseline ran at different code (see What differs from the baseline), so part of the difference may be the code.
- The higher CPU per cycle and memory with a longer delay: measured, consistent across steps, and largest at 1 s; where in ate-api-server the extra CPU goes is not.

**Not conclusive**

- Server quantiles: `locust_server_duration_milliseconds` has doubling buckets (409.6, 819.2, 1638.4, 3276.8 ms), and `histogram_quantile` interpolates within one. The 615 / 780 / 801 / 817 of the 500 ms steps that held say only that calls took 410–819 ms, and the 1229 / 1556 / 1597 / 1630 that recur at both delays say 819–1638 ms. Only the 1 s saturated steps, whose calls spread across two buckets, show a real spread. The means are precise.
- The dip to 7.28 cores at the 1 s test's 2700/s step.

**Scope and observations**

- Isolate mode: a fixed delay per call with no tail, pause rather than suspend, no router traffic.
- One configuration: one 8 CPU replica on size0 Postgres with default tuning.
- The 1 s test's ate-api-server container restarted once during setup, 7 s before the orchestrator listed the pods and before the runner started; nothing failed during the run. The cause is not in the orchestrator log.
- kubectl lost its Google credentials briefly around 12:27 UTC, after the 500 ms test's ladder ended. Two of the orchestrator's job-status checks failed, it retried, and the run continued.

**To make it conclusive**

1. Run `ateapi_cold_resume_pause_fake_8cpu` (150 ms) at `a1c6acd0`, to separate the delay from the code.
2. A Go CPU and heap profile of ate-api-server at 2500/s with 1 s and with 150 ms, to see what grows with the calls in flight.
3. Repeat both tests once, to see whether the 1 s test's dip at 2700/s and the ceilings hold.

## How results are measured

`analyze_ladder.py` measures each step from 30 s after it starts to 5 s before it ends, 145 s of each 3-minute step, against the benchmarking Prometheus while it still held the run. The 2026-10-09 tiny run's summary (`~/bench-runs/ateapi-cold-tiny-20261009/summary.md`) describes each column in detail.
