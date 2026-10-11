# The 25 Postgres commits in each resume and pause cycle

Every cold resume benchmark measures 25.0–25.1 Postgres commits per resume + pause cycle, at every rate, CPU size, replica count, Worker count and disk type (`summary.md`, `summary-size10.md`). This document accounts for all 25, from the code and from `pg_stat_statements`, and notes which of them cost a WAL flush.

- **Code:** branch `fake-dataplane-test` at `5af49ce9`. Paths are under `cmd/ateapi/internal/` unless shown in full.
- **Measurements:** `pg_stat_statements` and `pg_stat_database`, scraped by postgres-exporter into the benchmarking Prometheus during the 2026-10-10 size10 runs. These are the pd-ssd run (run_ts 1791638643), the pd-balanced run (1791597772) and the 10,000-Worker run (1791633612).
  - The exporter keeps only each statement's `queryid`, not its text, so the code maps IDs to statements.
  - The IDs are the same in all three runs, because each fresh install creates the schema the same way.

## Summary

- **25 commits per cycle: 13 for the resume and 12 for the pause.**
  - **23 single-statement autocommits.** The store runs each `pool.QueryRow`/`pool.Exec` as its own implicit transaction.
  - **2 explicit transactions of 4 statements each,** binding the actor to a Worker and releasing it.
- **31 statements per cycle, or 35 counting `BEGIN` and `COMMIT`.** `pg_stat_statements` measured 35.1 (35.0 plus about 0.08 of background polling).
- **15 of the 25 commits are read-only** (`SELECT`s in autocommit). A read-only transaction gets no transaction ID and writes no commit record, so it doesn't wait for a WAL flush.
- **The other 10 commits write**, and each waits for the WAL flush. Those 10 are what the commit-path ceiling in `summary-size10.md` counts against:
  - 2 lease acquires
  - 2 lease releases
  - 4 actor updates
  - the 2 Worker transactions
- **The actor row is read 11 times per cycle** (7 `GetActor`s plus the read half of 4 `UpdateActor`s) and written 4 times.

## How the store issues SQL

- **Pass-through store layer.** The workflows call `ServiceImpl` (`controlapi/service.go:91,106`), which passes straight through to `store/atepg.Persistence` for every method on these paths.
- **Autocommit by default.** Each `atepg` method is one `p.pool.QueryRow` or `p.pool.Exec`, and therefore one implicit transaction, with one exception below.
- **The only explicit transactions:** `writeAndAppendEvent` (`store/atepg/outbox.go:79-109`) runs `pool.Begin`, the callback's SQL, `INSERT INTO worker_outbox`, then `Commit`. Binding, releasing and updating a Worker go through it.
- **`UpdateActor` is two commits** (`store/atepg/actor.go:79-133`):
  - an autocommit `SELECT uid, version, proto FROM actors`
  - a separate autocommit `UPDATE actors … WHERE … version = $n`
  
  There's no transaction around them and no `FOR UPDATE`. The version guard in the `WHERE` clause provides optimistic concurrency.
- **Actor leases are rows** (`store/atepg/lease.go`):
  - acquire: `INSERT INTO leases … ON CONFLICT (key) DO UPDATE … WHERE expires_at <= clock_timestamp() RETURNING key` (`:120`)
  - release: `DELETE FROM leases WHERE key = $1 AND token = $2` (`:216`)
  - Both are writes.
- **Not on this path:** `LISTEN`/`NOTIFY`, `pg_notify` and advisory locks.

## Resume: 13 commits

A cold resume of a paused actor with a local snapshot, no volumes and no conflicts. Handler: `controlapi/actor.go:481`. Workflow: `controlapi/workflow_resume.go`.

| # | Operation | Code | SQL | Commit | Writes |
|---|---|---|---|---|---|
| 1 | `GetActor`, hot-path check before the lease | `workflow_resume.go:87` | `SELECT proto FROM actors` | autocommit | |
| 2 | Acquire the actor lease | `workflow.go:206,222` → `atepg/lease.go:120` | `INSERT INTO leases … ON CONFLICT DO UPDATE … RETURNING` | autocommit | yes |
| 3 | `GetActor` in `loadActorForResume` | `workflow_resume.go:160` | `SELECT proto FROM actors` | autocommit | |
| 4 | `GetActorTemplate` | `workflow_resume.go:175` → `atepg/actor_template.go:58` | `SELECT proto FROM actor_templates` | autocommit | |
| 5 | `FindWorkerHostingActor`, the stale-claim check | `workflow_resume.go:426` → `atepg/worker_assignment.go:321` | `SELECT worker_name FROM worker_assignments WHERE actor_uid = …`. It finds nothing, because the pause deleted the row. | autocommit | |
| – | Pick a Worker | `workflow_resume.go:491`, `scheduling/scheduling.go` | none: in memory, from the Worker cache | – | |
| 6 | `BindActorToWorker` | `workflow_resume.go:524` → `atepg/worker_assignment.go:72-187` | `BEGIN`; `SELECT proto FROM workers WHERE name = … FOR UPDATE`; `INSERT INTO worker_assignments … ON CONFLICT (actor_uid) DO NOTHING`; `UPDATE workers SET version, proto …`; `INSERT INTO worker_outbox`; `COMMIT` | explicit, 4 statements | yes |
| 7–8 | `UpdateActor` to RESUMING, with the assignment | `workflow_resume.go:539` | `SELECT … FROM actors`, then `UPDATE actors` | 2 autocommits | the `UPDATE` |
| 9 | Restore RPC. fake-atelet mints the actor's certificate, and the mint reads the actor. | `workflow_resume.go:701-728`; `cmd/benchmarking/isolate/fake-atelet/herder.go:97-108,161-163` → `workerservice/certificate.go:53` | `SELECT proto FROM actors` | autocommit | |
| 10 | `GetActor` in `finalizeRunning` | `workflow_resume.go:797` | `SELECT proto FROM actors` | autocommit | |
| 11–12 | `UpdateActor` to RUNNING | `workflow_resume.go:802` | `SELECT … FROM actors`, then `UPDATE actors` | 2 autocommits | the `UPDATE` |
| 13 | Release the lease | `atepg/lease.go:65-85,216` | `DELETE FROM leases` | autocommit | yes |

**Totals:** 13 commits (5 writing), 16 statements, 18 counting `BEGIN` and `COMMIT`.

**Why the mint happens (row 9):**
- It runs only when the request names an egress gateway (`herder.go:98`).
- The install sets `--default-egress-gateway-address` (`manifests/ate-install/ate-api-server.yaml:106`).
- fake-atelet's `--mint-actor-certificate` defaults to true (`cmd/benchmarking/isolate/fake-atelet/main.go:75`).

A real ateom mints through atelet in the same way, so this commit is part of production's cost too.

## Pause: 12 commits

Handler: `controlapi/actor.go:459`. Workflow: `controlapi/workflow_pause.go`. Unlike the resume, the pause doesn't read the actor before taking the lease.

| # | Operation | Code | SQL | Commit | Writes |
|---|---|---|---|---|---|
| 1 | Acquire the actor lease | `workflow_pause.go:53` | `INSERT INTO leases … ON CONFLICT DO UPDATE … RETURNING` | autocommit | yes |
| 2 | `GetActor` in `loadActorForPause` | `workflow_pause.go:100` | `SELECT proto FROM actors` | autocommit | |
| 3 | `GetActorTemplate` | `workflow_pause.go:104` | `SELECT proto FROM actor_templates` | autocommit | |
| 4–5 | `UpdateActor` to PAUSING, with the snapshot name | `workflow_pause.go:134` | `SELECT … FROM actors`, then `UPDATE actors` | 2 autocommits | the `UPDATE` |
| – | Checkpoint RPC | `workflow_pause.go:200` | none. fake-atelet doesn't mint on Checkpoint (`herder.go:171-183`). | – | |
| 6 | `GetActor` in `ensurePausedFinalized` | `workflow_pause.go:217` | `SELECT proto FROM actors` | autocommit | |
| 7 | `GetWorker` | `workflow_pause.go:224` → `atepg/worker.go:61,75` | `SELECT proto FROM workers` | autocommit | |
| 8 | `ReleaseActorFromWorker` | `workflow_pause.go:233` → `atepg/worker_assignment.go:189-234` | `BEGIN`; `SELECT … FROM workers … FOR UPDATE`; `DELETE FROM worker_assignments … RETURNING proto`; `UPDATE workers`; `INSERT INTO worker_outbox`; `COMMIT` | explicit, 4 statements | yes |
| 9 | `GetActor`, the re-read after the release | `workflow_pause.go:243` | `SELECT proto FROM actors` | autocommit | |
| 10–11 | `UpdateActor` to PAUSED, setting the local snapshot and clearing the assignment | `workflow_pause.go:269` | `SELECT … FROM actors`, then `UPDATE actors` | 2 autocommits | the `UPDATE` |
| 12 | Release the lease | `atepg/lease.go:216` | `DELETE FROM leases` | autocommit | yes |

**Totals:** 12 commits (5 writing), 15 statements, 17 counting `BEGIN` and `COMMIT`.

## The measurement

Statement calls per cycle in the pd-ssd run's 5000/s step: 725,007 cycles in a 145 s window, 25.06 commits per cycle. Mean time is execution time inside Postgres. "Ceiling" is the run's 7000/s step, and "10k Workers" is the 3000/s step of the 10,000-Worker run.

| queryid | Calls per cycle | Rows per call | Mean at 5000/s | Mean at ceiling | Mean, 10k Workers | Statement |
|---|---|---|---|---|---|---|
| `2746378599054124034` | 7.0 | 1 | 31 µs | 33 µs | 27 µs | `SELECT proto FROM actors` (`GetActor`) |
| `-6555171763657768259` | 4.0 | 1 | 46 µs | 53 µs | 46 µs | `UPDATE actors` (likely; the costlier of the two 4/cycle IDs) |
| `-4441328662304304489` | 4.0 | 1 | 26 µs | 29 µs | 22 µs | `SELECT uid, version, proto FROM actors` (likely) |
| `-7642016025207542908` | 2.0 | 1 | 53 µs | 85 µs | 28 µs | most likely `SELECT … FROM workers … FOR UPDATE`; see below |
| `-3065665242670061812` | 2.0 | 1 | 66 µs | 75 µs | 56 µs | one of: lease upsert, lease delete, `UPDATE workers`, outbox insert |
| `-8511877388419603168` | 2.0 | 1 | 35 µs | 36 µs | 28 µs | one of the same four |
| `-3542883333257437982` | 2.0 | 1 | 35 µs | 38 µs | 31 µs | one of the same four |
| `-3154300369275581113` | 2.0 | 1 | 24 µs | 29 µs | 20 µs | one of the same four |
| `-7605597855520435171` | 2.0 | 1 | 17 µs | 19 µs | 15 µs | `SELECT proto FROM actor_templates` (likely; the cheapest) |
| `4854991825702068830` | 2.0 | 0 | 0.7 µs | 0.7 µs | 0.6 µs | `BEGIN` or `COMMIT` |
| `6097083398544187049` | 2.0 | 0 | 0.8 µs | 0.8 µs | 0.7 µs | `COMMIT` or `BEGIN` |
| `1815471240457602673` | 1.0 | 1 | 98 µs | 111 µs | 86 µs | `INSERT INTO worker_assignments` or `DELETE … RETURNING` |
| `-6107092359095529422` | 1.0 | 1 | 52 µs | 47 µs | 37 µs | the other of those two |
| `-7250248760999745136` | 1.0 | 1 | 22 µs | 25 µs | 20 µs | `SELECT proto FROM workers` (`GetWorker`, likely) |
| `-8081109363834187016` | 1.0 | 0 | 22 µs | 24 µs | 19 µs | `FindWorkerHostingActor`: its one call per cycle finds no row |
| `7601299121316520603` | 0.04 | 499 | 1.0 ms | 1.5 ms | 0.6 ms | background: the WatchWorkers outbox poll (see below) |
| `1397548843350673252` | 0.04 | 1 | 26 µs | 28 µs | 25 µs | background: the poll's second query |

- **The counts match the code exactly:**
  - 7 actor reads, 4 actor read-and-update pairs
  - 8 statements at 2 per cycle (2 lease statements, the template read, the 3 Worker-transaction statements, and `BEGIN`/`COMMIT`)
  - 4 at 1 per cycle (the assignment insert and delete, `GetWorker`, `FindWorkerHostingActor`)
  
  That's 31 data statements plus 4 utility statements.
- **What's certain and what isn't.** Calls per cycle and rows per call pin down `GetActor`, `BEGIN`/`COMMIT` and `FindWorkerHostingActor`. The other IDs share a count, so their statements are matched by cost and marked "likely" or "one of". Capturing `pg_stat_statements` text in a live run, before teardown, would settle them.
- **`-7642016025207542908` is the only statement whose cost rises sharply with load and falls with more Workers:**
  - 53 µs at 5000/s, 85 µs at the ceiling (+62%)
  - 28 µs with 10× the Workers, where each Worker hosts a tenth as many actors
  
  That pattern matches a row lock on a hot Worker row: the `SELECT … FOR UPDATE` that both Worker transactions take. It's also the statement `ateapi-cold-resume-2cpu-experiment.md` saw grow with a lock waiter at 1500/s.
- **Execution time is a small part of Postgres's CPU.** The statements' execution time adds up to about 6.0 s per second at 5000/s, or 1.2 ms per cycle, against 3.95 ms of Postgres CPU per cycle. The rest goes to per-call work outside execution: protocol, parse and bind, transaction start and commit, and WAL.

## Background, not per cycle

- **WatchWorkers poll.** Every 50 ms, each ate-api-server replica polls the outbox with a two-query batch on its watch pool (`store/atepg/outbox.go:113,472-475`). That's one implicit transaction, so about 20 commits/s per replica, regardless of cycle rate. At 10 replicas and 5000 cycles/s, it adds 0.04 commits per cycle, the 0.06 above 25.0.
- **Lease renewal never fires.** The lease TTL is 30 s and renewal runs every 10 s (`store/atepg/lease.go:31,143-165`), but a resume or pause holds its lease for milliseconds to about 160 ms.
- **Outbox rows: 2 per cycle,** inside the two Worker transactions and already counted. ate-api-server also publishes them in process (`outbox.go:105`).
- **Worker assignment reconciler:** no database work here. It acts only when a Worker's epoch moves ahead of its observed epoch.
- **Periodic jobs:**
  - Worker cache relist every 5 minutes
  - template reconciler every 20 s
  - outbox partition and expired-lease maintenance every minute
  - fake-workersync's Worker re-registration (a `GetWorker` and an `UpdateWorker` transaction) every 10 s or so per Worker
  
  None of these depends on the cycle rate.
- **Authorization:** no database reads. `--experimental-enable-authz` is off by default. With it on, OpenFGA would read tuples from the same pool on each RPC.

## Which commits wait for the WAL flush

| Kind | Per cycle | Flushes the WAL |
|---|---|---|
| Read-only autocommits: 7 `GetActor`, 4 `UpdateActor` reads, 2 `GetActorTemplate`, 1 `FindWorkerHostingActor`, 1 `GetWorker` | 15 | no: no transaction ID, no commit record |
| Lease acquire and release | 4 | yes |
| `UPDATE actors` | 4 | yes |
| Bind and release transactions | 2 | yes |
| **Total** | **25** | **10** |

- **Write commits are about 70,000 per second at the ceiling.** In the pd-ssd run, about 1,250–1,300 WAL fsyncs/s each covered about 135 commits, about 175,000 commits/s in total (`summary-size10.md`). Only the write commits wait for those fsyncs, so the queue behind each flush is about 54 write commits per fsync.
- **What this changes about "fewer commits per cycle".** Removing read-only commits saves round trips and CPU on both sides, but doesn't touch the WAL flush. Only removing *write* commits raises the commit-path ceiling.

## Where commits could be removed

These are leads for reducing the count, not tested changes. Each touches correctness, so each needs a design review.

| Change | Commits saved per cycle | Write commits saved | Notes |
|---|---|---|---|
| `UpdateActor` as one `UPDATE … WHERE version = $n RETURNING` instead of a read then a write | 4 | 0 | Callers mutate the decoded proto, so the read-modify-write has to move into one statement or a transaction. |
| Reuse the actor read inside a workflow instead of re-reading it (the resume reads it 4 times, the pause 3) | up to 5 | 0 | Some re-reads exist to see state changed by the RPC in between. Check each one. |
| Cache actor templates, which the template reconciler already tracks | 2 | 0 | Needs invalidation when a template changes. |
| Fold the actor `UPDATE` that follows a bind or release into that transaction | 2 | 2 | RESUMING with the bind, PAUSED with the release. |
| Replace the per-call lease rows with the version guard the actor `UPDATE`s already carry, or hold the lease in memory with a fencing token | up to 4 | up to 4 | The lease serializes concurrent operations on one actor; whatever replaces it must too. |

- **Read-side changes** (rows 1–3) could remove up to 11 of the 15 read-only commits. That saves round trips and CPU on both sides, but leaves the commit-path ceiling where it is.
- **Write-side changes** (rows 4–5) could cut the write commits from 10 to 4–6, which would raise the commit-path ceiling by roughly 1.7–2.5×. That estimate assumes commits per flush stay about the same.

## Assumptions behind the count

- **The happy path.** A version conflict in `UpdateActor` or in the bind adds a `GetActor` and re-runs the assignment attempt (`workflow_resume.go:274-294,550`). No run reported conflicts in volume, and commits per cycle held at 25.0–25.1 under saturation.
- **No volumes.** Resuming an actor with pending volumes adds one more `UpdateActor` (2 commits).
- **The pause's re-read.** If `GetWorker` finds no Worker, the release and the second `GetActor` are skipped.
- **Minor sources:** pgxpool's ping of a connection idle for over 1 s, and first-use statement prepares. Both add near zero per cycle under steady load.
