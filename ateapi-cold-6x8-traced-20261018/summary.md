# Where the WAL fdatasync spends its time: a traced 6 × 8 CPU cold resume run on size10 Postgres, 2026-10-18

- Test `ateapi_cold_resume_pause_fake_6x8cpu_size10` (`tests.yaml`, with `track_io_timing=on` added to `track_wal_io_timing=on`), run with the local orchestrator on branch `ateapi-cold-resume-capacity` at `525ba7d1`, 18:01–18:47 UTC, ladder 18:10–18:43. Complete; none of about 24.8 million requests failed (12,399,930 cold resumes, 12,393,379 pauses).
- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`). Postgres on `gke-nh-prow-parity-default-pool-ca559d59-4d75`, its data volume a new 500Gi pd-balanced disk attached as NVMe device `259:13` (`data/trace-target.txt`).
- Results: `gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev/runs/ateapi_cold_resume_pause_fake_6x8cpu_size10/run_date=2026-10-18/run_ts=1792346984/`; local copy in `results/6x8/`. Everything this run added is in `data/`, described under [Files](#files).

**Result: the WAL fdatasync's time is almost all one block write. The flush, the ext4 journal, Postgres's write path and the data-file writes on the same volume add little or nothing.**

- **The ceiling is about 6,690 cycles/s** (6,631–6,748 over steps 5–11), set as before by Postgres's WAL flush: about 1,300 fdatasyncs/s of 0.70 ms, busy about 91% of each second, carrying about 123 commits each, so about 160,000 commits/s.
- **Each fdatasync is one block write of the WAL, about 37 KiB taking about 0.63 ms, then one cache flush.** At every step from 6500/s on, the trace counts one write completing in 256 µs–2 ms for each fdatasync, and `/proc/diskstats` one flush. 98% of fdatasyncs take 0.5–1 ms. The trace also shows a "fast write" of 16–64 µs per fdatasync, but that is the flush, miscounted (see [How it was run](#how-it-was-run)). A follow-up confirmed one data write per fdatasync (`../fsync-microbenchmark-20261016/summary.md`, follow-up of 2026-10-19).
- **The flush costs 0.042 ms.** The volume reports a write-back cache, but flushing it is cheap; the durable round trip is in the write.
- **ext4's journal is rarely involved:** 0.2–10 jbd2 commits a second against 1,300 fdatasyncs, because Postgres preallocates WAL segments and so overwrites blocks in place.
- **Postgres's own write is 0.03 ms** (`pg_stat_io` WAL `write_time` per write): the pwrite into the page cache.
- **Data-file writes do not slow the flush in this run.** Data-file writeback fell from 617 to about 90 writes/s across the ladder while the fdatasync stayed at 0.67–0.72 ms; the step with the most data-file writes, 5500/s at 812/s, had the fastest fdatasync, 0.67 ms. Each data-file block write is large and slow (160–230 KiB, 23–48 ms), but it does not show in the WAL write's latency. This rules out contention from the same volume as the cause of the fsync rising from 0.52 ms at 4 × 8 to 0.70–0.76 ms at 7 and 8 × 8.

**Not established:**

- **Why the WAL write takes about 0.63 ms.** On another new pd-balanced volume, the 2026-10-19 follow-up measured 0.37–0.45 ms for 8–36 KiB writes, including Postgres under 64-client pgbench at about 1,800 fdatasyncs/s. Differences from this run include the volume, the larger writes here, and the concurrent data-file writeback.
- **What makes the fsync slower with more replicas.** Each install creates a new Postgres volume, so volume-to-volume variation is a live candidate: the 5 × 8 run of 2026-10-16 (`../ateapi-cold-5x8-size10-neighbors-20261016/summary.md`), with Postgres on another node and another new volume, had 0.53–0.57 ms fsyncs, against 0.70 ms here at similar commits per fsync. The microbenchmark also measured two Hyperdisk Extreme volumes at 0.39 and 0.69 ms.


## Per step

Postgres's view (`data/wal-io.csv`, `data/steps.jsonl`):

| Target | Resume/s | Server mean (ms) | Server p50 / p99 (ms) | ateapi cores | Postgres cores | Active PG conns | fsyncs/s | Mean fsync | Commits per fsync |
|---|---|---|---|---|---|---|---|---|---|
| 5000 | 4,205 | 1,058 | 265 / 6,061 | 22.4 | 49.2 | 163 | 1,160 | 0.79 ms | 87 |
| 5500 | 5,486 | 191 | 159 / 745 | 28.5 | 33.9 | 109 | 1,365 | 0.67 ms | 97 |
| 6000 | 6,000 | 251 | 170 / 960 | 30.7 | 38.3 | 172 | 1,280 | 0.71 ms | 112 |
| 6500 | 6,513 | 583 | 439 / 1,630 | 33.8 | 42.6 | 161 | 1,302 | 0.71 ms | 120 |
| 7000 | 6,686 | 1,114 | 1,024 / 3,224 | 34.6 | 44.1 | 168 | 1,325 | 0.70 ms | 121 |
| 7500 | 6,650 | 1,125 | 826 / 5,765 | 35.2 | 40.8 | 151 | 1,284 | 0.72 ms | 125 |
| 8000 | 6,748 | 1,110 | 324 / 5,895 | 35.2 | 44.0 | 154 | 1,310 | 0.71 ms | 124 |
| 8500 | 6,631 | 1,129 | 308 / 4,645 | 34.8 | 43.8 | 152 | 1,275 | 0.73 ms | 125 |
| 9000 | 6,709 | 1,118 | 301 / 4,718 | 34.6 | 44.4 | 156 | 1,307 | 0.71 ms | 123 |
| 9500 | 6,667 | 1,125 | 324 / 4,759 | 34.5 | 40.1 | 172 | 1,279 | 0.72 ms | 125 |
| 10000 | 6,687 | 1,120 | 313 / 4,873 | 35.0 | 44.2 | 178 | 1,322 | 0.70 ms | 122 |

The 5000/s step fell short, probably because the warm-up of the 14,000 actors overlapped its start; its latency and CPU are not comparable with the others.

The kernel's view (`data/trace-table.txt`, from `analyze_trace.py`). fdatasync is the syscall from processes named `postgres`. WS fast and slow split the synchronous block completions at 256 µs; the slow mean covers 256 µs–2 ms. WS slow is the WAL write. WS fast is not a write: it is each flush's empty request, paired with the flush's issue, so it duplicates the flush count. The flush and the busy share come from `/proc/diskstats`; data-file writes from `pg_stat_io`.

| Target | fdatasyncs/s | Mean fdatasync | WS fast/s | WS slow/s | Slow WS mean | Flushes/s | Mean flush | Async writes/s (W) | W mean | jbd2 commits/s | Disk MiB/s | Disk busy | Data-file writes/s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 5000 | 1,148 | 0.79 ms | 1,150 | 1,229 | 0.67 ms | 1,171 | 0.042 ms | 83 | 44 ms | 3.5 | 82.0 | 79% | 617 |
| 5500 | 1,370 | 0.66 ms | 1,372 | 1,471 | 0.63 ms | 1,368 | 0.039 ms | 54 | 23 ms | 3.8 | 83.4 | 84% | 812 |
| 6000 | 1,275 | 0.70 ms | 1,276 | 1,386 | 0.69 ms | 1,283 | 0.041 ms | 41 | 40 ms | 4.1 | 83.8 | 83% | 348 |
| 6500 | 1,302 | 0.70 ms | 1,307 | 1,330 | 0.64 ms | 1,321 | 0.042 ms | 37 | 38 ms | 10.0 | 61.4 | 83% | 118 |
| 7000 | 1,321 | 0.70 ms | 1,325 | 1,321 | 0.61 ms | 1,340 | 0.042 ms | 37 | 48 ms | 9.1 | 55.0 | 83% | 69 |
| 7500 | 1,283 | 0.72 ms | 1,283 | 1,283 | 0.65 ms | 1,285 | 0.042 ms | 33 | 44 ms | 0.2 | 53.6 | 84% | 101 |
| 8000 | 1,309 | 0.70 ms | 1,309 | 1,309 | 0.63 ms | 1,303 | 0.041 ms | 37 | 38 ms | 0.2 | 55.5 | 83% | 103 |
| 8500 | 1,276 | 0.72 ms | 1,281 | 1,277 | 0.66 ms | 1,300 | 0.042 ms | 37 | 40 ms | 9.1 | 55.0 | 84% | 86 |
| 9000 | 1,312 | 0.70 ms | 1,317 | 1,312 | 0.62 ms | 1,323 | 0.042 ms | 25 | 38 ms | 9.1 | 54.3 | 83% | 84 |
| 9500 | 1,286 | 0.71 ms | 1,290 | 1,286 | 0.64 ms | 1,295 | 0.042 ms | 34 | 43 ms | 9.1 | 55.0 | 84% | 85 |
| 10000 | 1,327 | 0.69 ms | 1,326 | 1,326 | 0.61 ms | 1,323 | 0.041 ms | 42 | 35 ms | 0.2 | 55.4 | 83% | 96 |

The slow-write mean is from the histogram's bucket midpoints, so it is approximate. At the 5000–6000/s steps Postgres was still creating new 16 MiB WAL segments, 1.7–2.1 a second, which accounts for the extra MiB/s; from 7000/s it recycled them.

## How it was run

1. `systemd-inhibit --what=sleep:idle --who=bench --why="cold resume traced benchmark" --mode=block ./go.sh`. The inhibitor keeps the workstation from suspending, which killed the samplers in the 2026-10-17 attempt. The scripts carry absolute paths under `~/bench-runs/ateapi-cold-6x8-traced-20261018/`; change them to rerun elsewhere.
2. `go.sh` starts three helpers in the background, then runs the orchestrator (`run.py`, with `tests.yaml` and `target-clusters/dev.sh`):
   - `sample-io.sh` writes every 15 s, from inside postgres-0: `pg_stat_io` rows with any writes, `pg_stat_checkpointer`, `pg_stat_wal`, commits, and the `/proc/diskstats` line of the data volume's device (`data/io.jsonl`).
   - `sample-wal-io.sh` writes WAL write and fsync counters and wait-event counts every 15 s (`data/wal-io.csv`).
   - `launch-trace.sh` waits for the runner pod to be Running, reads postgres-0's node and its data volume's `major:minor` from `/proc/self/mountinfo`, and starts `pgio.bt` there in a privileged, hostPID pod from `trace-pod.yaml.tmpl` in namespace `kernel-trace` for 45 minutes. The pod's log is saved when it completes (`data/trace.txt`). The namespace must exist before the run (`kubectl create namespace kernel-trace`); delete it afterwards.
3. Download the results from GCS into `results/6x8/`, port-forward Prometheus (`kubectl -n benchmarking port-forward svc/prometheus 19090:9090`), and run `python3 benchmarking/automation/analyze_ladder.py results/6x8 > data/steps.jsonl`.
4. `python3 analyze_trace.py data > data/trace-steps.jsonl` aggregates the trace, diskstats and `pg_stat_io` over each step's window; `data/trace-table.txt` formats it.

`pgio.bt` prints, every 30 s and then clears:

- fdatasync and fsync calls from processes named `postgres`: count, total time, and an fdatasync latency histogram;
- block requests on the data device, by request type: count, total issue-to-complete time, sectors and a latency histogram;
- jbd2 commits on the data device: count and total duration.

It keys each block request's issue time by sector. Two things about that on these NVMe volumes:

- **Flushes are miscounted as fast synchronous writes.** `rwbs-test.yaml` runs `dd oflag=dsync` on a node and prints raw block events. It shows a flush issuing as `FF` at sector 0 and completing as `FF` at sector 2^64−1, which the pairing never matches. The 2026-10-19 follow-up (`../fsync-microbenchmark-20261016/followup-20261019-wal-write-split/results/flush-events.txt`) shows a third event: the fdatasync's original empty request completes as `WS` at sector 0 with no sectors. `pgio.bt` pairs that completion with the flush's issue, so each flush appears as a `WS` of 16–64 µs with no data. That doubles the `WS` count and halves its mean size. Flush counts and time come from `/proc/diskstats` (fields 16 and 17 after the device name) instead.
- **Summed block times are unusable for some types.** A few mismatched pairs produce huge values, so `analyze_trace.py` takes latencies from the histograms and drops buckets of 1 s and above.

The `rwbs` field is a `char[8]` with stale bytes after the terminating NUL, so `pgio.bt` maps it to a numeric code (legend at the top of `data/trace.txt`): 2 = WFSM (journal commit block), 3 = WSM (journal metadata), 4 = WS (synchronous write), 5 = WM, 6 = W (asynchronous writeback), 12 = RA.

## Files

| File | What |
|---|---|
| `go.sh`, `run.py`, `tests.yaml`, `target-clusters/dev.sh` | The run |
| `sample-io.sh`, `sample-wal-io.sh` | Postgres and diskstats samplers |
| `launch-trace.sh`, `pgio.bt`, `trace-pod.yaml.tmpl` | Kernel tracing on Postgres's node |
| `rwbs-test.yaml` | Check of how flush requests appear in block tracepoints |
| `analyze_trace.py` | Per-step aggregation of the trace, diskstats and `pg_stat_io` |
| `data/trace.txt` | bpftrace output, 30 s intervals 18:10–18:55 |
| `data/io.jsonl` | `pg_stat_io`, checkpointer, `pg_stat_wal` and diskstats every 15 s; the last line is a failed sample from Postgres shutting down |
| `data/wal-io.csv` | WAL counters and wait events every 15 s |
| `data/steps.jsonl` | Per-step load, latency, CPU and Postgres metrics from `analyze_ladder.py` |
| `data/trace-steps.jsonl`, `data/trace-table.txt` | Output of `analyze_trace.py` |
| `data/trace-target.txt`, `data/run-times.txt` | Traced node and device; run start, end and exit code |
