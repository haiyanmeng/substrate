# 6 × 8 CPU cold resume on size10 Postgres with the WAL on its own volume, 2026-10-19

- Test `ateapi_cold_resume_pause_fake_6x8cpu_size10_walvol` (`tests.yaml`): the 6 × 8 size10 test with `track_io_timing=on`, as in the traced run, renamed so its results stay apart. Run with the local orchestrator, 13:49–14:34 UTC, ladder 13:57:52–14:30:18. Complete; none of about 27.0 million requests failed (13,482,614 cold resumes, 13,476,131 pauses).
- Code: local branch `ateapi-cold-wal-volume` at `974afc34`. That is `463168aa` on `ateapi-cold-resume-capacity` plus one commit, `wal-volume-patch.txt`, which changes `manifests/ate-install/postgres/postgres.yaml` to:
  - add a second 500Gi claim, `wal`, from the cluster default StorageClass (pd-balanced, as `data`);
  - mount it at `/var/lib/postgresql/wal`;
  - set `POSTGRES_INITDB_WALDIR=/var/lib/postgresql/wal/pg_wal`, so `initdb` makes `pg_wal` a symlink into it.

  The branch is not pushed.
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`). Postgres on `gke-nh-prow-parity-default-pool-ca559d59-9v65`, data files on `nvme0n3`, WAL on `nvme0n2` (`data/wal-layout.txt`).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_6x8cpu_size10_walvol/run_date=2026-10-19/run_ts=1792418241/`; local copy in `results/6x8walvol/`. `data/steps.jsonl` comes from `benchmarking/automation/analyze_ladder.py`. `data/wal-io.csv` and `data/io.jsonl` come from the samplers; `data/io.jsonl` holds both volumes' `/proc/diskstats` lines (`diskstats` for data, `wal_diskstats` for WAL).

**Result: the highest 6 × 8 ceiling so far, about 7,650 cycles/s, with a faster WAL flush. One run cannot yet tell the layout from a faster volume.**

- **The ceiling is about 7,650 cycles/s** (7,564–7,718 over the 8000–9500/s steps). The pd-balanced runs with WAL and data on one volume reached about 6,690 to 7,400, and Hyperdisk Extreme about 6,800.
- **The WAL flush still sets it**, busy 92–93% of each second. At the ceiling each fdatasync took 0.61–0.65 ms and carried 122–127 commits, so commits/s reached about 185,000. That is 4% above the best shared-volume 6 × 8 run and 15% above the traced run.
- **The WAL write itself is faster:** about 0.53 ms at the ceiling on the WAL volume, against about 0.63 ms in the traced run on a shared volume. The flush is about 0.035 ms, as before.
- **The data files barely use their volume:** 17–49 writes a second, at most 2% busy. The WAL volume carries essentially all the I/O.
- **The last step dipped to 7,319.** The fdatasync rose to 0.73 ms because the WAL volume's own write latency rose to 0.65 ms. A volume's latency can drift within a run.

**Why this does not yet credit the layout:** shared-volume runs have already ranged from 0.53 to 0.72 ms per fdatasync. The 5 × 8 run of 2026-10-16 (`../ateapi-cold-5x8-size10-neighbors-20261016/summary.md`) had 0.53–0.57 ms with WAL and data on one volume, on the same node. The traced run (`../ateapi-cold-6x8-traced-20261018/summary.md`) also found that data-file writes do not slow the WAL write. So this result sits at the good end of what one shared volume can do. Alternating repeats of both layouts, run at the same code, would separate the two.

**Repeats since:** five alternating runs later the same day (`../ateapi-cold-6x8-layout-repeats-20261019/summary.md`) found no layout gain on the same node. Three shared-volume runs on `…-9v65` reached 7,503–7,524 cycles/s at 0.61–0.63 ms per fdatasync, a faster flush than this run's 0.644 ms. Runs varied mainly with the node Postgres landed on, or the disk it got there.

## Comparison with the other 6 × 8 runs

| Run | Postgres storage | Ceiling (cycles/s) | Mean fdatasync at the ceiling | Commits per fdatasync | Summary |
|---|---|---|---|---|---|
| 2026-10-15 | pd-balanced, shared | about 7,400 (7,358–7,434) | 0.66–0.68 ms | 127–130 | `../ateapi-cold-resume-summary.md` |
| 2026-10-17 | pd-balanced, shared | about 7,150 (6,948–7,219) | not sampled | not sampled | samples lost to a workstation suspend |
| 2026-10-18 | pd-balanced, shared | about 6,690 (6,631–6,748) | 0.70–0.72 ms | 121–125 | `../ateapi-cold-6x8-traced-20261018/summary.md` |
| 2026-10-19 | Hyperdisk Extreme, shared | about 6,800 (6,463–7,144) | 0.72–0.83 ms | 131–138 | `../ateapi-cold-6x8-hde-20261019/summary.md` |
| 2026-10-19 | pd-balanced, separate WAL volume | about 7,650 (7,564–7,718) | 0.61–0.65 ms | 122–127 | this file |

## Per step

From `data/steps.jsonl` and `data/wal-io.csv`. Flush busy is fsync time per second.

| Target | Resume/s | Server mean (ms) | Server p50 / p99 (ms) | ateapi cores | Postgres cores | Active PG conns | Commits/s | fsyncs/s | Mean fsync | Commits per fsync | Flush busy |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5,000 | 164 | 154 / 204 | 26.1 | 18.2 | 97 | 120,260 | 1,611 | 0.57 ms | 74 | 92% |
| 5500 | 5,474 | 191 | 156 / 1,345 | 27.9 | 19.2 | 91 | 131,960 | 1,595 | 0.58 ms | 83 | 92% |
| 6000 | 6,008 | 174 | 156 / 395 | 30.6 | 23.1 | 116 | 144,199 | 1,555 | 0.59 ms | 93 | 92% |
| 6500 | 6,465 | 206 | 159 / 1,414 | 32.9 | 26.0 | 112 | 155,292 | 1,553 | 0.60 ms | 100 | 93% |
| 7000 | 6,932 | 231 | 164 / 2,000 | 35.3 | 28.5 | 129 | 166,356 | 1,516 | 0.61 ms | 110 | 93% |
| 7500 | 7,406 | 248 | 167 / 2,003 | 37.2 | 30.1 | 117 | 177,730 | 1,520 | 0.61 ms | 117 | 93% |
| 8000 | 7,668 | 971 | 819 / 3,179 | 39.7 | 32.0 | 156 | 184,446 | 1,502 | 0.62 ms | 123 | 93% |
| 8500 | 7,718 | 966 | 384 / 5,559 | 38.2 | 32.0 | 134 | 185,095 | 1,528 | 0.61 ms | 122 | 93% |
| 9000 | 7,699 | 972 | 336 / 5,945 | 39.5 | 32.0 | 132 | 184,785 | 1,495 | 0.62 ms | 123 | 92% |
| 9500 | 7,564 | 991 | 297 / 3,854 | 38.5 | 31.0 | 160 | 181,812 | 1,422 | 0.65 ms | 127 | 92% |
| 10000 | 7,319 | 1,024 | 339 / 3,264 | 37.9 | 30.1 | 157 | 175,859 | 1,269 | 0.73 ms | 138 | 93% |

Both volumes' `/proc/diskstats` (`data/io.jsonl`), over the same windows:

| Target | WAL writes/s | WAL MiB/s | WAL mean write | WAL flushes/s | WAL mean flush | WAL busy | Data writes/s | Data MiB/s | Data busy |
|---|---|---|---|---|---|---|---|---|---|
| 5000 | 1,735 | 67.6 | 0.60 ms | 1,627 | 0.033 ms | 87% | 18 | 3.5 | 0% |
| 5500 | 1,693 | 72.4 | 0.62 ms | 1,574 | 0.033 ms | 86% | 42 | 8.6 | 1% |
| 6000 | 1,686 | 78.4 | 0.63 ms | 1,555 | 0.035 ms | 86% | 17 | 3.4 | 1% |
| 6500 | 1,586 | 55.1 | 0.55 ms | 1,561 | 0.034 ms | 87% | 45 | 9.3 | 2% |
| 7000 | 1,528 | 50.4 | 0.53 ms | 1,532 | 0.035 ms | 87% | 46 | 9.6 | 2% |
| 7500 | 1,528 | 52.9 | 0.53 ms | 1,532 | 0.035 ms | 87% | 49 | 9.8 | 2% |
| 8000 | 1,505 | 54.7 | 0.53 ms | 1,505 | 0.036 ms | 86% | 20 | 4.2 | 0% |
| 8500 | 1,517 | 54.9 | 0.52 ms | 1,516 | 0.036 ms | 86% | 18 | 3.4 | 0% |
| 9000 | 1,498 | 54.7 | 0.53 ms | 1,497 | 0.035 ms | 86% | 24 | 4.8 | 1% |
| 9500 | 1,428 | 53.4 | 0.56 ms | 1,428 | 0.037 ms | 87% | 32 | 6.5 | 1% |
| 10000 | 1,286 | 51.1 | 0.65 ms | 1,289 | 0.037 ms | 88% | 36 | 7.4 | 1% |

`/proc/diskstats` counts each flush's empty request as a write. Writes/s subtracts them (writes minus flushes), and the WAL mean write subtracts their time, taken as the flush time. With the data files elsewhere, the WAL volume's writes are WAL writes, about one per flush. The extra writes at 5000–6000/s are new 16 MiB WAL segments being created, 1.7–2.1 a second (`pg_stat_io` WAL `init` writes); creation tapered at 6500/s and stopped from 7000/s, when Postgres recycled segments instead.

## How it was run

1. Create the branch: `git checkout -b ateapi-cold-wal-volume 463168aa && git am wal-volume-patch.txt`. The orchestrator clones the branch named by `go.sh`'s `--branch`, so the commit need only exist locally. A worktree avoids switching the main checkout.
2. `systemd-inhibit --what=sleep:idle --who=bench --why="cold resume WAL volume benchmark" --mode=block ./go.sh` keeps the workstation awake. `go.sh` starts the two samplers and runs the orchestrator (`run.py`, `tests.yaml`, `target-clusters/dev.sh`):
   - `sample-io.sh` writes every 15 s from inside postgres-0: `pg_stat_io`, `pg_stat_checkpointer`, `pg_stat_wal`, commits, and the `/proc/diskstats` lines of the volumes mounted at `/var/lib/postgresql/data` and `/var/lib/postgresql/wal`.
   - `sample-wal-io.sh` writes WAL counters and wait events every 15 s.

   The scripts carry absolute paths under `~/bench-runs/ateapi-cold-6x8-walvol-20261019/`; change them to rerun elsewhere.
3. After Postgres starts, confirm the layout: `kubectl -n ate-system get pvc` shows `data-postgres-0` and `wal-postgres-0`, and `ls -ld $PGDATA/pg_wal` in the postgres container shows the symlink (`data/wal-layout.txt`).
4. Download the results into `results/6x8walvol/`, port-forward Prometheus (`kubectl -n benchmarking port-forward svc/prometheus 19090:9090`), and run `python3 benchmarking/automation/analyze_ladder.py results/6x8walvol > data/steps.jsonl`.
5. The orchestrator's teardown deletes `ate-system`, and with it both disks.
