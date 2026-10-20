# ate-api-server cold resume capacity at 2 CPUs, with and without authorization, 2026-10-19

- Test `ateapi_cold_resume_pause_fake_2cpu`, run three times with a local driver (`scripts/driver.py`) that calls `benchmarking/automation/orchestrator.py`'s own steps from a local checkout:
  - **poc-authz-on**: cluster `substrate-poc`, 18:39 UTC (run_ts 1792435160). Authorization on, the install default.
  - **prow-authz-on**: cluster `nh-prow-parity`, 19:39 UTC (run_ts 1792438745). Authorization on.
  - **prow-authz-off**: cluster `nh-prow-parity`, 20:46 UTC (run_ts 1792442797). The same as prow-authz-on, but installed with `ATE_API_EXPERIMENTAL_ENABLE_AUTHZ=false`.
- Each run complete, no request failed.
- Code: `974379e0`, which is PR 2686's branch `fake-dataplane-test-1` at `b722bd29` plus the three commits of `fix-fake-data-plane` (`--preview` on the fake binaries, the controller authz role for fake-workersync, and WorkerPool SandboxConfig resolution in fake-workersync), without which no `--fake-data-plane` install comes up on current `main`. `main` is at `bbf30db1` (2026-10-19) underneath.
- Results: `results/<run>/`, with `stats.jsonl`, the per-step rows in `steps.jsonl`, `failures.csv`, `server_summary.json` and the driver's log in `orchestrator.log`. Each `gcs-path.txt` gives the run's upload path. `results/pg-statements-per-cycle.txt` breaks down Postgres statements per cycle for the two `nh-prow-parity` runs.
- Compared with the 2 CPU result on branch `ateapi-cold-resume-capacity` (`ateapi-cold-2-4-8cpu-20261009/summary.md`): about 815 cycles/s on `nh-prow-parity`, 2026-10-09, branch `fake-dataplane-test` at `4ded1d6b`.

## Setup

- Isolate mode (fake data plane): fake-atelet answers each call after 150 ms; 200 fake Workers of 1000 actors each.
- Each actor books 10m CPU (`actorCPU`), so placement never binds even though a fake Worker reports its node's allocatable as its capacity.
- resumepaused: 1000 users, each owning one actor. Its first iteration creates the actor and pauses it, and every later one resumes it and pauses it again. One cycle is one `ResumeActor` plus one `SuspendActor(ASYNCHRONOUS)`.
- Ladder of 3-minute steps at 100, 200, 400, 600, 800, 1000 and 1500 cycles/s.
- ate-api-server: 1 replica, 2 CPU and 4Gi requests = limits (Guaranteed, `GOMAXPROCS` 2), read/write pool of 64 connections. Applied by the test's `controlPlane:` block after a `--cordon-control-plane --fake-data-plane` install at the default size.
- Postgres: `postgres-0` at the default size, 2 CPU request and 16 CPU limit, with `pg_stat_statements` and a postgres_exporter sidecar.

**Placement**

| Role | substrate-poc (6 × c3-standard-4) | nh-prow-parity (3 × c3-highcpu-88, 7 × c3-highcpu-4) |
|---|---|---|
| Postgres | one c3-standard-4, `ate.dev/workloadType=ate-postgres` | one c3-highcpu-88 (`…-4d75`), `ate.dev/workloadType=ate-postgres` |
| Control plane | two c3-standard-4, `ate.dev/workloadType=ate-control-plane`, shared with GKE system pods | one c3-highcpu-88 (`…-9v65`), `ate.dev/workloadType=ate-control-plane` |
| Runner | a temporary c3-standard-22 pool tainted `ate.dev/benchmarking=true`, deleted after the run | one c3-highcpu-4 (`…-0j04`), labeled and tainted `ate.dev/benchmarking=true` |
| Workers (fake-atelet) | three c3-standard-4 | one c3-highcpu-88 and six c3-highcpu-4 |

- The node roles were set with `kubectl label` and `kubectl taint`, not node pools.
- On `nh-prow-parity` the runner requested 3 CPU instead of the test's 8, set by the driver (`RUNNER_CPU`), and was pinned to its node with a `nodeSelector` the driver adds (`RUNNER_NODE_SELECTOR`); `tests.yaml` is unchanged. In the `substrate-poc` run the runner container used at most 0.11 cores while driving the full plateau of about 630 calls/s.

## Result

| Run | Ceiling (cycles/s) | Bound by | ate-api-server CPU per cycle at the ceiling | Postgres commits per cycle |
|---|---|---|---|---|
| poc-authz-on | about 310 (301–313) | ate-api-server CPU: 1.96 of 2 cores, 5–8% throttled | about 6.3 ms | 33 |
| prow-authz-on | about 500 (489–516) | ate-api-server CPU: 2.00 cores, 25–27% throttled | about 4.0 ms | 39 below the ceiling, 33 at it |
| prow-authz-off | about 735 (724–745) | ate-api-server CPU: 2.00 cores, 38% throttled | about 2.7 ms | 26 |
| `ateapi-cold-resume-capacity`, 2026-10-09 | about 815 (808–818) | ate-api-server CPU: 2.00 cores, 23–25% throttled | about 2.45 ms | 25 |

CPU per cycle is ate-api-server's cores divided by the cycle rate.

**prow-authz-on and prow-authz-off, step by step**

| Target/s | Authz on: achieved/s | Commits/cycle | CPU ms/cycle | Client resume p50 / p99 | Authz off: achieved/s | Commits/cycle | CPU ms/cycle | Client resume p50 / p99 |
|---|---|---|---|---|---|---|---|---|
| 100 | 100 | 39.3 | 5.5 | 192 / 255 ms | 100 | 26.2 | 3.7 | 192 / 255 ms |
| 200 | 200 | 39.2 | 5.1 | 192 / 255 ms | 200 | 26.1 | 3.4 | 192 / 255 ms |
| 400 | 400 | 39.1 | 4.5 | 192 / 255 ms | 400 | 26.0 | 3.1 | 192 / 255 ms |
| 600 | 490 | 33.1 | 4.1 | 1536 / 2038 ms | 600 | 26.0 | 2.9 | 192 / 255 ms |
| 800 | 489 | 33.1 | 4.1 | 1535 / 2038 ms | 745 | 26.1 | 2.7 | 768 / 1019 ms |
| 1000 | 504 | 33.1 | 4.0 | 1520 / 2037 ms | 733 | 26.1 | 2.7 | 768 / 1019 ms |
| 1500 | 516 | 33.1 | 3.9 | 1514 / 2037 ms | 724 | 26.0 | 2.8 | 768 / 1019 ms |

- Below the ceiling a resume takes about 154 ms on the server, 150 ms of it fake-atelet, and about 192 ms at the client.
- CPU per cycle falls as the rate rises because ate-api-server's fixed background work is spread over more cycles.
- Postgres was never the limit: at most 1.84 cores (authz on) and 2.24 cores (authz off) of its 16-core limit, about 8 of 70 connections active, few lock or WAL-write waits.

## Why the ceiling is below the 2026-10-09 run

**Authorization accounts for most of it.**
- ate-setup enforces ateapi authorization by default since c4518a13 (2026-10-16), after the 2026-10-09 runs.
- With it on, each cycle runs 13 OpenFGA reads of its `tuple` table: 7 lookups of one tuple and 6 reads of a relation's tuples (`results/pg-statements-per-cycle.txt`). OpenFGA runs inside ate-api-server, so each check costs ate-api-server CPU as well as the Postgres reads.
- At 400/s, unsaturated in both runs, a cycle costs 4.5 ms of ate-api-server CPU with authorization and 3.1 ms without: authorization is about 1.4 ms per cycle, roughly a third of ate-api-server's work on this path. It takes the 2 CPU ceiling from about 735/s to about 500/s.

**Without authorization the Postgres work matches the earlier run.**
- 36 statements and 26 commits per cycle, against 35 and 25 on 2026-10-09.
- The one new statement is `SELECT proto, worker_name FROM worker_assignments WHERE actor_uid = $1`, once per cycle. One `GetActor` per cycle is gone, as `postgres-commits-per-cycle.md` notes for `main` after 2026-10-12.

**About 10% remains unexplained.**
- About 735/s now against about 815/s, or about 2.7 ms of CPU per cycle against about 2.45 ms. The earlier runs repeated within about 2%, so this is a real difference.
- Candidates, not measured:
  - `SuspendActor(ASYNCHRONOUS)` in place of `PauseActor` (#2790, #2813)
  - the extra `worker_assignments` read
  - the user class: resumepaused here, resumecold (a shared fleet of actors) on 2026-10-09
  - other ate-api-server changes since 2026-10-14
- CPU profiles of ate-api-server on both commits under the same load would locate it.

**The node type matters too.** The same code with authorization on reached about 310/s on `substrate-poc` and about 500/s on `nh-prow-parity`, at about 6.3 and 4.0 ms of CPU per cycle. On a c3-standard-4, 2 vCPUs are the two hyperthreads of one physical core, shared there with the rest of the control plane and GKE's system pods; on the mostly idle c3-highcpu-88 they are not. Compare runs on the same node type only.

## Caveats

- **Latency above the ceiling is bucket-limited.** Client and server latencies come from histograms with factor-of-2 buckets (`internal/benchmarking/boomer/metrics/prom.go`), so at saturation a percentile is an interpolated point inside a wide bucket: the authz-on run shows a server p50 of 2457 ms above a client p50 of 1536 ms on `substrate-poc`. Below the ceiling the values are sound.
- **The plateau is also closed-loop.** Each user waits for its resume and pause before the next, so once calls queue the users cannot offer more than users ÷ cycle time. What brings on the plateau is ate-api-server's CPU limit.
- **Runner CPU.** The `nh-prow-parity` runs gave the runner 3 CPU instead of 8; the `substrate-poc` run measured it at no more than 0.11 cores.

## Scripts

- `scripts/driver.py`: runs one `tests.yaml` entry through the orchestrator's steps from a local checkout, against the cluster in `$KUBECONFIG`, with the repo's `.ate-dev-env.sh`. `RUNNER_CPU` overrides the entry's `runnerCpu`; `RUNNER_NODE_SELECTOR=key=value` pins the runner Job.

  ```bash
  KUBECONFIG=<kubeconfig pinned to the cluster> \
    [ATE_API_EXPERIMENTAL_ENABLE_AUTHZ=false] RUNNER_CPU=3 RUNNER_NODE_SELECTOR=ate.dev/benchmarking=true \
    python3 scripts/driver.py <checkout> ateapi_cold_resume_pause_fake_2cpu gs://<bucket>/benchmark-results
  ```

- `scripts/install-only.py`: installs, shapes and deploys the workloads for one entry without a run; used to read the new statements' SQL text from `pg_stat_statements`.
