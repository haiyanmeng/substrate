# WAL flush latency on GKE volumes: an fsync microbenchmark, 2026-10-16

In the cold resume capacity runs (`../ateapi-cold-resume-summary.md`), Postgres's WAL flush sets the size10 ceiling: the fsync runs back to back, busy 92–95% of the time, at 0.51–0.76 ms per fsync. This microbenchmark measures a single write-and-flush stream outside Postgres, layer by layer, to see where that time goes: Postgres, the filesystem, or the disk and the network to it.

**Result: the floor is Persistent Disk's per-operation round trip, about 0.4–0.5 ms, and competing writes on the same volume raise it.** Postgres's own code costs about 1 µs; the filesystem adds nothing for writes that overwrite allocated blocks, as Postgres's preallocated WAL segments do; pd-ssd is no faster than pd-balanced, and Hyperdisk Balanced is slower.

## Setup

- Cluster `nh-prow-parity` (project `haiyanmeng-gke-dev`), one pod pinned to `gke-nh-prow-parity-default-pool-ca559d59-9v65`, a c3-highcpu-88 node of the pool that runs Postgres in the capacity runs; Container-Optimized OS, kernel 6.12.94+. Nothing else of the benchmark ran on the cluster.
- Run on 2026-10-16 from about 23:52 to 00:09 UTC on 2026-10-17, every test in sequence in one pod, so no two tests overlap.
- Image `postgres:18-alpine`, with `pg_test_fsync` from PostgreSQL 18 and `fio` 3.41 installed at start.
- Targets, each a fresh volume of 500 GiB, the size of the capacity runs' Postgres volume (Persistent Disk performance scales with size), attached to the node as an NVMe device:

| Target | StorageClass | Filesystem |
|---|---|---|
| pd-balanced, ext4 | `standard-rwo` (`type: pd-balanced`), the cluster default Postgres uses | ext4, `rw,relatime` |
| pd-balanced, raw block | `standard-rwo`, `volumeMode: Block` | none |
| pd-ssd, ext4 | `premium-rwo` (`type: pd-ssd`) | ext4, `rw,relatime` |
| Hyperdisk Balanced, ext4 | `fsync-bench-hyperdisk-balanced` (`type: hyperdisk-balanced`, default provisioned IOPS and throughput), created for the run and deleted after | ext4, `rw,relatime` |
| tmpfs | `emptyDir` with `medium: Memory` | tmpfs |

- Tests on each target (`bench.sh`):
  - `pg_test_fsync -s 10`: PostgreSQL's own tool. It rewrites the same blocks of a preallocated file, as Postgres does with its WAL segments, and times each `wal_sync_method`. Not run on the raw block device, which it cannot use.
  - `fio`, alone: 32 KiB sequential writes, one at a time (`ioengine=sync`, `iodepth=1`), each followed by `fdatasync`, for 30 s. 32 KiB is about what a WAL flush writes at the size10 ceilings (27–60 KiB). On ext4 it writes a new file, which grows as it goes.
  - `fio`, contended: the same job alongside a second job doing 8 KiB random writes (`direct=1`, `iodepth=8`, limited to 80 MiB/s) to the same volume, standing in for Postgres's data-file writes. On the raw device the two jobs write different regions.
- Files: `bench.sh` (the script), `sc-pvc.yaml` (the StorageClass and volumes), `pod.yaml` (the pod); `results/` holds each test's output: `<target>-pg_test_fsync.txt`, `<target>-alone.json` and `<target>-contended.json` from `fio --output-format=json`, and `mounts.txt`.

To rerun, create the namespace, apply the manifests and the script, and read the results from the pod's log:

```bash
kubectl create namespace fsync-bench
kubectl apply -f sc-pvc.yaml
kubectl -n fsync-bench create configmap bench --from-file=bench.sh
kubectl apply -f pod.yaml
kubectl -n fsync-bench logs -f bench        # each result file follows "### FILE <name>"
kubectl delete namespace fsync-bench && kubectl delete storageclass fsync-bench-hyperdisk-balanced
```

## Results

**`fio`: 32 KiB write + `fdatasync`, one at a time**

| Target | Alone: `fdatasync` mean / p50 / p99 | Alone: flushes/s | With 70–80 MiB/s of random writes: mean / p50 / p99 | Contended: flushes/s |
|---|---|---|---|---|
| tmpfs | ~0 µs | 164,834 | ~0 µs | 156,376 |
| pd-balanced, raw block | **0.49 / 0.49 / 0.62 ms** | 1,978 | **0.86 / 0.94 / 1.35 ms** | 1,143 |
| pd-balanced, ext4 | 1.30 / 1.30 / 1.57 ms | 762 | 1.08 / 0.93 / 3.20 ms | 910 |
| pd-ssd, ext4 | 1.26 / 1.25 / 1.52 ms | 786 | 0.91 / 0.54 / 2.15 ms | 1,084 |
| Hyperdisk Balanced, ext4 | 2.04 / 1.83 / 4.49 ms | 487 | 1.75 / 1.57 / 4.56 ms | 566 |

The `write()` call itself took 5–13 µs everywhere; the time is in `fdatasync`. The background job reached its 80 MiB/s on tmpfs and pd-ssd, about 69–70 MiB/s on pd-balanced, and 44 MiB/s on Hyperdisk Balanced.

**`pg_test_fsync`: per-operation time, rewriting a preallocated file**

| Target | One 8 kB write: `open_datasync` / `fdatasync` / `fsync` | Two 8 kB writes: `open_datasync` / `fdatasync` / `fsync` |
|---|---|---|
| tmpfs | 0 / 1 / 1 µs | 1 / 1 / 1 µs |
| pd-balanced, ext4 | 369 / **402** / 1,167 µs | 742 / **432** / 1,181 µs |
| pd-ssd, ext4 | 356 / 386 / 1,039 µs | 695 / 406 / 1,156 µs |
| Hyperdisk Balanced, ext4 | 494 / 533 / 1,643 µs | 946 / 533 / 1,733 µs |

`fdatasync` is Postgres's default `wal_sync_method` on Linux.

## What it shows

- **Postgres's code is not the floor.** The same write and flush take about 1 µs on tmpfs. What Postgres adds is its design: one WAL flush at a time, each covering every commit already waiting, so throughput is flush latency times the commits per flush.
- **The disk and the network to it set the floor.** On the raw device, with no filesystem, a 32 KiB write plus `fdatasync` takes 0.49 ms, and `pg_test_fsync` takes 0.40–0.43 ms on ext4. Postgres measured 0.51–0.55 ms per WAL fsync at light load in the capacity runs (size0 2 × 8 and size10 4 × 8, 17–40 KiB per fsync), the same round trip. Persistent Disk is network storage, so this is mostly the round trip to the storage service and its replicated write; from inside the VM the network cannot be told apart from the backend.
- **The filesystem only costs when a file grows.** `fio` on ext4 takes 1.3 ms against 0.49 ms on the raw device because it appends: each `fdatasync` must also commit ext4's journal for the new size and blocks. `pg_test_fsync`, which rewrites allocated blocks, matches the raw device, and so does Postgres, which preallocates and recycles WAL segments. `fsync`, which also commits file metadata, takes about 1.1 ms.
- **Competing writes on the same volume slow every flush.** 70 MiB/s of random writes raise the raw device's flush from 0.49 to 0.86 ms mean (0.94 p50). In the capacity runs Postgres's fsync rose from about 0.52 ms at 4 × 8 to 0.74 ms at 8 × 8 while its data files, on the same volume, absorbed more writes; this fits, though the data-file write rate was not measured in those runs.
- **Faster disk types do not help.** pd-ssd matches pd-balanced, as the 2026-10-10 capacity run on pd-ssd found (about 1% more throughput). Hyperdisk Balanced, at its default provisioned performance, was slower, with much longer tails.

## What it suggests

1. **Put the WAL on its own volume.** A separate PVC for `pg_wal` keeps its flushes clear of data-file writes, at about 0.5 ms rather than 0.7 ms at 6–8 × 8. If each flush kept carrying as many commits, that would lift the size10 plateau by up to about a third; that is an inference.
2. **Only node-local storage goes well below 0.5 ms.** The c3-highcpu-88 nodes have no Local SSD; testing it needs a node pool with Local SSD, whose data does not survive the node.
3. **Measure the contention under real load.** A capacity run sampling `pg_stat_io` with `track_io_timing` and the Postgres volume's `/proc/diskstats` (write and flush latency) would show whether the disk's flush latency rises with Postgres's data-file writes. A first attempt on 2026-10-17 lost its samples to a workstation suspend.

## Limits

- One run per target, 30 s per `fio` test and 10 s per `pg_test_fsync` method; Persistent Disk latency varies between runs and volumes, as the capacity runs' fsync times did by up to about 30%.
- One node and machine type; Hyperdisk Balanced at its default provisioned IOPS and throughput, which a higher provisioning may change.
- The contended test's background writes are synthetic, 8 KiB random at a fixed rate; Postgres's checkpoint and background writes are burstier.
- No kernel-level tracing: counting the block requests each `fdatasync` issues would need a privileged pod on the node.
