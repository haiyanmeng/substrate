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
- **Competing writes on the same volume slow every flush.** 70 MiB/s of random writes raise the raw device's flush from 0.49 to 0.86 ms mean (0.94 p50). In the capacity runs Postgres's fsync rose from about 0.52 ms at 4 × 8 to 0.74 ms at 8 × 8 while its data files, on the same volume, absorbed more writes; this fits, though the data-file write rate was not measured in those runs. Under real load it does not hold: the traced run of 2026-10-18 measured that rate, and the WAL fdatasync did not rise with it (`../ateapi-cold-6x8-traced-20261018/summary.md`).
- **Faster disk types do not help.** pd-ssd matches pd-balanced, as the 2026-10-10 capacity run on pd-ssd found (about 1% more throughput). Hyperdisk Balanced, at its default provisioned performance, was slower, with much longer tails; the follow-up below shows that provisioning it higher, or using Hyperdisk Extreme, does not change that.

## What it suggests

1. **Put the WAL on its own volume.** Tested since, and it does not help. This suggestion assumed that data-file writes slowed the WAL's flushes. The traced run of 2026-10-18 (`../ateapi-cold-6x8-traced-20261018/summary.md`) found they do not. Alternating repeats of both layouts on 2026-10-19 (`../ateapi-cold-6x8-layout-repeats-20261019/summary.md`) found no gain on the same node. Runs differed by about 10% depending on the node Postgres landed on, or the disk it got there.
2. **Only node-local storage goes well below 0.5 ms.** The c3-highcpu-88 nodes have no Local SSD; testing it needs a node pool with Local SSD, whose data does not survive the node.
3. **Measure the contention under real load.** Done in the traced run of 2026-10-18 (`../ateapi-cold-6x8-traced-20261018/summary.md`). Data-file writeback fell from about 620 to 90 writes a second across the ladder while the WAL fdatasync stayed flat, so contention on the volume does not explain the slower flush.

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


## Follow-up: what block requests a WAL fdatasync issues, 2026-10-19

The traced 6 × 8 cold resume run (`../ateapi-cold-6x8-traced-20261018/summary.md`) appeared to show two synchronous writes per WAL fdatasync, one completing in 16–64 µs and one in about 0.63 ms. This follow-up records every block request inside sampled fdatasyncs, under real Postgres group commit and fixed write patterns, to find what the two are. Files are in `followup-20261019-wal-write-split/`.

**Result: an fdatasync of WAL issues one data write and one flush. The "fast write" was the flush, misread by the earlier tracer.**

- **Exactly one data write per fdatasync** in every pattern, carrying all the bytes written since the last flush. Across 3,373 sampled fdatasyncs no write was split, and none completed in under 128 µs.
- **A flush appears in the block tracepoints as three events.** The flush request is issued as `FF` at sector 0 and completes as `FF` at sector 2^64−1. Then the fdatasync's original empty request completes as **`WS` at sector 0 with no sectors** (`results/flush-events.txt`). The 6 × 8 tracer keyed requests by sector, so it paired that `WS` completion with the flush's issue at sector 0. That produced one phantom write per fdatasync, lasting as long as the flush and carrying no data. It also halved the apparent size of each write, from 37 to about 19 KiB.
- **On this volume the data write takes 0.37–0.45 ms and the whole fdatasync 0.46–0.52 ms.** The difference, about 0.07 ms, is the flush plus kernel overhead. That is the same split as in the 6 × 8 run (0.63 ms write, 0.70 ms fdatasync), where the write itself was 0.2 ms slower.

### Setup

- One pod on node `…-9v65` with a new 500 GiB pd-balanced volume (`standard-rwo`), ext4, attached as NVMe `259:13`. Queue limits: 256 KiB maximum request, no chunk boundary, write-back cache with FUA, no I/O scheduler (`results/trace.txt`, first lines).
- Container `writer` (`postgres:18-alpine`, `writer.sh`) runs these phases, logged with start times:
  - Postgres 18 with `shared_buffers=2GB`, initialized with `pgbench -i -s 50`;
  - `pgbench -N -c 64 -j 8 -T 60`, group commit as under ate-api-server;
  - `pgbench -N -c 8 -j 4 -T 30`;
  - then 2,000 `dd conv=notrunc,fdatasync` calls in each of three patterns on a preallocated, flushed 16 MiB file: 32 KiB at aligned offsets; Postgres-like (rewrite the last partial 8 KiB page and append 29 KiB); and 8 KiB at one offset, as `pg_test_fsync` does.
- Container `tracer` (privileged, `quay.io/iovisor/bpftrace`, `tracer.sh`) runs `trace.bt` on the volume's device. For every 50th fdatasync from `postgres` or `dd` it prints each step with its time: the ext4 writeback range, bios queued, splits, requests issued and completed, and the fdatasync's end. Every 10 s it prints whole-device aggregates: requests per fdatasync, write sizes split at 128 µs latency, splits and merges.
- `flush-events.yaml` prints every block event on the volume around five 8 KiB `dd` fdatasyncs.
- Run 2026-10-19, 12:02–12:06 UTC; flush-events at about 12:12.

### Reproducing it

From `fsync-microbenchmark-20261016/followup-20261019-wal-write-split/`; edit the node name in `pod.yaml` and `flush-events.yaml` first:

```bash
kubectl apply -f pvc.yaml                           # namespace fsync-bench and one 500 GiB PVC
kubectl -n fsync-bench create configmap split --from-file=writer.sh --from-file=tracer.sh --from-file=trace.bt
kubectl apply -f pod.yaml
# About 4 minutes; the tracer's log ends with "### END".
kubectl -n fsync-bench logs split -c tracer > results/trace.txt
kubectl -n fsync-bench logs split -c writer > results/writer.txt
python3 analyze.py results/trace.txt > results/phases.txt

kubectl -n fsync-bench delete pod split
kubectl apply -f flush-events.yaml                  # reuses the PVC; about 10 s of tracing
kubectl -n fsync-bench logs flush-events > results/flush-events.txt

kubectl delete namespace fsync-bench                # deletes the PVC and its disk
```

### Results

Sampled fdatasyncs (`results/phases.txt`). The dd phases are shorter than the 10 s aggregation interval, so `analyze.py` tells them apart by write size.

| Phase | Sampled fdatasyncs | Data writes per fdatasync | KiB per write | Write latency mean / p50 / p90 | fdatasync mean / p50 | Splits |
|---|---|---|---|---|---|---|
| pgbench, 64 clients (40,853 tps) | 2,195 | 1.00 | 14.6 | 446 / 436 / 509 µs | 520 / 510 µs | 0 |
| pgbench, 8 clients (8,559 tps) | 1,087 | 1.00 | 9.8 | 398 / 387 / 440 µs | 462 / 450 µs | 0 |
| dd, 32 KiB aligned | 31 | 1.00 | 32.0 | 426 / 424 / 466 µs | 513 / 514 µs | 0 |
| dd, Postgres-like | 20 | 1.00 | 36.0 | 393 / 392 / 414 µs | 480 / 482 µs | 0 |
| dd, 8 KiB at one offset | 40 | 1.00 | 8.0 | 373 / 357 / 454 µs | 458 / 445 µs | 0 |

A sampled fdatasync under pgbench, times in µs:

```
S   0        fdatasync starts
WP  +4       ext4 writepages over the whole file
Q   +9       WS bio, 32 sectors
I   +14      WS request issued
WPR +18      4 pages written
C   +420     WS completes (lat 407)
Q   +429     FWS empty bio: the flush
E   +460     fdatasync returns (dur 460)
```

### What it shows

- **The WAL write is a single request, and its latency is the fdatasync's cost.** The flush and kernel overhead add about 0.07 ms on top of it.
- **Write size matters little in this range.** 8 to 36 KiB writes took 0.37–0.45 ms. A Postgres-like rewrite of the last partial page is no slower than an aligned append.
- **The 6 × 8 run's write was slower: 0.63 ms against 0.45 ms here, at a lower flush rate.** This test does not say why. Differences include a different volume, the 6 × 8 run's larger writes (37 KiB), and its concurrent data-file writeback. The first follow-up found volume-to-volume differences of up to 1.8× on Hyperdisk Extreme, though not on pd-balanced.

### Limits

- One volume, one node, about four minutes of load.
- The sample is every 50th fdatasync. The whole-device aggregates in `results/trace.txt`, which cover every request, agree: one request per fdatasync and no write under 128 µs.
- `trace.bt` maps a flush's completion at sector 2^64−1 back to sector 0. If the empty `WS` completion arrived before the `FF` one, it would be miscounted as a fast write. It never did here: the whole-device aggregates show no write under 128 µs.
- The `pw_off` and `pw_len` fields on `S` lines are always 0. `trace.bt` hooks `pwrite64`, and Postgres 18's WAL writes did not show up there, probably because they go through another write call such as `pwritev`. Nothing above depends on these fields.
