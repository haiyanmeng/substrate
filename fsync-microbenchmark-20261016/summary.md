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
- **Faster disk types do not help.** pd-ssd matches pd-balanced, as the 2026-10-10 capacity run on pd-ssd found (about 1% more throughput). Hyperdisk Balanced, at its default provisioned performance, was slower, with much longer tails; the follow-up below shows that provisioning it higher, or using Hyperdisk Extreme, does not change that.

## What it suggests

1. **Put the WAL on its own volume.** A separate PVC for `pg_wal` keeps its flushes clear of data-file writes, at about 0.5 ms rather than 0.7 ms at 6–8 × 8. If each flush kept carrying as many commits, that would lift the size10 plateau by up to about a third; that is an inference.
2. **Only node-local storage goes well below 0.5 ms.** The c3-highcpu-88 nodes have no Local SSD; testing it needs a node pool with Local SSD, whose data does not survive the node.
3. **Measure the contention under real load.** A capacity run sampling `pg_stat_io` with `track_io_timing` and the Postgres volume's `/proc/diskstats` (write and flush latency) would show whether the disk's flush latency rises with Postgres's data-file writes. A first attempt on 2026-10-17 lost its samples to a workstation suspend.

## Limits

- One run per target, 30 s per `fio` test and 10 s per `pg_test_fsync` method; Persistent Disk latency varies between runs and volumes, as the capacity runs' fsync times did by up to about 30%.
- One node and machine type; Hyperdisk Balanced at its default provisioned IOPS and throughput, which a higher provisioning may change.
- The contended test's background writes are synthetic, 8 KiB random at a fixed rate; Postgres's checkpoint and background writes are burstier.
- No kernel-level tracing: counting the block requests each `fdatasync` issues would need a privileged pod on the node.

## Follow-up: provisioned Hyperdisk, two volumes per type, 2026-10-17

The first run tested Hyperdisk Balanced only at its default provisioning, on one volume. The follow-up asks whether high provisioning, or Hyperdisk Extreme, beats pd-balanced, and how much two volumes of the same type differ. Files are in `followup-20261017/`.

**Result: no Hyperdisk configuration is faster than pd-balanced for a one-at-a-time write and flush.** Provisioned Hyperdisk Balanced is 60–80% slower; Hyperdisk Extreme matched pd-balanced on one volume and was 70% slower on the other. pd-balanced was consistent across its two volumes.

### Setup

Same node (`…-9v65`), image, tools and tests as the first run; ext4 on every target. `followup-20261017/bench.sh` takes the targets as `name:mountpath` arguments, timestamps each test (`results/timeline.txt`), and prints every result file to its log. Run on 2026-10-17 from 15:02 to about 15:25 UTC.

| Target | Volumes | StorageClass parameters (`followup-20261017/sc-pvc.yaml`) | As created (from `gcloud compute disks describe`) |
|---|---|---|---|
| pd-balanced | `pdb-a`, `pdb-b` | `standard-rwo` | pd-balanced, 500 GB |
| Hyperdisk Balanced, provisioned | `hdbp-a`, `hdbp-b` | `type: hyperdisk-balanced`, `provisioned-iops-on-create: "20000"`, `provisioned-throughput-on-create: "600Mi"` | hyperdisk-balanced, 500 GB, 20,000 IOPS, 600 MiB/s |
| Hyperdisk Extreme | `hde-a`, `hde-b` | `type: hyperdisk-extreme`, `provisioned-iops-on-create: "50000"` | hyperdisk-extreme, 500 GB, 50,000 IOPS |

### Reproducing it

From `fsync-microbenchmark-20261016/followup-20261017/`, against a cluster with a node that supports Hyperdisk Extreme (c3-highcpu-88 does); edit the node name in `pod.yaml` first:

```bash
kubectl create namespace fsync-bench
kubectl apply -f sc-pvc.yaml                       # two StorageClasses, six 500 GiB PVCs
kubectl -n fsync-bench create configmap bench --from-file=bench.sh
kubectl apply -f pod.yaml                          # PVCs bind when the pod schedules

# Optional: confirm the disks' type and provisioning.
for p in pdb-a hdbp-a hde-a; do
  h=$(kubectl get pv "$(kubectl -n fsync-bench get pvc $p -o jsonpath='{.spec.volumeName}')" -o jsonpath='{.spec.csi.volumeHandle}')
  gcloud compute disks describe "$(echo $h | cut -d/ -f6)" --zone "$(echo $h | cut -d/ -f4)" \
    --format='value(type.basename(),sizeGb,provisionedIops,provisionedThroughput)'
done

# About 25 minutes; the log ends with "### END". Save it, then split it on "### FILE <name>".
kubectl -n fsync-bench logs bench > followup-bench.log

kubectl delete namespace fsync-bench                # deletes the PVCs and their disks
kubectl delete storageclass fsync-bench-hdb-provisioned fsync-bench-hde
```

The pod's log keeps the results after the pod exits, as long as the pod is not deleted. The pod uses a `nodeSelector` rather than `nodeName`: these StorageClasses bind volumes only once the scheduler places the pod.

### Results

**`pg_test_fsync`, rewriting a preallocated file (the pattern Postgres's WAL uses)**

| Volume | One 8 kB write: `open_datasync` / `fdatasync` / `fsync` | Two 8 kB writes: `open_datasync` / `fdatasync` |
|---|---|---|
| pd-balanced a | 373 / **402** / 1,156 µs | 746 / 427 µs |
| pd-balanced b | 380 / **400** / 1,187 µs | 732 / 428 µs |
| Hyperdisk Balanced, provisioned a | 606 / **646** / 2,242 µs | 1,253 / 733 µs |
| Hyperdisk Balanced, provisioned b | 671 / **725** / 1,865 µs | 1,304 / 782 µs |
| Hyperdisk Extreme a | 602 / **689** / 1,925 µs | 1,170 / 711 µs |
| Hyperdisk Extreme b | 323 / **386** / 961 µs | 648 / 429 µs |

**`fio`, 32 KiB write + `fdatasync`, appending on ext4**

| Volume | Alone: mean / p50 / p99 | Alone: flushes/s | Contended: mean / p50 / p99 | Contended: flushes/s | Background reached |
|---|---|---|---|---|---|
| pd-balanced a | 1.29 / 1.27 / 1.57 ms | 767 | 1.08 / 0.93 / 3.42 ms | 910 | 69 MiB/s |
| pd-balanced b | 1.32 / 1.30 / 1.58 ms | 751 | 1.10 / 0.93 / 3.42 ms | 899 | 69 MiB/s |
| Hyperdisk Balanced, provisioned a | 2.15 / 2.02 / 4.82 ms | 462 | 1.56 / 0.92 / 5.87 ms | 636 | 60 MiB/s |
| Hyperdisk Balanced, provisioned b | 1.93 / 1.86 / 3.23 ms | 513 | 1.33 / 0.82 / 3.46 ms | 744 | 70 MiB/s |
| Hyperdisk Extreme a | 2.01 / 1.94 / 3.42 ms | 494 | 1.32 / 0.86 / 3.62 ms | 749 | 73 MiB/s |
| Hyperdisk Extreme b | 1.20 / 1.12 / 2.41 ms | 821 | 0.86 / 0.54 / 2.61 ms | 1,149 | 80 MiB/s |

### What it shows

- **Provisioning does not shorten the round trip.** At 20,000 IOPS and 600 MiB/s, Hyperdisk Balanced's rewrite `fdatasync` took 646–725 µs, slower than its default-provisioned 533 µs in the first run and well behind pd-balanced's 400 µs. One-at-a-time flushes use a few hundred to two thousand operations per second, far below any of these limits, so provisioned IOPS and throughput are never in play; what counts is the latency of each round trip, which provisioning does not buy.
- **Hyperdisk Extreme varies by volume.** One volume took 386 µs, about pd-balanced's 400 µs; the other 689 µs. The pattern held in every test on each, so it is the volume, not noise between tests. Its best case only matches pd-balanced.
- **pd-balanced is consistent.** Its two volumes, and the first run's, agree within about 2% in every test: 400–402 µs in `pg_test_fsync` and 1.29–1.32 ms in `fio`.
- **The first run's conclusion holds.** For a WAL flushed one write at a time, none of pd-ssd, Hyperdisk Balanced at any provisioning, or Hyperdisk Extreme is faster than pd-balanced; the levers are keeping other writes off the WAL's volume, or node-local storage.
- On ext4, every target's contended `fio` mean and p50 are lower than when it runs alone, while its p99 is higher. The appending `fio` job's journal commits may batch differently when the second job writes alongside; this run does not show why, and `pg_test_fsync`, which does not append, is the better guide to Postgres.

### Limits

- Two volumes per type, one run each; Hyperdisk Extreme's two volumes disagree by 1.8×, so more volumes would be needed to say what a typical one does.
- One node, one machine type, one zone (us-central1-a), at one time of day.

