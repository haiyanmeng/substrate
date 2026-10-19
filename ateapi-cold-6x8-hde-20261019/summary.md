# 6 × 8 CPU cold resume on size10 Postgres with its volume on Hyperdisk Extreme, 2026-10-19

- Test `ateapi_cold_resume_pause_fake_6x8cpu_size10_hde` (`tests.yaml`): the 6 × 8 size10 test with `track_io_timing=on`, as in the traced run, renamed so its results stay apart. Run with the local orchestrator on branch `ateapi-cold-resume-capacity` at `4912a4d7`, 12:42–13:26 UTC, ladder 12:49:36–13:22:01. Complete; none of about 25.4 million requests failed (12,672,927 cold resumes, 12,666,489 pauses).
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`). Postgres on `gke-nh-prow-parity-default-pool-ca559d59-9v65`. Its data volume was a new 500 GB Hyperdisk Extreme disk with 50,000 provisioned IOPS (`data/postgres-disk.txt`, from `gcloud compute disks describe`), created from the StorageClass in `sc.yaml`.
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_6x8cpu_size10_hde/run_date=2026-10-19/run_ts=1792414144/`; local copy in `results/6x8hde/`. `data/steps.jsonl` comes from `benchmarking/automation/analyze_ladder.py`. `data/wal-io.csv` and `data/io.jsonl` come from the samplers below.

**Result: Hyperdisk Extreme did not raise the ceiling, and its WAL fdatasync was slower than on pd-balanced.**

- **The ceiling is about 6,800 cycles/s** (6,463–7,144 over the 7000–10000/s steps). That is inside the range of the three pd-balanced 6 × 8 runs, about 6,690 to 7,400.
- **The WAL flush still sets it**, busy 93–95% of each second at every step. At the ceiling the fdatasync took 0.72–0.83 ms, against 0.66–0.72 ms on pd-balanced. Postgres packed more commits into each fdatasync (131–138 against 121–130), so commits/s, and with it the ceiling, came out about the same.
- **Per-write latency is higher on this volume.** At the light steps, 5000–5500/s, the fdatasync took 1.03–1.09 ms, against 0.67–0.79 ms on pd-balanced at the same load in the traced run (`../ateapi-cold-6x8-traced-20261018/summary.md`).
- **The flush itself costs about 0.04 ms, as on pd-balanced** (`/proc/diskstats`); the cost is in the write.
- **This matches the microbenchmark** (`../fsync-microbenchmark-20261016/summary.md`, follow-up of 2026-10-17). No Hyperdisk configuration beat pd-balanced for one write and flush at a time. Hyperdisk Extreme varied by volume, 0.39 ms on one and 0.69 ms on the other; this volume behaves like the slower one.
- **The ceiling wobbled more than in the pd-balanced runs**, from 6,463 to 7,144. Its highest steps, the last two, are where the fdatasync fell to about 0.72 ms.

**Limits:** one volume, one run. The microbenchmark's two Hyperdisk Extreme volumes differed by 1.8×. This run shows that the disk type does not reliably help; it does not show what a typical Hyperdisk Extreme volume would do.

## Comparison with the pd-balanced 6 × 8 runs

| Run | Postgres volume | Ceiling (cycles/s) | Mean fdatasync at the ceiling | Commits per fdatasync | Summary |
|---|---|---|---|---|---|
| 2026-10-15 | pd-balanced | about 7,400 (7,358–7,434) | 0.66–0.68 ms | 127–130 | `../ateapi-cold-resume-summary.md` |
| 2026-10-17 | pd-balanced | about 7,150 (6,948–7,219) | not sampled | not sampled | samples lost to a workstation suspend |
| 2026-10-18 | pd-balanced | about 6,690 (6,631–6,748) | 0.70–0.72 ms | 121–125 | `../ateapi-cold-6x8-traced-20261018/summary.md` |
| 2026-10-19 | Hyperdisk Extreme, 50,000 IOPS | about 6,800 (6,463–7,144) | 0.72–0.83 ms | 131–138 | this file |

## Per step

From `data/steps.jsonl` and `data/wal-io.csv`. Flush busy is fsync time per second.

| Target | Resume/s | Server mean (ms) | Server p50 / p99 (ms) | ateapi cores | Postgres cores | Active PG conns | Commits/s | fsyncs/s | Mean fsync | Commits per fsync | Flush busy |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 5,000 | 174 | 156 / 370 | 24.9 | 18.9 | 119 | 120,091 | 921 | 1.03 ms | 131 | 94% |
| 5500 | 5,507 | 196 | 167 / 442 | 28.0 | 21.3 | 160 | 132,150 | 864 | 1.09 ms | 153 | 94% |
| 6000 | 6,007 | 200 | 173 / 407 | 31.0 | 22.1 | 121 | 144,305 | 1,207 | 0.77 ms | 119 | 93% |
| 6500 | 6,501 | 201 | 168 / 702 | 33.0 | 25.8 | 128 | 156,214 | 1,330 | 0.71 ms | 118 | 94% |
| 7000 | 6,613 | 934 | 593 / 3,554 | 34.8 | 25.2 | 181 | 158,548 | 1,161 | 0.81 ms | 136 | 94% |
| 7500 | 6,463 | 1,156 | 1,190 / 3,231 | 34.2 | 26.9 | 171 | 154,741 | 1,137 | 0.83 ms | 138 | 95% |
| 8000 | 6,873 | 1,087 | 819 / 3,229 | 36.3 | 28.4 | 159 | 165,130 | 1,220 | 0.77 ms | 135 | 94% |
| 8500 | 6,791 | 1,107 | 388 / 3,785 | 35.5 | 27.5 | 173 | 161,724 | 1,208 | 0.78 ms | 137 | 94% |
| 9000 | 6,726 | 1,114 | 362 / 3,255 | 35.2 | 28.3 | 134 | 161,324 | 1,166 | 0.80 ms | 138 | 94% |
| 9500 | 7,144 | 1,046 | 328 / 3,231 | 37.2 | 29.3 | 156 | 171,616 | 1,304 | 0.72 ms | 132 | 94% |
| 10000 | 7,053 | 1,059 | 357 / 3,229 | 36.8 | 29.0 | 147 | 169,512 | 1,289 | 0.73 ms | 131 | 94% |

The volume's `/proc/diskstats` (`data/io.jsonl`), over the same windows:

| Target | Writes/s, flushes excluded | MiB/s | Flushes/s | Mean flush | Busy |
|---|---|---|---|---|---|
| 5000 | 1,067 | 69.1 | 924 | 0.047 ms | 91% |
| 5500 | 1,016 | 73.0 | 868 | 0.043 ms | 91% |
| 6000 | 1,376 | 82.2 | 1,212 | 0.038 ms | 88% |
| 6500 | 1,413 | 63.7 | 1,342 | 0.037 ms | 88% |
| 7000 | 1,208 | 53.1 | 1,178 | 0.039 ms | 90% |
| 7500 | 1,169 | 52.3 | 1,136 | 0.041 ms | 90% |
| 8000 | 1,257 | 55.6 | 1,220 | 0.044 ms | 89% |
| 8500 | 1,253 | 55.4 | 1,222 | 0.044 ms | 89% |
| 9000 | 1,209 | 54.1 | 1,179 | 0.045 ms | 88% |
| 9500 | 1,332 | 57.5 | 1,305 | 0.038 ms | 87% |
| 10000 | 1,339 | 57.5 | 1,308 | 0.041 ms | 89% |

`/proc/diskstats` counts each flush's empty request as a write; Writes/s here subtracts them (writes minus flushes), leaving the WAL writes and the data-file writeback. The table gives no mean write latency. On a volume shared with the data files, diskstats' write time is dominated by the data-file writeback, whose requests take tens of milliseconds each (see `../ateapi-cold-6x8-traced-20261018/summary.md`), so it does not measure the WAL write.

## How it was run

1. `kubectl apply -f sc.yaml` creates the StorageClass `ate-bench-hyperdisk-extreme`.
2. `target-clusters/dev.sh` exports `ATE_API_POSTGRES_STORAGE_CLASS=ate-bench-hyperdisk-extreme`. `ate-setup` reads that variable directly. Passing `--ateapi-postgres-storage-class` in `ateArgs` does not work: `hack/install-ate.sh` drops the flag without an error, and a first attempt came up on pd-balanced.
3. `systemd-inhibit --what=sleep:idle --who=bench --why="cold resume benchmark" --mode=block ./go.sh` keeps the workstation awake. `go.sh` starts the two samplers and runs the orchestrator (`run.py`, `tests.yaml`):
   - `sample-io.sh` writes every 15 s from inside postgres-0: `pg_stat_io`, `pg_stat_checkpointer`, `pg_stat_wal`, commits, and the data volume's `/proc/diskstats` line.
   - `sample-wal-io.sh` writes WAL counters and wait events every 15 s.

   The scripts carry absolute paths under `~/bench-runs/ateapi-cold-6x8-hde-20261019/`; change them to rerun elsewhere.
4. After Postgres starts, confirm the disk: look up the PV of `ate-system/data-postgres-0` and run `gcloud compute disks describe` on its volume handle (`data/postgres-disk.txt`).
5. Download the results into `results/6x8hde/`, port-forward Prometheus (`kubectl -n benchmarking port-forward svc/prometheus 19090:9090`), and run `python3 benchmarking/automation/analyze_ladder.py results/6x8hde > data/steps.jsonl`.
6. The orchestrator's teardown deletes `ate-system`, and with it the disk. Afterwards, `kubectl delete storageclass ate-bench-hyperdisk-extreme`.
