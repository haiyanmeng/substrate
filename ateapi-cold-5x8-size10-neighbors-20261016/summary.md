# What shares the nodes with a 5 × 8 CPU cold resume run on size10 Postgres, 2026-10-16

- Test `ateapi_cold_resume_pause_fake_5x8cpu_size10`, unchanged from `tests.yaml`, run with the local orchestrator on branch `ateapi-cold-resume-capacity` at `ab89dcae`, 15:53–16:37 UTC, ladder 16:01:34–16:31:35. Complete; none of about 22.2 million requests failed (11,094,342 cold resumes, 11,088,777 pauses, 12,000 warm-ups).
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`, 3 × c3-highcpu-88 plus the worker pool, every node a 100 GB pd-balanced boot disk).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_5x8cpu_size10/run_date=2026-10-16/run_ts=1792166493/`; local copy and per-step analysis (`steps.jsonl`, from `benchmarking/automation/analyze_ladder.py`; `neighbors.jsonl`, from `results/neighbors.py`) in `results/5x8cpu_size10/`. WAL samples every 15 s in `results/wal-io.csv`; every pod's node and phase every 30 s in `results/placement.tsv` (from `results/sample-placement.sh`), node roles in `results/nodes.tsv`. Orchestrator log: `~/bench-runs/ateapi-cold-5x8-size10-neighbors-20261016/orchestrator.log`.
- The run it repeats is 5 × 8 in `../ateapi-cold-4to9x8-size10-20261015/summary.md`, at `5857fd53`.

**Result: the other pods on the nodes did not measurably affect the run.**

- **It repeats the 2026-10-15 run** within 1% at every step: the ceiling is about 6870 cycles/s (6853–6884) against 6817–6899, with the same Postgres cores, commits/s and WAL flush. The placement was the same, Postgres on `…-9v65` and ate-api-server on `…-4d75`.
- **Neighbors use at most about 3 cores of a node's 88**, steady through each step and rising only with the rate. No node used more than half its cores, and the ceiling is still set by Postgres's WAL flush.
- **No pod arrived, left or moved** on the three nodes during the ladder: all 56 placement snapshots match.
- **fake-atelet runs on every benchmark node**, Postgres's and ate-api-server's included, because the default-pool nodes are atelet nodes. At 1.1–1.7 cores at the ceiling it is the largest neighbor on each.

## Setup

As the 2026-10-15 run: 5 ate-api-server replicas at 8 CPUs and 4Gi on one node, read/write pools of 40, size10 Postgres with `track_wal_io_timing=on`, 12,000 users, fake-atelet answering after 150 ms, 200 fake Workers, ladder 4500–9000/s in 3-minute steps of 500. The orchestrator pins only Postgres, ate-api-server and the runner to a node each; every other pod schedules freely.

## Per step

| Target | Resume/s, now / 10-15 | Server mean (ms), now / 10-15 | Server p50 / p99 | ateapi cores | ateapi CPU per cycle | Postgres cores, now / 10-15 | Commits/s, now / 10-15 | Active PG conns | Lock waits | fsyncs/s | Mean fsync | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 4500 | 4,500 / 4,500 | 172 / 174 | 156 / 558 | 22.6 | 5.01 ms | 15.8 / 15.9 | 108,133 / 108,053 | 80 | 4.0 | 1,755 | 0.53 ms | 61 |
| 5000 | 5,000 / 5,002 | 177 / 180 | 157 / 630 | 25.3 | 5.06 ms | 18.3 / 18.3 | 120,099 / 120,168 | 74 | 3.9 | 1,756 | 0.53 ms | 68 |
| 5500 | 5,496 / 5,497 | 188 / 189 | 158 / 742 | 27.5 | 5.00 ms | 20.5 / 21.1 | 132,184 / 132,101 | 68 | 4.4 | 1,729 | 0.53 ms | 76 |
| 6000 | 6,001 / 6,000 | 204 / 202 | 162 / 800 | 29.8 | 4.97 ms | 21.4 / 21.6 | 143,781 / 144,146 | 89 | 6.5 | 1,712 | 0.54 ms | 84 |
| 6500 | 6,499 / 6,509 | 271 / 291 | 177 / 1,054 | 32.0 | 4.92 ms | 25.3 / 25.4 | 155,902 / 155,424 | 90 | 9.2 | 1,691 | 0.55 ms | 93 |
| 7000 | 6,881 / 6,819 | 742 / 881 | 382 / 3,824 | 34.2 | 4.96 ms | 27.5 / 27.3 | 165,803 / 164,248 | 105 | 12.3 | 1,655 | 0.56 ms | 100 |
| 7500 | 6,865 / 6,882 | 928 / 925 | 189 / 6,389 | 33.8 | 4.93 ms | 27.4 / 27.6 | 164,947 / 165,076 | 113 | 15.4 | 1,645 | 0.57 ms | 100 |
| 8000 | 6,884 / 6,899 | 929 / 928 | 198 / 6,389 | 32.9 | 4.78 ms | 27.5 / 27.8 | 165,433 / 165,718 | 110 | 10.9 | 1,687 | 0.56 ms | 98 |
| 8500 | 6,857 / 6,889 | 935 / 931 | 191 / 6,389 | 33.8 | 4.93 ms | 27.4 / 27.8 | 164,980 / 165,390 | 132 | 16.0 | 1,660 | 0.56 ms | 99 |
| 9000 | 6,853 / 6,817 | 936 / 941 | 194 / 6,388 | 33.7 | 4.91 ms | 25.4 / 27.4 | 163,961 / 163,507 | 116 | 15.4 | 1,653 | 0.56 ms | 99 |

From 7000/s the WAL fsyncs take 93–94% of each second (fsyncs/s × mean fsync), as on 2026-10-15, where the run had 1,630–1,697 fsyncs/s of 0.55–0.57 ms carrying 97–101 commits each.

## What else ran on each node

At the ceiling steps, 7000–9000/s:

| Node (role) | Benchmark pod cores | Other pods' cores | Largest neighbors | Node CPU pressure | Benchmark container CPU pressure | Network rx / tx |
|---|---|---|---|---|---|---|
| `…-9v65` (Postgres) | 25.4–27.5 | 1.7 | fake-atelet 1.6; anetd 0.03; fluentbit, gke-metrics-agent and podcertificate-controller under 0.01 | 8% | 7–8% | 240 / 240 MB/s |
| `…-4d75` (ate-api-server) | 32.9–34.2 | 3.1 | fake-atelet 1.7; fluentbit 1.3; anetd 0.04; atenet-egress, fake-workersync and k8s-credential-provider under 0.03 | 4% | 1.7–1.8%, throttled 0.1–0.3% | 285 / 300 MB/s |
| `…-d1sf` (runner) | 5.7–6.2 | 1.2 | fake-atelet 1.1; anetd 0.03; atenet-router 0.02; Prometheus 0.016; Grafana about 0 | 0.7% | 0.4% | 27 / 27 MB/s |

Each node also ran 11–16 GKE system pods (kube-system, gmp-system, gke-managed-otel), together under 0.1 core apart from fluentbit. The neighbors' CPU grows with the rate, fake-atelet's from 0.8–1.2 cores at 4500/s, and holds steady within each step; `results/5x8cpu_size10/neighbors.jsonl` has every step.

Postgres's 7–8% CPU pressure, with about 60 of its node's cores idle and 1.7 used by neighbors, more likely comes from scheduling among Postgres's own backends than from neighbors; this data does not separate the two.

## Confidence and limits

- **ate-api-server's logs probably cost about 5 cores** on its node: fluentbit's 1.3, and about 4 cores of node CPU outside any pod that appear only on that node, plausibly containerd writing the replicas' stdout. Prometheus scrapes only the root cgroup, not systemd's services, so the 4 cores are not attributed. They do not limit the run: the node had about 47 cores idle.
- **The root cgroup's CPU is not exact.** On the Postgres node it reads 1.3–3.3 cores below the sum of its pods, so node totals are soft; per-pod figures come from each container's own counter.
- **CPU, CPU pressure, throttling and network are all that was measured.** Memory bandwidth, shared cache and disk contention from neighbors are not visible in these metrics. With neighbors this small they are unlikely to matter, but this run does not show it.
- **Teardown**: `ate-setup` timed out waiting for the `ate-system` namespace to be deleted after the results were uploaded; the namespace was gone a few minutes later.

## How results are measured

`steps.jsonl` as in the 2026-10-15 summary: each step's window runs from 30 s after it starts to 5 s before it ends. `neighbors.py` uses the same windows and, for each node that held Postgres, ate-api-server or the runner, queries cAdvisor in the benchmarking Prometheus: every pod's CPU, the root cgroup's CPU, CPU pressure (`container_pressure_cpu_waiting_seconds_total`) for the node and the benchmark container, CFS throttling, and the node's network bytes. The WAL columns are from the sampler over the same windows.
