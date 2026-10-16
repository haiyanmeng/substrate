# ate-api-server cold resume capacity, 2 CPUs to 3 × 8 CPUs, 2026-10-14

- Tests `ateapi_cold_resume_pause_fake_2cpu`, `_4cpu`, `_8cpu`, `_2x8cpu`, `_2x8cpu_size10` and `_3x8cpu_size10`, run back to back with the local orchestrator on branch `fake-dataplane-test-1` at `e68233a7`, 13:46–16:53 UTC. Every test complete, no request failed.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_<test>/run_date=2026-10-14/`; local copies and per-step analysis (`steps.jsonl`) in `results/<test>/`. Postgres's WAL, commit and wait counters, sampled every 15 s across the whole series, are in `results/wal-io.csv`. Orchestrator log: `~/bench-runs/ateapi-cold-6tests-20261014/orchestrator.log`.
- An earlier run the same day at `217f0d8c` completed 2cpu and 4cpu and was then stopped so the series could restart with the Postgres exporter fix (below). Its results are in `results/repeat-217f0d8c/` and serve as same-day repeats.
- A repeat of `2x8cpu` on 2026-10-15, 20:23–20:52 UTC, on branch `ateapi-cold-resume-capacity` at `6f6f44c1`, with the test entry plus `track_wal_io_timing = on` (added to `tests.yaml` for it) so the size0 WAL flush is timed: complete, no request failed. Results in `results/repeat-20261015/2x8cpu/`, WAL samples in `results/repeat-20261015/wal-io.csv`, log in `~/bench-runs/ateapi-cold-2x8cpu-waltiming-20261015/orchestrator.log`.

| Test | Steps (UTC) | run_ts | Local copy |
|---|---|---|---|
| `2cpu` | 13:56–14:16 | 1791986148 | `results/2cpu/` |
| `4cpu` | 14:26–14:47 | 1791987973 | `results/4cpu/` |
| `8cpu` | 14:57–15:14 | 1791989799 | `results/8cpu/` |
| `2x8cpu` | 15:25–15:43 | 1791991513 | `results/2x8cpu/` |
| `2x8cpu_size10` | 15:54–16:12 | 1791993251 | `results/2x8cpu_size10/` |
| `3x8cpu_size10` | 16:24–16:47 | 1791995026 | `results/3x8cpu_size10/` |
| `2cpu`, repeat at `217f0d8c` | 12:48–13:09 | 1791982082 | `results/repeat-217f0d8c/2cpu/` |
| `4cpu`, repeat at `217f0d8c` | 13:18–13:38 | 1791983870 | `results/repeat-217f0d8c/4cpu/` |
| `2x8cpu`, repeat on 2026-10-15 at `6f6f44c1`, WAL timing on | 20:30–20:48 | 1792096192 | `results/repeat-20261015/2x8cpu/` |

## What changed since the earlier runs

The earlier runs are in `../ateapi-cold-2-4-8cpu-20261009/` (`summary.md` for size0, `summary-size10-replicas.md` for size10). Three things differ:

- **Workload.** The test entries now run the `resumepaused` user class: each user owns one actor, creates it and makes it cold in the step that adds the user, then resumes and pauses it again every iteration. The earlier runs used `resumecold`, where users took paused actors from a shared fleet and returned them. One cycle is still one resume plus one pause.
- **Code.** `e68233a7` is the benchmark branch rebased on upstream main as of 2026-10-14. That brings in, among others, the default-deny ingress for ate-system, the asynchronous atelet Snapshot API (`PromoteSnapshot` replacing `UploadPausedCheckpoint`) and `ReportActorSnapshotState`.
- **Fake data plane install.** The fake data plane is now installed by `ate-setup --fake-data-plane`, upstream's version of the earlier branch commit.

These results cannot separate the effect of the workload from that of the code.

Two automation fixes were needed under ate-system's default-deny ingress, both on the branch:

- `217f0d8c` labels the runner's `benchmarking` namespace `ate.dev/api-access=true`. Without it the boomer worker could not dial ate-api-server, and a first attempt's 2cpu runner timed out with no requests.
- `e68233a7` names the postgres_exporter sidecar's port `metrics`, a port name the observability NetworkPolicy admits. Without it Prometheus timed out on every scrape and the `217f0d8c` runs recorded no Postgres exporter metrics; their commit rates come from the WAL sampler instead.

## Setup

- Isolate mode (fake data plane): fake-atelet on every atelet node answers each call after 150 ms and mints the actor certificate on each restore; 200 fake Workers of 1000 actors each, so placement never binds.
- Users and actors: 1000 for 2 CPU, 2000 for 4 CPU, 3000 for 8 CPU, 6000 for 2 × 8 CPU (both Postgres sizes) and 8000 for 3 × 8 CPU.
- Ladders of 3-minute steps: 2 CPU 100–1500/s, 4 CPU 250–2500/s, 8 CPU 2400–2900/s in steps of 100, 2 × 8 CPU 2500–5000/s in steps of 500, 3 × 8 CPU 3000–6500/s in steps of 500.
- ate-api-server: CPU request = limit = the test's CPUs per replica, 4Gi memory, `GOMAXPROCS` from the limit. Read/write pool 64 per replica with one replica, 40 with two or three.
- Postgres: size0 for the first four tests, as in `summary.md`. size10 (`--cluster-size=size10`) with `track_wal_io_timing = on` for the last two, as in `summary-size10-replicas.md`.

## Result

| Test | Ceiling (cycles/s) | Earlier | Change | Bound by |
|---|---|---|---|---|
| 2cpu | about 850 (848–855); repeat 852–861 | about 815 (808–818) | +4% | ate-api-server CPU: 2.00 of 2 cores, throttled in 22–26% of periods |
| 4cpu | about 1645 (1641–1648); repeat 1631–1655 | about 1540 (1522–1562) | +7% | ate-api-server CPU: 3.98 of 4 cores, throttled 34–39% |
| 8cpu | about 2690 (2679–2696) | about 2610 (2598–2615); repeat 2627 | +2–3% | ate-api-server CPU: 7.8 of 8 cores, throttled 9% |
| 2 × 8, size0 | about 3850 (3842–3858); repeat on 10-15 about 3630 (3626–3645) | about 3670 (3648–3674); repeat 3735 | +3–5%; repeat −1% | Postgres's WAL flush, timed in the 10-15 repeat: commits level off at about 92,500/s (87,000 in the repeat) with 29–37 connections waiting on `WALWrite`; ate-api-server at 13.5 of 16 cores, unthrottled |
| 2 × 8, size10 | about 3745 (3737–3757) | about 3680 (3652–3706) | +2% | Postgres's commit path: about 90,000 commits/s, the WAL flush busy 95% of the time; ate-api-server at 13.4 of 16 cores |
| 3 × 8, size10 | about 4960 (4931–4980) | about 4930 (4898–4948) | +1% | Postgres's commit path: about 119,000 commits/s, the WAL flush busy 94–95% of the time; ate-api-server at 19.4–19.9 of 24 cores |

Every ceiling rose, most where ate-api-server's CPU is the limit (2–7%), least where Postgres's commit path is (1–5%). That fits a cheaper cycle:

| Test | ate-api-server CPU per cycle at the ceiling, earlier → now | Postgres commits per cycle, earlier → now |
|---|---|---|
| 2cpu | 2.44–2.47 → 2.34–2.36 ms | 25.0 → 24.0 |
| 4cpu | 2.53 → 2.41–2.47 ms | 25.0 → 24.0 |
| 2 × 8, size10 | 3.67 → 3.44–3.58 ms | 25.0 → 24.0 |
| 3 × 8, size10 | 4.10–4.12 → 3.90–4.01 ms | 25.0 → 24.0 |

Postgres's CPU per cycle fell too, from 2.89–2.90 ms to 2.77–2.79 ms at the 2 CPU ceiling. With the same Postgres commit cap, one commit fewer per cycle is worth about 4% more cycles, close to the gain at 2 × 8 and 3 × 8.

Well below each ceiling a resume takes 157–175 ms, 150 ms of it fake-atelet. Past it, calls queue at the server and the mean rises to 590–650 ms with one replica and 810–860 ms with two or three. 3 × 8 still met its 5000/s step, at a 272 ms mean.

**Repeats.** The `217f0d8c` 2cpu and 4cpu runs repeat these within 1.5%, with the same CPU per cycle (2.32–2.35 ms at 2 CPU, 2.40–2.48 ms at 4 CPU). The two commits differ only in the Postgres exporter fix. The 2026-10-15 repeat of 2 × 8 on size0 reached about 3630/s, 6% below this run; see Postgres's commit path for why.

## Postgres's commit path

With WAL timing on (the size10 tests), the flush runs back to back at every step: about 1,800–1,970 fsyncs/s at 0.48–0.53 ms each, busy 93–95% of the time. Throughput is then fsyncs/s times the commits each fsync carries, and that stops growing at the ceiling:

| Test | fsyncs/s at the ceiling | fsync time | Commits per fsync | Connections waiting on `WALWrite` |
|---|---|---|---|---|
| 2 × 8, size0 | 1,871–1,888 | not timed | 49 | 29–37 |
| 2 × 8, size0, repeat on 10-15 | 1,735–1,764 | 0.54–0.55 ms | 50 | 24–31 |
| 2 × 8, size10 | 1,785–1,835 | 0.51–0.53 ms | 49–50 | 30–38 |
| 3 × 8, size10 | 1,789–1,814 | 0.52–0.53 ms | 66–67 | 48–56 |

These match the earlier runs: about 50 commits per fsync at 2 × 8 and fsyncs of about 0.5 ms at two and three replicas (`summary-size10-replicas.md`). On size0 the fsync rate is about the same, 1,970–2,150/s at every step of the single-replica tests, so size10's larger resources do not raise the cap, as `summary-2x8-postgres.md` found. Earlier the two 2 × 8 ceilings were the same, about 3670 and 3680; this time size0 finished about 3% ahead, within the run-to-run variation `summary-size10-replicas.md` reports when fsync latency differs. Lock waits stayed at 1–2 connections at most steps, up to 3.4 at 3 × 8.

**The 2 × 8 size0 repeat, with the flush timed.** The 2026-10-15 repeat turned on `track_wal_io_timing` for size0, which this run did not have:

| Target | Resume/s | ate-api-server cores (per replica) | ate-api-server CPU per cycle | Postgres cores | Commits/s | fsyncs/s | Mean fsync | Flush busy | WAL write busy | Commits per fsync | `WALWrite` waits | Lock waits |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 2500 | 2500 | 9.5 (4.76 / 4.71) | 3.79 ms | 6.6 | 59,929 | 1,864 | 0.51 ms | 95% | 3% | 32 | 22 | 1.0 |
| 3000 | 3000 | 11.1 (5.54 / 5.52) | 3.69 ms | 8.4 | 72,079 | 1,830 | 0.52 ms | 95% | 3% | 40 | 23 | 2.0 |
| 3500 | 3501 | 12.3 (6.33 / 5.95) | 3.51 ms | 10.3 | 84,049 | 1,766 | 0.53 ms | 94% | 3% | 47 | 34 | 1.4 |
| 4000 | 3645 | 13.5 (6.96 / 6.54) | 3.70 ms | 11.1 | 87,576 | 1,764 | 0.54 ms | 95% | 3% | 50 | 31 | 1.2 |
| 4500 | 3626 | 13.5 (6.95 / 6.53) | 3.72 ms | 11.0 | 86,930 | 1,735 | 0.55 ms | 95% | 3% | 50 | 27 | 1.4 |
| 5000 | 3626 | 13.5 (6.93 / 6.53) | 3.71 ms | 11.1 | 87,134 | 1,737 | 0.55 ms | 95% | 3% | 50 | 24 | 2.0 |

- The fsync runs back to back at every step, busy 94–95% of the time even at 2500/s, while writing the WAL takes 3%. On size0, as on size10, the limit is the fsync itself, not the rest of the commit path.
- Commits per fsync rise with load, 32 to 50, and stop at 50 at the ceiling, as in this run (49). A flush covers only the commits already waiting, and two replicas keep about 40–46 connections active: the cap is how many commits ate-api-server keeps in flight, as `summary-2x8-postgres.md` inferred.
- The repeat's lower ceiling is its fsync rate: the same 50 commits per fsync at 1,735–1,764 fsyncs/s instead of 1,871–1,888, 7% fewer, gives 87,000 commits/s instead of 92,500. Its fsyncs took 0.54–0.55 ms; this run's, untimed, would have been about 0.50 ms at the same 95% busy.
- ate-api-server used the same 13.5 of its 16 cores at a lower rate, so its CPU per cycle at the ceiling was about 6% higher, 3.70–3.72 ms against 3.49–3.50 ms here. It is not the limit in either run. The repeat ran at a later commit (`6f6f44c1`, with more upstream changes than `e68233a7`), so the difference may be the code or run-to-run.
- Placement: ate-api-server on `…-4d75`, Postgres on `…-9v65`.

