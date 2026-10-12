# ate-api-server replica scaling on size10 Postgres: 2 to 8 × 8 CPUs, 2026-10-11

On size0 Postgres, ate-api-server stopped scaling past two replicas: 4 × 4 CPUs did worse than 2 × 8 (`summary.md`). On size10, 2 × 8 behaved as on size0, about 3680 cycles/s (`summary-2x8-postgres.md`). These runs add 8 CPU replicas one at a time, from 3 to 8, against size10 Postgres, with the same workload as `summary.md`.

**Result: the ceiling rises with each replica up to 7, by less each time, and stops there.**
- **Ceilings:** about 3680, 4930, 5930, 6750, 7340, 8200 and 8150 cycles/s from 2 to 8 replicas. 7 × 8 and 8 × 8 are the same within 1%, at about 205,000 commits/s.
- **Postgres's WAL flush sets the cap.** More replicas let each fsync carry more commits, about 50 at 2 × 8 and 150 at 6 × 8, but each fsync takes longer as replicas are added, from 0.52 to 0.77 ms, so commits per second level off. Why fsyncs slow down is open: not because each writes more.
- **The fake's 150 ms delay is not what limits it.** With fake-atelet answering at once, 7 × 8 reached the same ceiling, about 8100 cycles/s, and 8 × 8 6% less, about 7680.
- **ate-api-server's CPU does not:** its share in use falls from 85% at 2 × 8 to 71% at 8 × 8.
- **Each cycle costs more ate-api-server CPU** as replicas are added: 3.67 ms at 2 × 8, 5.6 ms at 8 × 8, 53% more.

## Runs

Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 for these tests), the local orchestrator, branch `fake-dataplane-test`. Every runner Job completed and uploaded its results with no request failed.

**3 × 8 and 4 × 8**
- **Tests:** `ateapi_cold_resume_pause_fake_3x8cpu_size10` and `_4x8cpu_size10`, run back to back at `3d02ace1`, which adds both. They made 12,459,977 and 17,532,668 requests.
- **The 3 × 8 `timeout` is a reporting error.** The orchestrator reports `timeout` for 3 × 8 and exits 1, but that runner finished and uploaded every file by about 13:46 UTC, 28 minutes after its Job was submitted. The orchestrator's `kubectl get job` did not see the Job complete before its 56-minute limit, most likely because the call hung as in the credential outages of `summary-2x8-postgres.md`; `wait_for_job` logs nothing while a call hangs.

| Test | Ladder (UTC) | run_ts | Local copy |
|---|---|---|---|
| `3x8cpu_size10` | 2026-10-11 13:19–13:43 | 1791724740 | `results/3x8cpu-size10/` |
| `4x8cpu_size10` | 2026-10-11 14:29–14:56 | 1791728936 | `results/4x8cpu-size10/` |
| `5x8cpu_size10` | 2026-10-11 20:24–20:54 | 1791750251 | `results/5x8cpu-size10/` |
| `6x8cpu_size10` | 2026-10-11 21:04–21:37 | 1791752627 | `results/6x8cpu-size10/` |
| `7x8cpu_size10` | 2026-10-11 15:34–16:07 | 1791732857 | `results/7x8cpu-size10/` |
| `8x8cpu_size10` | 2026-10-11 19:38–20:14 | 1791747509 | `results/8x8cpu-size10/` |

- **Results:** `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_{3x8,4x8}cpu_size10/run_date=2026-10-11/`.
- **WAL samples:** taken every 15 s from `pg_stat_io`, `pg_stat_wal`, `pg_stat_database` and `pg_stat_activity`, in `results/4x8cpu-size10/wal-io-3x8-and-4x8.csv`. They cover the 3 × 8 run's steps through 4000/s and the 4 × 8 run's every step.
- **Log:** `~/bench-runs/ateapi-cold-3x8-4x8-size10-20261011/orchestrator.log`.

**5 × 8 to 8 × 8**
- **Tests:** `ateapi_cold_resume_pause_fake_5x8cpu_size10` to `_8x8cpu_size10`, at `f50a785a`, which adds the bounded `wait_for_job` to the orchestrator (`3193cbe5` and `a3701fce` add the tests). They made 22,080,864, 26,615,130, 28,057,512 and 31,034,963 requests.
- **First attempt** (`~/bench-runs/ateapi-cold-5to8x8-size10-20261011/`):
  - 5 × 8 and 6 × 8 failed to install, the GKE API server rejecting requests with `ResourceExhausted … ENHANCE_YOUR_CALM`.
  - 7 × 8 then ran normally.
  - The workstation running the orchestrator suspended from 15:48 to 19:31 UTC, during and after 7 × 8. The runner works inside the cluster, so the benchmark was unaffected, but the orchestrator and the WAL sampler stopped.
  - After the workstation woke, the orchestrator submitted 8 × 8 and then ended with the Claude Code session that ran it, about 19:38 UTC. 8 × 8's runner Job ran on without it.
- **Reruns** of 5 × 8 and 6 × 8 (`~/bench-runs/ateapi-cold-5x8-6x8-size10-rerun-20261011/`) waited for 8 × 8's Job to finish, then ran cleanly: both `complete`, orchestrator exit 0. The first rerun also tore down 8 × 8's install.
- **Results:** `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_{5x8,6x8,7x8,8x8}cpu_size10/run_date=2026-10-11/`.
- **WAL samples:** for all four in `results/8x8cpu-size10/wal-io-5to8x8.csv`. They cover every step of 5 × 8 and 6 × 8, none of 7 × 8, and the last step of 8 × 8.

**7 × 8 and 8 × 8 with no fake delay**
- **Tests:** `ateapi_cold_resume_pause_fake_7x8cpu_size10_nodelay` and `_8x8cpu_size10_nodelay`, the 7 × 8 and 8 × 8 tests with `fakeDataPlane.delay: "0s"` and nothing else changed. Run back to back at `1b6006b0`, which adds them, on 2026-10-12, 00:10–01:40 UTC. Both complete with no request failed (30,066,177 and 31,822,116 requests), orchestrator exit 0.
- **Ladders:** 7 × 8 ran 6000–11000/s from 00:19 to 00:52 (run_ts 1791764343), and 8 × 8 ran 6000–11500/s from 01:00 to 01:37 (run_ts 1791766842). Local copies are in `results/7x8cpu-size10-nodelay/` and `results/8x8cpu-size10-nodelay/`.
- **WAL samples:** for every step of both, in `results/8x8cpu-size10-nodelay/wal-io-7x8-and-8x8-nodelay.csv`.
- **Log:** `~/bench-runs/ateapi-cold-7x8-8x8-nodelay-size10-20261011/orchestrator.log`.

**Other**
- **2 × 8 on size10:** `ateapi_cold_resume_pause_fake_2x8cpu_size10`, run_ts 1791671486 at `2a04ee26`, from `summary-2x8-postgres.md`. `2a04ee26`, `3d02ace1` and `f50a785a` differ only in benchmark test definitions and automation, not in ate-api-server code.

## Setup

- **Data plane:** isolate mode (fake data plane), 3 fake-atelets answering each call after 150 ms, 200 fake Workers of 1000 actors each, so placement never binds.
- **Load:** resumecold, one cycle being one resume plus one pause, on ladders of 3-minute steps of 500.
- **Postgres:** size10 (`--cluster-size=size10`: 80 CPU requested, 140Gi, `shared_buffers = 35GB`, `max_connections = 300`, size10's WAL settings; `summary-2x8-postgres.md`), on a 500Gi pd-balanced volume, with `track_wal_io_timing = on` through `postgresArgs`.
- **Placement:** each test installed substrate fresh. Postgres, ate-api-server and the runner each had their own node; fake-atelet ran on all three. Every ate-api-server replica shared the ateapi node.

| | 2 × 8 | 3 × 8 | 4 × 8 | 5 × 8 | 6 × 8 | 7 × 8 | 8 × 8 |
|---|---|---|---|---|---|---|---|
| Total ate-api-server CPUs | 16 | 24 | 32 | 40 | 48 | 56 | 64 |
| Read/write pool per replica | 40 | 40 | 40 | 40 | 40 | 35 | 30 |
| Postgres connections, pools plus 5 owner and watch per replica | 90 | 135 | 180 | 225 | 270 | 280 | 280 |
| Users and actors | 6000 | 8000 | 10,000 | 12,000 | 14,000 | 16,000 | 18,000 |
| Ladder | 2500–5000/s | 3000–6500/s | 4000–8000/s | 4500–9000/s | 5000–10000/s | 5000–10000/s | 5000–10500/s |
| Runner CPU / memory requests | 8 / 2Gi | 12 / 3Gi | 16 / 4Gi | 16 / 5Gi | 16 / 6Gi | 16 / 6Gi | 16 / 7Gi |

Every replica has 8 / 8 CPU (request = limit), 4Gi and `GOMAXPROCS` 8. At 7 and 8 replicas, pools of 40 would need 315 and 360 of size10's 300 connections, so they are 35 and 30.

## Result

| ate-api-server | CPUs | Ceiling (cycles/s) | Against 2 × 8 | ate-api-server cores used | Busiest replica / the others | ate-api-server CPU per cycle | Postgres cores | Commits/s |
|---|---|---|---|---|---|---|---|---|
| 2 × 8 | 16 | about 3680 (3652–3706) | 1.00× | 13.4–13.6 (85%) | 6.96–7.04 / 6.44–6.54 | 3.67 ms | 11.8–12.0 | about 92,000 |
| 3 × 8 | 24 | about 4930 (4898–4948) | 1.34× for 1.5× the CPU | 20.1–20.3 (84%) | 7.05–7.14 / 6.53–6.68 | 4.10–4.12 ms | 17.7–18.0 | about 123,000 |
| 4 × 8 | 32 | about 5930 (5889–5970) | 1.61× for 2× the CPU | 26.4–26.8 (83%) | 7.04–7.13 / 6.43–6.56 | 4.47–4.50 ms | 22.9–23.0 | about 148,000 |
| 5 × 8 | 40 | about 6750 (6707–6849) | 1.83× for 2.5× the CPU | 32.0–32.9 (80–82%) | 6.82–6.93 / 5.96–6.91 | 4.75–4.90 ms | 27.1–27.8 | about 168,000–171,000 |
| 6 × 8 | 48 | about 7340 (7312–7447) | 1.99× for 3× the CPU | 36.5–37.3 (76–78%) | 6.50–6.54 / 5.88–6.24 | 4.98–5.01 ms | 29.2–31.8 | about 183,000 |
| 7 × 8 | 56 | about 8200 (8184–8220) | 2.23× for 3.5× the CPU | 42.4–43.1 (76–77%) | 6.35–6.42 / 5.61–6.35 | 5.18–5.24 ms | 34.0–37.0 | about 205,000 |
| 8 × 8 | 64 | about 8150 (8105–8216) | 2.21× for 4× the CPU | 45.1–46.2 (70–72%) | 5.91–5.94 / 5.19–5.92 | 5.56–5.66 ms | 33.4–36.7 | about 203,000–206,000 |

The 7 × 8 run's last step fell to 8002/s; the range above is of its three steps before that.

For reference, the 1M-actor 10 × 8 CPU run on size10 (`summary-size10.md`, at `5af49ce9`) reached about 6920 cycles/s with 80 CPUs, about 7.0 ms of ate-api-server CPU per cycle and about 135 commits per fsync. Its workload differs (1,000,000 actors, 1000 Workers, no fake delay, 4000 users), so it is not a point on this curve.

**The WAL flush:**

| ate-api-server | Step | fsyncs/s | Mean fsync | Flush busy | Commits per fsync | Active PG conns | Waiting on `WALWrite` | Waiting on a lock |
|---|---|---|---|---|---|---|---|---|
| 2 × 8 | 4000/s (ceiling) | 1,800 | 0.52 ms | 97% | 50 | 46 | 29 | 0.4 |
| 3 × 8 | 3000/s | 1,889 | 0.50 ms | 98% | 40 | 38 | 25 | 0.1 |
| 3 × 8 | 4000/s | 1,743 | 0.54 ms | 97% | 57 | 55 | 33 | 0.7 |
| 3 × 8 | 5000–6500/s (ceiling) | not sampled | | | | 61–67 | 37–44 | |
| 4 × 8 | 4000/s | 1,836 | 0.51 ms | 97% | 55 | 47 | 26 | 0.6 |
| 4 × 8 | 6000/s | 1,724 | 0.55 ms | 97% | 87 | 68 | 38 | 3.0 |
| 4 × 8 | 6500–8000/s (ceiling) | 1,691–1,768 | 0.54–0.56 ms | 96–99% | 85–88 | 75–93 | 41–58 | 1.5–3.9 |
| 5 × 8 | 5000/s | 1,805 | 0.51 ms | 97% | 69 | 72 | 47 | 0.7 |
| 5 × 8 | 6500/s | 1,608 | 0.59 ms | 98% | 101 | 100 | 57 | 2.1 |
| 5 × 8 | 7000–9000/s (ceiling) | 1,375–1,463 | 0.64–0.68 ms | 97–98% | 118–122 | 105–128 | 57–78 | 4.3–6.2 |
| 6 × 8 | 5000/s | 1,642 | 0.57 ms | 97% | 76 | 81 | 51 | 2.3 |
| 6 × 8 | 7000/s | 1,327 | 0.71 ms | 97% | 132 | 141 | 102 | 6.4 |
| 6 × 8 | 7500–10000/s (ceiling) | 1,209–1,235 | 0.76–0.78 ms | 96–97% | 149–154 | 138–155 | 77–89 | 6.9–10.3 |
| 7 × 8 | 8500–10000/s (ceiling) | not sampled | | | | 140–148 | 65–78 | |
| 8 × 8 | 10500/s (ceiling) | 1,415 | 0.65 ms | 96% | 143 | 145 | 79 | 8.6 |

**More replicas make each flush carry more commits, until larger flushes slow down enough to cancel it.**
- The WAL flush runs back to back at every step of every run, fsyncs taking 96–99% of wall time. Throughput is therefore fsyncs/s times the commits each fsync covers.
- More replicas keep more Postgres connections active, from 39–48 at 2 × 8 to 138–167 at 6–8 × 8, and so more commits waiting at each flush: about 50 per fsync at 2 × 8, 86 at 4 × 8, 120 at 5 × 8 and 150 at 6 × 8.
- This is the mechanism `summary-2x8-postgres.md` inferred from 2 × 8 alone; the series shows it directly.
- But each fsync takes longer as replicas are added: 0.52 ms at 50 commits, 0.55 ms at 86, 0.66 ms at 120 and 0.77 ms at 150. Fsyncs/s fall from about 1,800 to about 1,220.
- The size of each fsync does not explain the slowdown. In the no-delay runs, where the sampler also recorded WAL bytes, 7 × 8's fsyncs of about 35 KB took 0.60–0.64 ms, 8 × 8's of about 40 KB took 0.77–0.79 ms, and 8 × 8's of 64 KB at 7000/s took 0.75 ms. What slows them is not measured. More backends contending for each flush, with more replicas and more active connections, is a candidate.
- Commits per second therefore rise less with each replica, about 92,000, 148,000, 169,000 and 183,000 from 2 to 6 replicas, and stop at about 205,000 at 7 and 8 replicas: 8200 cycles/s at 25 commits per cycle.
- The 8 × 8 run's one sampled step had 143 commits per fsync at 0.65 ms, a smaller and faster batch than 6 × 8's. It used pools of 30, and one sample is too few to say more.

**Each cycle costs more ate-api-server CPU as replicas are added.**
- The cost per cycle rose from 3.67 ms at 2 × 8 to 4.1 ms at 3 × 8 and 4.5 ms at 4 × 8, 22% more, at every step, not only at the ceiling. That is most of the gap between 2× the CPU and 1.61× the throughput.
- `summary.md` found this cost rising with the total cycle rate, through contention shared across the system, and with each replica's own background work (Worker cache, template and assignment reconcilers, the outbox watch poll).
- At 5 to 8 replicas it kept rising, to 4.8, 5.0, 5.2 and 5.6 ms, 53% above 2 × 8. At the same 5000/s it was 4.88, 5.14, 5.31 and 5.65 ms from 5 to 8 replicas (see Cost per cycle), so the replica count itself adds cost, apart from the rate.
- Postgres's CPU per cycle rose with the rate, from 3.24 ms at 2 × 8's ceiling to 4.0–4.5 ms at 7 and 8 × 8's.

**ate-api-server is not at its CPU limit, and less so with each replica.**
- No replica was throttled, and the share of ate-api-server's CPU in use fell from 85% at 2 × 8 to 71% at 8 × 8.
- At 2 to 4 replicas, one replica used 7.04–7.14 of its 8 cores at every saturated step, the others 0.4–0.6 fewer. A single 8 CPU replica used 7.75 (`summary.md`), so 7.1 is not where a replica stops. That replica may run work the others do not, such as the jobs one replica takes on for all of them.
- From 5 replicas on even the busiest stops short of that, at 6.8–6.9 at 5 × 8 and 5.9 at 8 × 8, so no replica's CPU holds the others back there.

**Without the fake delay the ceiling is the same.** The 7 × 8 and 8 × 8 tests were repeated with fake-atelet answering every call at once instead of after 150 ms:

| | 7 × 8, 150 ms | 7 × 8, no delay | 8 × 8, 150 ms | 8 × 8, no delay |
|---|---|---|---|---|
| Ceiling (cycles/s) | about 8200 (8184–8220) | about 8100 (8068–8157) | about 8150 (8105–8216) | about 7680 (7633–7726) |
| Commits/s at the ceiling | about 205,000 | 201,000–205,000 | 203,000–206,000 | 191,000–193,000 |
| ate-api-server cores used | 42.4–43.1 (76–77%) | 42.2–44.2 (75–79%) | 45.1–46.2 (70–72%) | 43.5–45.0 (68–70%) |
| ate-api-server CPU per cycle at the ceiling | 5.18–5.24 ms | 5.21–5.42 ms | 5.56–5.66 ms | 5.68–5.85 ms |
| fsyncs/s at the ceiling | not sampled | 1,461–1,534 | 1,415 (last step) | 1,190–1,224 |
| Mean fsync | not sampled | 0.60–0.64 ms | 0.65 ms | 0.77–0.79 ms |
| Commits per fsync | not sampled | 131–139 | 143 | 157–161 |
| KB written per fsync | not recorded | 34–35 | not recorded | 39–40 |
| Flush busy | not sampled | 96–97% | 96% | 97–98% |
| Resume server mean just below the ceiling | 204–320 ms | 60–112 ms at 7000–7500/s | 200–287 ms | 101–221 ms at 7000–7500/s |

- **The delay does not limit throughput.** Without it a cycle holds its actor's lease, its user and its connections for a fraction of the time, yet 7 × 8's ceiling moved by about 1%. Commits stop at about 190,000–205,000/s with the flush busy 96–98% of the time, as with the delay, so the limit is the commit path, not the fake.
- **ate-api-server's work per cycle does not depend on the delay:** 5.2–5.4 ms at 7 × 8 and 5.7–5.8 ms at 8 × 8 either way.
- **8 × 8 is 6% lower without the delay.** Its fsyncs carry more commits (157–161 against 131–139 at 7 × 8) but take longer (0.77–0.79 against 0.60–0.64 ms), so fewer commits get through. One run each, but the gap is several times the run-to-run variation.
- **Without the delay, latency is the control plane's own.** Below saturation a resume takes 26 ms at 6000/s at 7 × 8 (server p50 11 ms) and 33 ms at 8 × 8, rising to 60–112 ms and 101–221 ms in the steps before the ceiling.
- **Past the ceiling the calls are served very unevenly.** A resume's server p50 is 23–400 ms but its p90 4.7–7.9 s and p99 11.7–12.6 s, far wider than with the delay (p99 3.2–6.4 s): most calls get through quickly while a few wait many seconds. Where they wait, such as for a pool connection or an actor's lease, these runs do not show.

**Postgres's resources have room; its commit path does not.**
- At most 37.5 of size10's 80 cores and 21.5GiB of its 140Gi, at 7 and 8 × 8.
- Every block served from memory (100% buffer cache hits).
- At most 269 of 300 connections open, 140–167 of them active.
- Lock waits grew with the rate, from under 1 connection at 2 × 8 to 4–10 at 5–8 × 8. They now take a small share of the time a cycle spends waiting.

**Latency.**
- **Below the ceiling:** a resume takes the fake's 150 ms plus 13–26 ms at the lower steps of every run.
- **Just before saturation:** 37 ms more at 3 × 8 (187 ms at 4500/s), 55–112 ms at 4 × 8, 71 ms at 5 × 8 (221 ms at 6500/s), 99 ms at 6 × 8 (249 ms at 7000/s), and 137–170 ms at 7 and 8 × 8 (287–320 ms at 8000/s).
- **Past the ceiling the spread is wide:** a resume's server p50 is about 180–1100 ms but its p90 2.2–4.6 s and its p99 3.2–6.4 s, against 820 ms and 1.5 s at 2 × 8. The widest are 6 × 8's last five steps: p90 4.6 s, p99 6.4 s.

**The load generator kept up.** The runner used at most 5.1, 6.5, 7.3, 8.1, 8.7 and 9.2 cores from 3 to 8 replicas, against requests of 12 and then 16.

## Per step

Columns as in `summary.md`. Server p50 / p90 / p95 / p99 are bucket-limited (see `summary.md`, Confidence and limits). The 2 × 8 run's table is in `summary-2x8-postgres.md`.

**3 × 8 CPU, size10 (8000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 3000 | 3000.0 | 163.4 / 163.4 | 155 / 196 / 202 / 314 | 161.0 | 12.49 | 4.22 / 4.15 / 4.12 | 9.23 | 75,139 | 37.8 | 24.8 |
| 3500 | 3500.1 | 167.8 / 167.7 | 156 / 198 / 204 / 403 | 163.8 | 14.49 | 4.89 / 4.81 / 4.79 | 11.31 | 87,616 | 35.7 | 20.7 |
| 4000 | 3999.4 | 173.1 / 172.5 | 157 / 200 / 240 / 451 | 167.9 | 16.36 | 5.52 / 5.44 / 5.40 | 13.48 | 100,069 | 54.7 | 32.8 |
| 4500 | 4500.7 | 187.3 / 187.0 | 160 / 233 / 366 / 702 | 180.8 | 18.13 | 6.11 / 6.02 / 6.00 | 15.80 | 112,362 | 58.0 | 35.0 |
| 5000 | 4948.1 | 641.2 / 640.9 | 338 / 2337 / 2807 / 3183 | 597.6 | 20.32 | 7.04 / 6.68 / 6.60 | 17.91 | 123,805 | 64.8 | 44.4 |
| 5500 | 4897.7 | 840.8 / 840.5 | 289 / 2784 / 3030 / 3227 | 782.1 | 20.10 | 7.05 / 6.53 / 6.53 | 17.67 | 122,692 | 61.3 | 44.4 |
| 6000 | 4937.7 | 836.4 / 836.1 | 282 / 2786 / 3032 / 3228 | 777.6 | 20.29 | 7.14 / 6.58 / 6.57 | 17.86 | 123,095 | 66.6 | 36.6 |
| 6500 | 4932.2 | 836.6 / 836.2 | 289 / 2783 / 3030 / 3227 | 780.4 | 20.30 | 7.13 / 6.61 / 6.56 | 17.95 | 123,467 | 62.8 | 37.2 |

4500/s held at 187 ms; from 5000/s throughput stays at 4898–4948/s with a mean of 641–841 ms. The busiest replica used 7.04–7.14 cores at the saturated steps, the others 6.53–6.68; none was throttled.

**4 × 8 CPU, size10 (10,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Per replica | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 4000 | 4000.1 | 167.5 / 167.4 | 155 / 197 / 203 / 443 | 163.1 | 18.14 | 4.63 / 4.51 / 4.51 / 4.50 | 13.62 | 100,068 | 47.2 | 26.3 |
| 4500 | 4500.1 | 172.2 / 172.0 | 156 / 199 / 205 / 564 | 166.4 | 19.59 | 5.17 / 5.05 / 4.70 / 4.67 | 15.84 | 112,607 | 67.8 | 45.3 |
| 5000 | 5000.1 | 176.0 / 175.8 | 157 / 200 / 232 / 602 | 169.6 | 21.51 | 5.69 / 5.57 / 5.56 / 4.68 | 18.22 | 125,044 | 63.4 | 32.2 |
| 5500 | 5500.2 | 205.6 / 205.3 | 162 / 334 / 551 / 786 | 195.1 | 24.32 | 6.18 / 6.05 / 6.05 / 6.04 | 20.75 | 137,160 | 65.7 | 35.1 |
| 6000 | 6000.2 | 262.5 / 261.6 | 177 / 620 / 731 / 872 | 245.4 | 26.54 | 6.73 / 6.61 / 6.60 / 6.59 | 23.00 | 150,293 | 67.6 | 37.5 |
| 6500 | 5905.8 | 867.5 / 867.2 | 202 / 2650 / 2990 / 3261 | 810.5 | 26.38 | 7.04 / 6.47 / 6.45 / 6.43 | 22.92 | 147,192 | 86.7 | 58.3 |
| 7000 | 5946.3 | 861.9 / 861.6 | 197 / 2647 / 2985 / 3255 | 812.0 | 26.74 | 7.13 / 6.55 / 6.53 / 6.53 | 22.98 | 149,020 | 87.6 | 55.7 |
| 7500 | 5888.5 | 875.4 / 874.9 | 202 / 2650 / 2985 / 3254 | 818.2 | 26.46 | 7.04 / 6.49 / 6.47 / 6.46 | 22.85 | 147,485 | 93.0 | 55.9 |
| 8000 | 5970.0 | 861.9 / 861.6 | 196 / 2643 / 2978 / 3246 | 811.2 | 26.76 | 7.11 / 6.56 / 6.55 / 6.53 | 23.02 | 149,394 | 75.3 | 41.4 |

6000/s held at 262 ms; from 6500/s throughput stays at 5889–5970/s with a mean of about 865 ms. The busiest replica used 7.04–7.13 cores at the saturated steps, the others 6.43–6.56, throttled in at most 0.2% of periods. At 5000/s one replica used 4.68 cores against 5.56–5.69 for the others, the only step where the load split unevenly by more than 0.5 cores.

From 5 × 8 on, the per-replica column gives the busiest replica and the range of the others.

**5 × 8 CPU, size10 (12,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Busiest replica / the others | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 4500 | 4501.4 | 168.9 / 168.7 | 155 / 197 / 203 / 525 | 163.6 | 21.84 | 4.53 / 3.88–4.53 | 14.88 | 112,569 | 46.1 | 22.0 |
| 5000 | 5000.1 | 173.2 / 172.9 | 156 / 199 / 205 / 586 | 166.6 | 24.39 | 4.99 / 4.60–4.98 | 18.29 | 125,337 | 72.1 | 47.3 |
| 5500 | 5500.1 | 182.0 / 181.4 | 158 / 202 / 311 / 683 | 173.7 | 26.94 | 5.42 / 5.27–5.42 | 20.87 | 137,525 | 74.4 | 42.1 |
| 6000 | 5998.1 | 203.3 / 202.9 | 161 / 298 / 523 / 806 | 191.5 | 28.62 | 5.83 / 5.44–5.83 | 23.31 | 150,519 | 75.9 | 42.7 |
| 6500 | 6474.5 | 221.3 / 220.5 | 166 / 436 / 652 / 919 | 207.0 | 31.24 | 6.29 / 6.14–6.29 | 25.80 | 162,385 | 99.7 | 56.6 |
| 7000 | 6822.7 | 827.4 / 826.9 | 372 / 2721 / 2999 / 3221 | 770.7 | 32.75 | 6.87 / 6.40–6.55 | 27.49 | 170,514 | 124.7 | 75.4 |
| 7500 | 6849.1 | 900.7 / 900.2 | 344 / 2858 / 3067 / 3235 | 839.8 | 32.84 | 6.93 / 6.03–6.91 | 27.78 | 171,372 | 114.5 | 72.1 |
| 8000 | 6736.9 | 918.0 / 917.7 | 340 / 2861 / 3069 / 3235 | 856.3 | 32.02 | 6.84 / 5.96–6.84 | 27.22 | 168,443 | 123.8 | 78.4 |
| 8500 | 6706.5 | 923.3 / 922.9 | 344 / 2863 / 3070 / 3235 | 861.1 | 32.85 | 6.85 / 6.32–6.83 | 27.06 | 167,839 | 105.0 | 56.8 |
| 9000 | 6710.6 | 923.7 / 923.2 | 349 / 2862 / 3070 / 3235 | 861.0 | 32.81 | 6.82 / 6.32–6.81 | 27.37 | 167,899 | 127.5 | 76.4 |

6500/s held at 221 ms; from 7000/s throughput stays at 6707–6849/s with a mean of 827–924 ms. Postgres's commits stopped at 168,000–171,000/s.

**6 × 8 CPU, size10 (14,000 users)**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Busiest replica / the others | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5000.2 | 172.5 / 172.1 | 156 / 199 / 204 / 561 | 166.7 | 25.68 | 4.37 / 4.24–4.28 | 18.78 | 125,124 | 80.6 | 51.1 |
| 5500 | 5514.8 | 179.4 / 179.1 | 157 / 201 / 269 / 642 | 171.9 | 27.76 | 4.77 / 4.32–4.69 | 21.25 | 137,700 | 120.9 | 82.7 |
| 6000 | 5999.8 | 189.1 / 188.4 | 159 / 204 / 360 / 755 | 179.6 | 30.08 | 5.16 / 4.74–5.07 | 23.81 | 150,219 | 86.0 | 50.3 |
| 6500 | 6500.5 | 207.4 / 206.8 | 163 / 336 / 540 / 798 | 195.9 | 32.54 | 5.51 / 5.38–5.43 | 26.84 | 162,698 | 129.3 | 83.3 |
| 7000 | 6983.5 | 249.4 / 248.9 | 169 / 580 / 728 / 1216 | 231.1 | 34.93 | 5.93 / 5.75–5.84 | 28.90 | 174,330 | 141.4 | 101.9 |
| 7500 | 7446.6 | 686.0 / 685.2 | 530 / 1947 / 2612 / 3144 | 636.0 | 37.29 | 6.50 / 5.88–6.24 | 31.78 | 186,203 | 150.2 | 78.6 |
| 8000 | 7350.8 | 978.8 / 978.4 | 189 / 4583 / 5568 / 6357 | 911.5 | 36.65 | 6.50 / 6.00–6.07 | 29.25 | 183,921 | 144.6 | 84.1 |
| 8500 | 7355.3 | 983.3 / 983.0 | 184 / 4585 / 5569 / 6357 | 914.3 | 36.60 | 6.52 / 5.99–6.03 | 31.21 | 183,328 | 154.9 | 88.7 |
| 9000 | 7330.7 | 984.8 / 984.6 | 181 / 4581 / 5567 / 6356 | 923.4 | 36.50 | 6.52 / 5.96–6.02 | 31.11 | 182,905 | 146.2 | 76.5 |
| 9500 | 7312.3 | 990.1 / 989.9 | 180 / 4591 / 5572 / 6357 | 921.2 | 36.59 | 6.54 / 5.99–6.02 | 31.20 | 182,941 | 150.6 | 84.4 |
| 10000 | 7328.8 | 985.8 / 985.4 | 185 / 4585 / 5569 / 6357 | 921.5 | 36.61 | 6.51 / 5.98–6.04 | 29.18 | 183,816 | 137.7 | 79.6 |

7000/s held at 249 ms; 7500/s reached 7447/s, and from 8000/s throughput stays at 7312–7355/s with a mean of about 985 ms. Commits held at about 183,000/s.

**7 × 8 CPU, size10 (16,000 users), pools of 35**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Busiest replica / the others | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5000.3 | 170.5 / 170.2 | 155 / 197 / 203 / 554 | 164.5 | 26.55 | 3.93 / 3.58–3.82 | 19.06 | 125,126 | 61.4 | 31.9 |
| 5500 | 5500.5 | 174.3 / 174.1 | 156 / 199 / 204 / 603 | 167.3 | 29.15 | 4.31 / 3.92–4.20 | 21.48 | 137,592 | 95.4 | 49.6 |
| 6000 | 6000.2 | 180.2 / 180.0 | 157 / 200 / 236 / 671 | 172.0 | 31.89 | 4.67 / 4.53–4.55 | 24.05 | 150,153 | 100.0 | 54.1 |
| 6500 | 6500.3 | 197.4 / 196.6 | 160 / 250 / 413 / 812 | 185.4 | 34.04 | 5.00 / 4.59–4.90 | 26.55 | 162,636 | 104.5 | 64.7 |
| 7000 | 6984.6 | 204.3 / 203.7 | 161 / 294 / 539 / 1046 | 189.0 | 36.14 | 5.25 / 4.73–5.24 | 29.07 | 174,614 | 117.8 | 58.3 |
| 7500 | 7500.6 | 222.0 / 221.4 | 164 / 416 / 657 / 1136 | 207.2 | 38.90 | 5.66 / 5.53–5.56 | 31.75 | 187,500 | 121.6 | 55.9 |
| 8000 | 8002.7 | 320.6 / 319.8 | 189 / 716 / 802 / 1433 | 296.9 | 40.88 | 6.01 / 5.42–5.92 | 34.91 | 200,721 | 141.8 | 81.7 |
| 8500 | 8183.7 | 976.3 / 975.6 | 931 / 2631 / 2954 / 3212 | 911.1 | 42.39 | 6.35 / 5.61–6.28 | 36.97 | 203,977 | 142.5 | 65.2 |
| 9000 | 8190.4 | 1006.6 / 1006.2 | 379 / 2892 / 3084 / 3238 | 938.2 | 42.66 | 6.40 / 5.61–6.35 | 36.72 | 205,488 | 146.0 | 65.5 |
| 9500 | 8220.2 | 1002.8 / 1002.5 | 393 / 2853 / 3094 / 3884 | 934.1 | 43.08 | 6.42 / 5.99–6.34 | 33.99 | 205,097 | 148.3 | 78.3 |
| 10000 | 8002.3 | 1028.0 / 1027.6 | 186 / 4249 / 5416 / 6351 | 958.1 | 41.28 | 5.99 / 5.85–5.95 | 33.37 | 200,672 | 140.4 | 64.6 |

8000/s held at 320 ms; from 8500/s throughput stays at 8184–8220/s with a mean of about 1000 ms, falling to 8002/s at 10000/s. Commits held at 204,000–205,000/s.

**8 × 8 CPU, size10 (18,000 users), pools of 30**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Busiest replica / the others | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5000.2 | 172.5 / 172.3 | 156 / 198 / 204 / 587 | 166.0 | 28.23 | 3.57 / 3.47–3.56 | 19.00 | 125,183 | 61.3 | 31.6 |
| 5500 | 5500.3 | 175.2 / 174.9 | 156 / 199 / 205 / 604 | 168.2 | 30.31 | 3.93 / 3.58–3.92 | 21.36 | 137,582 | 83.5 | 48.4 |
| 6000 | 6000.8 | 182.7 / 182.5 | 157 / 200 / 268 / 712 | 173.9 | 33.75 | 4.26 / 4.17–4.25 | 24.06 | 150,192 | 78.8 | 50.7 |
| 6500 | 6500.5 | 186.1 / 185.7 | 158 / 203 / 343 / 749 | 177.5 | 36.46 | 4.60 / 4.51–4.59 | 26.49 | 162,724 | 100.2 | 54.2 |
| 7000 | 7000.6 | 200.1 / 199.8 | 160 / 249 / 508 / 918 | 188.3 | 38.18 | 4.93 / 4.56–4.91 | 29.09 | 174,791 | 115.8 | 60.6 |
| 7500 | 7500.5 | 223.5 / 223.1 | 164 / 446 / 667 / 1104 | 209.3 | 40.69 | 5.22 / 4.77–5.20 | 31.81 | 187,559 | 143.3 | 77.2 |
| 8000 | 8018.1 | 287.0 / 286.5 | 179 / 668 / 789 / 1421 | 268.7 | 43.65 | 5.55 / 5.11–5.53 | 35.05 | 200,392 | 130.0 | 61.3 |
| 8500 | 8216.1 | 1084.5 / 1083.4 | 1122 / 2237 / 2757 / 3173 | 1005.3 | 46.20 | 5.94 / 5.49–5.89 | 36.70 | 205,827 | 139.6 | 70.1 |
| 9000 | 8155.6 | 1135.1 / 1134.2 | 808 / 2843 / 3060 / 3233 | 1054.7 | 45.77 | 5.92 / 5.50–5.92 | 33.42 | 204,417 | 135.0 | 63.2 |
| 9500 | 8133.1 | 1142.3 / 1141.2 | 776 / 2842 / 3059 / 3233 | 1059.4 | 46.03 | 5.91 / 5.60–5.89 | 36.26 | 203,239 | 136.7 | 76.3 |
| 10000 | 8164.1 | 1136.6 / 1135.4 | 730 / 2841 / 3060 / 3236 | 1053.5 | 45.67 | 5.91 / 5.45–5.89 | 36.03 | 204,877 | 167.0 | 97.5 |
| 10500 | 8104.7 | 1145.1 / 1144.2 | 530 / 3904 / 5229 / 6289 | 1062.4 | 45.07 | 5.91 / 5.19–5.88 | 35.86 | 202,714 | 144.7 | 78.6 |

8000/s held at 287 ms; from 8500/s throughput stays at 8105–8216/s with a mean of about 1140 ms, the same ceiling as 7 × 8. Commits held at 203,000–206,000/s.

**7 × 8 CPU, size10, no fake delay (16,000 users), pools of 35**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Busiest replica / the others | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 6000 | 6001.1 | 26.5 / 26.1 | 11 / 25 / 60 / 408 | 19.5 | 31.64 | 4.65 / 4.21–4.62 | 24.95 | 150,124 | 75.3 | 41.2 |
| 6500 | 6500.4 | 38.2 / 37.8 | 12 / 43 / 238 / 563 | 28.7 | 34.32 | 4.99 / 4.87–4.95 | 27.06 | 162,657 | 107.0 | 56.9 |
| 7000 | 7000.6 | 60.2 / 59.6 | 16 / 198 / 391 / 753 | 46.9 | 36.16 | 5.30 / 4.83–5.27 | 29.63 | 175,171 | 126.9 | 70.8 |
| 7500 | 7528.5 | 112.3 / 111.6 | 22 / 420 / 630 / 797 | 97.7 | 38.50 | 5.69 / 5.15–5.65 | 32.05 | 188,021 | 144.3 | 83.1 |
| 8000 | 8043.6 | 411.0 / 410.0 | 443 / 801 / 1124 / 1535 | 369.8 | 43.11 | 6.30 / 6.10–6.20 | 34.21 | 201,008 | 143.9 | 93.1 |
| 8500 | 8157.0 | 1002.0 / 1001.1 | 944 / 2415 / 2878 / 3248 | 921.8 | 44.21 | 6.57 / 5.81–6.51 | 34.90 | 204,982 | 157.0 | 92.5 |
| 9000 | 8100.0 | 1017.6 / 1016.9 | 208 / 4235 / 5403 / 6337 | 937.7 | 42.85 | 6.52 / 5.66–6.45 | 31.88 | 203,627 | 146.7 | 83.0 |
| 9500 | 8104.1 | 1028.1 / 1027.9 | 24 / 4795 / 6586 / 11803 | 945.9 | 42.19 | 6.57 / 5.61–6.04 | 35.09 | 202,224 | 146.8 | 69.2 |
| 10000 | 8067.9 | 1031.0 / 1030.7 | 24 / 4804 / 6654 / 11817 | 948.4 | 42.37 | 6.55 / 5.95–6.00 | 34.71 | 201,406 | 138.0 | 71.7 |
| 10500 | 8083.8 | 1032.6 / 1032.4 | 24 / 4725 / 6400 / 11664 | 947.0 | 42.66 | 6.58 / 5.99–6.04 | 34.87 | 202,103 | 160.5 | 81.4 |
| 11000 | 8128.5 | 1024.2 / 1024.0 | 23 / 4756 / 6479 / 11737 | 944.0 | 42.63 | 6.58 / 5.99–6.04 | 34.89 | 203,374 | 137.2 | 67.4 |

7500/s held at 112 ms; 8000/s reached 8044/s at 410 ms, and from 8500/s throughput stays at 8068–8157/s with a mean of about 1030 ms. From 9500/s the server p50 fell to 24 ms while the p99 rose to about 11.8 s.

**8 × 8 CPU, size10, no fake delay (18,000 users), pools of 30**

| Target | Resume/s | Resume mean client / server (ms) | Server p50 / p90 / p95 / p99 | Pause server mean | ateapi cores (all) | Busiest replica / the others | Postgres cores | Commits/s | Active PG conns | Waiting on WAL write |
|---|---|---|---|---|---|---|---|---|---|---|
| 6000 | 6000.3 | 32.6 / 32.2 | 12 / 34 / 161 / 482 | 24.9 | 33.41 | 4.27 / 3.82–4.25 | 24.97 | 150,154 | 78.9 | 43.0 |
| 6500 | 6500.4 | 47.8 / 47.5 | 17 / 87 / 313 / 681 | 38.0 | 36.09 | 4.58 / 4.31–4.58 | 27.81 | 163,014 | 108.7 | 62.2 |
| 7000 | 7000.8 | 101.8 / 101.4 | 22 / 427 / 634 / 799 | 86.5 | 38.35 | 4.93 / 4.48–4.91 | 30.99 | 174,879 | 143.4 | 82.6 |
| 7500 | 7539.9 | 222.1 / 221.4 | 85 / 672 / 751 / 814 | 198.7 | 42.37 | 5.37 / 5.19–5.35 | 32.52 | 187,930 | 154.1 | 91.1 |
| 8000 | 7683.0 | 1191.2 / 1190.7 | 1167 / 2545 / 2911 / 3204 | 1097.6 | 44.98 | 5.78 / 5.15–5.78 | 33.62 | 191,294 | 152.2 | 91.1 |
| 8500 | 7641.5 | 1221.9 / 1220.9 | 805 / 2872 / 3092 / 3269 | 1120.8 | 44.41 | 5.77 / 5.34–5.76 | 33.43 | 191,697 | 154.9 | 81.7 |
| 9000 | 7725.5 | 1208.8 / 1208.3 | 397 / 2935 / 3171 / 5370 | 1110.6 | 44.89 | 5.83 / 5.43–5.82 | 33.81 | 193,167 | 165.2 | 100.1 |
| 9500 | 7694.8 | 1213.7 / 1213.2 | 380 / 3168 / 4518 / 6146 | 1116.0 | 43.73 | 5.80 / 4.99–5.80 | 33.84 | 193,190 | 166.7 | 98.9 |
| 10000 | 7692.4 | 1216.9 / 1216.3 | 388 / 3920 / 5237 / 6290 | 1118.0 | 44.41 | 5.80 / 5.37–5.75 | 33.75 | 193,229 | 160.9 | 88.8 |
| 10500 | 7632.6 | 1213.3 / 1212.6 | 358 / 5202 / 9198 / 12325 | 1119.8 | 43.79 | 5.80 / 5.13–5.63 | 33.34 | 191,176 | 178.8 | 105.2 |
| 11000 | 7672.4 | 1212.2 / 1211.7 | 159 / 7834 / 10471 / 12580 | 1116.9 | 43.68 | 5.84 / 5.34–5.47 | 33.50 | 192,517 | 168.1 | 106.0 |
| 11500 | 7633.9 | 1231.9 / 1231.0 | 275 / 7899 / 10503 / 12586 | 1132.8 | 43.54 | 5.80 / 5.28–5.48 | 33.30 | 190,969 | 160.8 | 80.8 |

7000/s held at 101 ms; 7500/s reached 7540/s at 221 ms, and from 8000/s throughput stays at 7633–7726/s with a mean of about 1215 ms, 6% below the 150 ms run. Fsyncs held at about 1,200/s of about 0.78 ms, each covering about 160 commits.

## Cost per cycle

| | ateapi CPU per cycle | Postgres CPU per cycle | Commits per cycle |
|---|---|---|---|
| 2 × 8, 3000/s | 3.64 ms | 2.99 ms | 25.0 |
| 2 × 8, 3652/s (ceiling) | 3.67 ms | 3.24 ms | 25.0 |
| 3 × 8, 3000/s | 4.16 ms | 3.08 ms | 25.0 |
| 3 × 8, 4500/s | 4.03 ms | 3.51 ms | 25.0 |
| 3 × 8, 4948/s (ceiling) | 4.11 ms | 3.62 ms | 25.0 |
| 4 × 8, 4000/s | 4.53 ms | 3.40 ms | 25.0 |
| 4 × 8, 5000/s | 4.30 ms | 3.64 ms | 25.0 |
| 4 × 8, 5906/s (ceiling) | 4.47 ms | 3.88 ms | 24.9 |
| 5 × 8, 5000/s | 4.88 ms | 3.66 ms | 25.1 |
| 5 × 8, 6849/s (ceiling) | 4.79 ms | 4.06 ms | 25.0 |
| 6 × 8, 5000/s | 5.14 ms | 3.76 ms | 25.0 |
| 6 × 8, 7351/s (ceiling) | 4.99 ms | 3.98 ms | 25.0 |
| 7 × 8, 5000/s | 5.31 ms | 3.81 ms | 25.0 |
| 7 × 8, 8184/s (ceiling) | 5.18 ms | 4.52 ms | 24.9 |
| 8 × 8, 5000/s | 5.65 ms | 3.80 ms | 25.0 |
| 8 × 8, 8216/s (ceiling) | 5.62 ms | 4.47 ms | 25.1 |
| 7 × 8 no delay, 6000/s | 5.27 ms | 4.16 ms | 25.0 |
| 7 × 8 no delay, 8157/s (ceiling) | 5.42 ms | 4.28 ms | 25.1 |
| 8 × 8 no delay, 6000/s | 5.57 ms | 4.16 ms | 25.0 |
| 8 × 8 no delay, 7726/s (ceiling) | 5.81 ms | 4.38 ms | 25.0 |

- Every cycle costs 25 commits, as in every other run at this code.
- At the same rate ate-api-server's CPU per cycle is higher with more replicas: at 3000/s 3.64 ms at 2 × 8 against 4.16 at 3 × 8; at 4000/s about 4.09 at 3 × 8 against 4.53 at 4 × 8; at 5000/s 4.30, 4.88, 5.14, 5.31 and 5.65 ms from 4 to 8 replicas. Each replica adds about 0.3 ms per cycle at 5000/s. Much of the rise therefore comes with the replica count itself, such as each replica's own background work, not only with the rate.
- Postgres's CPU per cycle depends on the rate, not on the replica count: 3.64–3.81 ms at 5000/s from 4 to 8 replicas, rising to 4.5 ms at 8200/s.

## Confidence and limits

- **One run per configuration.** The gains from 2 to 7 replicas are far larger than the about ±1% run-to-run variation; 7 × 8 and 8 × 8 are the same within it. The exact factors rest on single runs.
- **Measured directly:** the flush's busy share, fsync latency and commits per fsync, with `track_wal_io_timing` on:
  - every step of 4 × 8, 5 × 8 and 6 × 8, and of both no-delay runs
  - the steps through 4000/s of 3 × 8
  - the last step of 8 × 8
  - none of 7 × 8
  The sampler ran on the workstation, which suspended during 7 × 8.
- **Run interruptions:** 7 × 8 and 8 × 8 ran while their orchestrator was suspended or gone. Their runners and Prometheus, which produced every figure here except the WAL samples, ran in the cluster and were unaffected.
- **Inferred:**
  - that larger batches come from more commits in flight, from the matching rise in active connections and `WALWrite` waits across the runs
  - why fsyncs slow down with more replicas: bytes per fsync, recorded in the no-delay runs, do not explain it
  - that the flush sets the 7 × 8 and 8 × 8 ceiling, from the cap on commits per second continuing the trend of the sampled runs and from ate-api-server's and Postgres's idle CPU; neither run has WAL samples at its saturated steps but the last of 8 × 8
  - why the busiest replica at 2 to 4 replicas uses more CPU than the others
- **Scope:**
  - isolate mode with a 150 ms fake call, no Worker churn, pause rather than suspend
  - size10 Postgres on pd-balanced, pools of 40 (35 and 30 at 7 and 8 × 8), c3-highcpu-88 nodes
  - the code before `dee31f7f`, which would cut each cycle to 20 commits

## Next steps

1. **The 7 × 8 test on `dee31f7f`** (branch `actor-update-in-worker-txn`): 20 commits per cycle instead of 25 and 8 writes instead of 10. With the flush capping commits per second, fewer per cycle is the most direct way past about 8200 cycles/s. It raised the 1M-actor ceiling 8% (`summary-size10.md`).
2. **Why fsyncs slow down with more replicas.** Bytes per fsync do not explain it (see Without the fake delay). Postgres's `WALWrite` lock waits and its WAL writer's activity at 7 × 8 against 8 × 8 would show whether contention for the flush does. Lower-latency storage (pd-ssd or Hyperdisk) is worth a 7 × 8 run only if the disk turns out to be the slow part.
3. **Where calls wait past the ceiling.** Without the delay a few calls wait 10+ s while most take 25 ms. A trace of the slowest calls, or pool-wait metrics from ate-api-server, would show whether they wait for a pool connection, an actor's lease or something else.
4. **A Go CPU profile of ate-api-server** at 5000/s at 4 × 8 and 8 × 8, to see what each added replica costs per cycle.
