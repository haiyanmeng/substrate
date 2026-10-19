# Shared vs separate WAL volume: alternating 6 × 8 CPU cold resume repeats on size10 Postgres, 2026-10-19

Five back-to-back runs of the 6 × 8 size10 cold resume test, 14:41–18:35 UTC. They alternate Postgres's two storage layouts:

- **shared:** WAL and data files on one 500Gi pd-balanced volume, the installed default;
- **separate:** the WAL on its own 500Gi pd-balanced volume (`../ateapi-cold-6x8-walvol-20261019/wal-volume-patch.txt`).

With the first separate-WAL run, earlier the same day (`../ateapi-cold-6x8-walvol-20261019/summary.md`), that gives three runs of each layout.

- Test: the traced run's 6 × 8 entry (`../ateapi-cold-6x8-traced-20261018/tests.yaml`, with `track_io_timing=on`); separate-WAL runs carry the suffix `_walvol` (`r*/tests.yaml`).
- Code: the shared runs used `ateapi-cold-resume-capacity`, at `463168aa` for r1 and `f2f9f2a9` for r3 and r5; between the two, only benchmark summaries changed. The separate-WAL runs used the local branch `ateapi-cold-wal-volume` at `974afc34`, which is `463168aa` plus the patch.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`). The orchestrator picks Postgres's node itself; each run's node and disks are in `r*/postgres-disks.txt`. Every run got new disks.
- Results: each run's GCS path is in its orchestrator log; local copies in `r*/results/`.

**Result: on the same node the separate WAL volume made no measurable difference. Runs varied about 10% by the node Postgres landed on, or the disk it got there.**

- **Shared, node `…-9v65`, three runs:** 7,503, 7,524 and 7,512 cycles/s, within 0.3% of each other, at 0.61–0.63 ms per WAL fdatasync.
- **Separate, node `…-9v65`, one run** (the first, earlier the same day): 7,594 cycles/s. Its fdatasync was *slower*, at 0.644 ms; it reached a 1% higher ceiling by packing a few more commits into each flush.
- **Separate, node `…-4d75`, two runs:** 7,424 and 6,951 cycles/s, at 0.674 and 0.822 ms. The traced shared-volume run of 2026-10-18 also ran on `…-4d75`, reaching 6,690 cycles/s at about 0.70 ms (`../ateapi-cold-6x8-traced-20261018/summary.md`).
- **The data files barely load their own volume** in the separate-WAL runs: 15–59 writes a second, at most 2% busy. So separating them removes almost nothing from the WAL's volume. This agrees with the traced run's finding that data-file writes do not slow the WAL write.
- **A separate WAL volume is not worth the second claim.** The variation that matters is between nodes or disks: the same configuration has ranged from about 6,690 to 7,600 cycles/s. One 6 × 8 run is therefore good to about ±5%.

**Not separated:** node from disk. Each run gets new disks, so a slow node and a slow disk on that node look the same. Postgres used both layouts only on `…-9v65`; on `…-4d75` this set has only separate-WAL runs, plus the traced shared run from the day before.

## Runs

Over the saturated steps, 8000–10000/s. Ceiling is the mean cold resumes/s, with the step range in brackets. fdatasync, fsyncs/s, commits per fdatasync and commits/s are over the same window, from each run's `wal-io.csv`.

| Run | Layout | Postgres node | Ceiling (cycles/s) | Mean fdatasync | fsyncs/s | Commits per fdatasync | Commits/s | Requests | Failed |
|---|---|---|---|---|---|---|---|---|---|
| r1 | shared | `…-9v65` | 7,503 (7,456–7,537) | 0.630 ms | 1,478 | 122 | 180,086 | 26,894,331 | 0 |
| r3 | shared | `…-9v65` | 7,524 (7,480–7,603) | 0.620 ms | 1,496 | 121 | 180,276 | 26,962,260 | 1 |
| r5 | shared | `…-9v65` | 7,512 (7,494–7,546) | 0.613 ms | 1,514 | 119 | 180,485 | 26,947,265 | 0 |
| walvol | separate | `…-9v65` | 7,594 (7,319–7,718) | 0.644 ms | 1,439 | 126 | 181,323 | 27,000,745 | 0 |
| r2 | separate | `…-4d75` | 7,424 (7,346–7,513) | 0.674 ms | 1,374 | 130 | 178,478 | 26,779,572 | 0 |
| r4 | separate | `…-4d75` | 6,951 (6,903–7,012) | 0.822 ms | 1,141 | 146 | 167,077 | 25,779,072 | 1 |

The two failures were each one `ResourceExhausted: no worker has room for the actor` on a cold resume, as in the 2026-10-17 run; they are unrelated to storage.

On the separate-WAL runs' WAL volumes, the write itself took 0.52–0.65 ms at the ceiling on `…-9v65`, 0.58–0.62 ms in r2 and 0.72–0.74 ms in r4, with flushes of 0.035–0.040 ms throughout. Per-step tables for every run, including both volumes' `/proc/diskstats`, are in `r*/tables.txt`.

## How it was run

1. The separate-WAL branch: `git checkout -b ateapi-cold-wal-volume 463168aa && git am ../ateapi-cold-6x8-walvol-20261019/wal-volume-patch.txt`. It need only exist locally; the orchestrator clones it from the local repository.
2. `systemd-inhibit --what=sleep:idle --who=bench --why="cold resume layout repeats" --mode=block ./go.sh` keeps the workstation awake through all five runs. `go.sh` calls `common/run-one.sh DIR TESTNAME BRANCH` for each in turn. It carries absolute paths under `~/bench-runs/ateapi-cold-6x8-layout-repeats-20261019/`; change them to rerun elsewhere.
3. `common/run-one.sh` writes the run's `run.py` and `tests.yaml` and starts three helpers, then runs the orchestrator with `common/target-clusters/dev.sh`:
   - `common/sample-io.sh` samples every 15 s from inside postgres-0: `pg_stat_io`, `pg_stat_checkpointer`, `pg_stat_wal`, commits, and the `/proc/diskstats` lines of the volumes at `/var/lib/postgresql/data` and, when present, `/var/lib/postgresql/wal`.
   - `common/sample-wal-io.sh` samples WAL counters and wait events every 15 s.
   - `common/record-disks.sh` records Postgres's node, claims, disk types and the `pg_wal` layout once Postgres is ready.
4. After each run, with Prometheus port-forwarded (`kubectl -n benchmarking port-forward svc/prometheus 19090:9090`), `common/analyze-run.sh DIR` downloads the results, runs `benchmarking/automation/analyze_ladder.py`, and writes `tables.txt` with `common/tables.py`. `/proc/diskstats` counts each flush's empty request as a write; `tables.py` subtracts them, and gives a mean write only for a volume holding the WAL alone.
