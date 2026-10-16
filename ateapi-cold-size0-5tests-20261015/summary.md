# ate-api-server cold resume capacity on size0, 2 to 10 CPUs and 2 × 4 CPUs, 2026-10-15 and -16

- Tests `ateapi_cold_resume_pause_fake_2x4cpu`, `_8cpu`, `_10cpu`, `_4cpu` and `_2cpu`, with the local orchestrator on branch `ateapi-cold-resume-capacity` at `6f6f44c1`. The first four ran back to back on 2026-10-15 from 22:26 to 01:38 UTC; `_2cpu` was relaunched alone on 2026-10-16, 01:42–02:14 UTC, after the first orchestrator stopped with the Claude Code session that ran it. Every runner Job completed; no request failed.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_<test>/run_date=2026-10-15/` (`_4cpu` and `_2cpu` under `run_date=2026-10-16/`); local copies and per-step analysis (`steps.jsonl`, from `benchmarking/automation/analyze_ladder.py`) in `results/<test>/`. Postgres's WAL fsync, commit and wait counters, sampled every 15 s, are in `results/wal-io.csv` and `results/wal-io-2cpu.csv`; `track_wal_io_timing` was off, so they hold counts, not times. Orchestrator logs: `~/bench-runs/ateapi-cold-5tests-size0-20261015/orchestrator.log` and `~/bench-runs/ateapi-cold-2cpu-size0-20261016/orchestrator.log`.
- **The `_10cpu` timeout is a reporting error.** The workstation suspended from 23:31 to 01:05 UTC. The `_10cpu` runner, in the cluster, finished its ladder at 23:48 and uploaded every file with `stats_generated: true`; the orchestrator, frozen, found its wait deadline passed when it woke and reported `timeout`. The WAL sampler, also on the workstation, has no samples for `_10cpu` past its first step.

| Test | Ladder (UTC) | run_ts | Local copy |
|---|---|---|---|
| `2x4cpu` | 10-15 22:32–22:53 | 1792103548 | `results/2x4cpu/` |
| `8cpu` | 10-15 23:02–23:20 | 1792105367 | `results/8cpu/` |
| `10cpu` | 10-15 23:30–23:48 | 1792107009 | `results/10cpu/` |
| `4cpu` | 10-16 01:12–01:33 | 1792113162 | `results/4cpu/` |
| `2cpu` | 10-16 01:49–02:10 | 1792115376 | `results/2cpu/` |

**Result: ate-api-server's CPU sets every ceiling, and they match the 2026-10-09 series rather than the 2026-10-14 runs.**

- **Ceilings:** about 790, 1530, 2633 and 2908 cycles/s at 2, 4, 8 and 10 CPUs, and 2560 at 2 × 4 CPUs. Each is within about 3% of the 2026-10-09 series; 2 and 4 CPU are 7% below the 2026-10-14 runs, and 8 CPU 2%.
- **A cycle costs about 7% more ate-api-server CPU than on 2026-10-14** at 2 and 4 CPUs, 2.50–2.63 ms against 2.34–2.47, with the same 24 commits and about the same Postgres CPU per cycle. Either the 22 upstream commits between `e68233a7` and `6f6f44c1` made a cycle dearer, or run-to-run variation is wider than the ±1% measured before; these runs cannot tell which.
- **The 8 CPU run is the baseline the data-plane delay runs lacked:** at the same commit, 500 ms and 1 s delays lower the 8 CPU ceiling 3.5% and 5.2% from 2633/s (`../ateapi-cold-8cpu-delay-20261015/summary.md` compared against the 2026-10-14 run at an earlier commit).

## What differs from the earlier runs

- **Against the 2026-10-09 series** (`../ateapi-cold-2-4-8cpu-20261009/summary.md`): the workload is `resumepaused`, each user owning one actor, instead of `resumecold`'s shared fleet; a cycle costs 24 Postgres commits instead of 25; the code is based on upstream main of 2026-10-14; and the fake data plane is installed by `ate-setup --fake-data-plane`.
- **Against the 2026-10-14 runs** (`../ateapi-cold-6tests-20261014/summary.md`): the same workload and test entries; the code is `6f6f44c1` instead of `e68233a7`, with 22 more upstream commits, among them #2620 (a paused actor is defined by its snapshots; the PAUSED and PAUSING states are gone) and #2536 (snapshots stamped with SandboxRuntime; restores scheduled on runtime-compatible workers).

## Setup

- Isolate mode: fake-atelet on every atelet node answers each call after 150 ms and mints the actor certificate on each restore; 200 fake Workers of 1000 actors each.
- Users and actors: 1000 for 2 CPU, 2000 for 4 CPU, 3000 for 8 CPU, 4000 for 10 CPU and 2 × 4 CPU.
- Ladders of 3-minute steps: 2 CPU 100–1500/s, 4 CPU 250–2500/s, 8 CPU 2400–2900/s in steps of 100, 10 CPU 2400–3400/s in steps of 200, 2 × 4 CPU 500–4000/s.
- ate-api-server: CPU request = limit = the test's CPUs per replica, 4Gi, `GOMAXPROCS` from the limit; read/write pool 64 with one replica, 40 per replica with two.
- Postgres: size0 (2 / 16 CPU, 1Gi / 2Gi, `max_connections = 100`, `shared_buffers = 128MB`), with `pg_stat_statements` and the postgres_exporter sidecar.
- Placement: Postgres, ate-api-server and the runner each on their own node. ate-api-server on `…-4d75` except at 4 CPU (`…-9v65`); Postgres on `…-9v65` for 2 × 4 and 8 CPU, `…-d1sf` for 10 and 2 CPU, `…-4d75` for 4 CPU.

## Result

| Test | Ceiling (cycles/s) | 2026-10-14 | 2026-10-09 to -12 | ate-api-server at the ceiling | ateapi CPU per cycle, now / 10-14 / 10-09 | Postgres CPU per cycle, now / 10-14 | Commits per cycle |
|---|---|---|---|---|---|---|---|
| 2 CPU | about 790 (785–799) | about 850 (848–855) | about 815 (808–818) | 2.00 of 2, throttled 26–30% | 2.50–2.54 / 2.34–2.36 / 2.44–2.47 ms | 2.82–2.84 / 2.77–2.78 ms | 24.1 |
| 4 CPU | about 1530 (1517–1536) | about 1645 (1611–1648) | about 1540 (1522–1562) | 3.98 of 4, throttled 32–35% | 2.59–2.63 / 2.41–2.47 / 2.53–2.58 ms | 2.62–2.80 / 2.59–2.77 ms | 24.0 |
| 8 CPU | about 2633 (2631–2637) | about 2690 (2679–2696) | about 2610; repeat 2627 | 7.82–7.84 of 8, throttled 13–16% | 2.97–2.98 / 2.90–2.92 / 2.96–2.99 ms | 2.81–2.82 / 2.86–2.88 ms | 24.0 |
| 10 CPU | about 2908 (2904–2912) | | about 2900 (2880–2949) | 9.38–9.45 of 10, unthrottled | 3.22–3.25 / — / 3.13–3.24 ms | 2.86–2.87 / — | 24.0 |
| 2 × 4 CPU | about 2560 (2547–2574) | | about 2600 (2589–2613) | one replica at 3.91–3.96 of 4, the other at 3.67–3.79; throttled 15–18% | 2.99 / — / 2.93–2.94 ms | 2.71–2.90 / — | 24.0 |

The 2026-10-14 8 CPU figures are its two saturated steps (2800 and 2900/s); its 2700/s step held at 2696/s.

**ate-api-server's CPU sets every ceiling.** Each single replica sits at its CPU limit in every saturated step, and at 2 × 4 one replica does. Postgres used 2.2–8.4 of its 16 cores, lock waits stayed at 0–2.5 connections, and 69–70 of Postgres's 100 connections were open with one replica, 88–90 with two. At 10 CPUs ate-api-server reached 9.4 of its 10 cores unthrottled, 94%, as on 2026-10-09: mostly CPU-bound, as that series found.

**The ceilings follow ate-api-server's CPU per cycle.** Its limit divided by its cost per cycle gives each ceiling within 2%: 2.00 ÷ 2.52 ms = 794/s at 2 CPU, 3.98 ÷ 2.61 ms = 1525/s at 4 CPU, 7.83 ÷ 2.97 ms = 2636/s at 8 CPU. So the 7% gap to 2026-10-14 at 2 and 4 CPUs is the 7% higher CPU per cycle. At 8 CPUs the cost is 2% higher and so is the gap. Against 2026-10-09, cost and ceilings agree within 3% at every size, despite the different workload and code.

**Why the cost differs from 2026-10-14 is not shown.** Commits per cycle are the same 24, and Postgres's CPU per cycle within 2%, so the extra cost is ate-api-server's own. The two runs differ in code by 22 upstream commits; they also ran on different days, and the 2026-10-14 2 and 4 CPU results were themselves repeated within 1.5% that day (at `217f0d8c`), so if this is variation it varies between days rather than within one.

**The data-plane delay, against this baseline.** The 8 CPU tests with 500 ms and 1 s delays ran at `a1c6acd0`, between `e68233a7` and `6f6f44c1`; against this run's 2633/s at 150 ms, their 2540 and 2495/s are 3.5% and 5.2% lower, rather than the 5.6% and 7.2% their summary computed against the 2026-10-14 2690/s. `a1c6acd0` and `6f6f44c1` differ only in two committed summaries.

**Well below each ceiling** a resume takes the fake's 150 ms plus 6–8 ms; just below it, 164–200 ms. Past it, latency rises until it absorbs the users: about 660 ms at 2 CPU, 690–700 ms at 4, 590–600 ms at 8, 690–720 ms at 10 and 825–837 ms at 2 × 4.

## WAL counters

With `track_wal_io_timing` off, the sampler counted fsyncs without timing them. Where it ran, fsyncs held at about 2,000–2,300/s at every step but the very lowest (about 1,000/s at 2 CPU's 100/s), and commits per fsync rose with the rate, from 2–5 at the lowest steps to 9–10 at the 2 CPU ceiling, 18–19 at 4 CPU, 29–30 at 8 CPU and 28–29 at 2 × 4 CPU: the same back-to-back flush as in the timed runs, carrying fewer commits each because these tests keep fewer connections active than the multi-replica ones. The flush is not what stops these tests: ate-api-server's CPU runs out first.

## Per step

Columns as in `analyze_ladder.py`, over each step's window (30 s after it starts to 5 s before it ends). Server p50 / p90 / p95 / p99 are quantiles of ate-api-server's resume handler time, interpolated within doubling buckets; the means are precise. fsyncs/s and commits per fsync are from the sampler over the same window, where it ran.

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

## Memory

Peak over each step's window, from cAdvisor and postgres-exporter; per step in `steps.jsonl`. Limits: ate-api-server 4Gi per replica, Postgres 2Gi.

| Test | ate-api-server per replica, peak: steps that held / saturated steps | Postgres working set, first → highest step | Postgres RSS, peak | Buffer cache hit rate, lowest step | Blocks read/s, highest step |
|---|---|---|---|---|---|
| 2 CPU | 98 / 120 MiB | 247 → 432 MiB | 186 MiB | 99.995% | 5 |
| 4 CPU | 127 / 184 MiB | 285 → 466 MiB | 184 MiB | 99.943% | 122 |
| 8 CPU | 178 / 249 MiB | 434 → 510 MiB | 185 MiB | 99.888% | 413 |
| 10 CPU | 199 / 310 MiB | 442 → 545 MiB | 186 MiB | 99.803% | 809 |
| 2 × 4 CPU | 103 / 252 MiB | 408 → 578 MiB | 237 MiB | 99.898% | 378 |

Memory limited nothing: at most 310 MiB per ate-api-server replica and 578 MiB for Postgres, memory pressure stalls under 0.01% of the time. As on 2026-10-09, ate-api-server's memory rises once calls queue past the ceiling, and Postgres's buffer cache misses more as the rate rises.

## Confidence and limits

**Solid**

- ate-api-server's CPU as the limit at every size: each single replica at its CPU limit in every saturated step, and the ceilings follow its CPU per cycle within 2%.
- 24 Postgres commits per cycle at every step.
- No request failed in any test.

**Directional**

- Each ceiling: one run at this commit. The 2026-10-09 series repeated within 1–2% at 2, 4 and 8 CPUs, and the 2026-10-14 2 and 4 CPU runs within 1.5% the same day.
- The 7% higher ate-api-server CPU per cycle than on 2026-10-14 at 2 and 4 CPUs: consistent across both sizes and the saturated steps, but one run each; whether the code or day-to-day variation causes it is not shown.
- The delay's effect at 8 CPUs, 3.5% at 500 ms and 5.2% at 1 s: one run each, at commits whose code is the same.

**Not conclusive**

- Server quantiles: interpolated within doubling buckets; the means are precise.

**Scope**

- Isolate mode: a fixed 150 ms per call, pause rather than suspend, no router traffic.
- size0 Postgres with default tuning; c3-highcpu-88 nodes.

## To make it conclusive

1. Run `_2cpu` and `_4cpu` at `e68233a7` and at `6f6f44c1` back to back on one day, to tell whether the code raised ate-api-server's CPU per cycle.
2. If it did, a Go CPU profile of ate-api-server at the 4 CPU ceiling at both commits, to find where.
3. Keep the workstation awake through a series (`systemd-inhibit --what=sleep`), so the orchestrator's verdicts and the WAL sampler cover every test.

## How results are measured

`analyze_ladder.py` measures each step from 30 s after it starts to 5 s before it ends, 145 s of each 3-minute step, against the benchmarking Prometheus while it still held the runs. The WAL columns diff the first and last sampler rows inside the same window.
