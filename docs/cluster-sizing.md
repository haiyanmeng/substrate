# Sizing an Agent Substrate cluster

This guide estimates how big each part of a Substrate cluster has to be for
your workload, and which limit you'll reach first. The numbers come from
benchmarks on GKE. Use them for a first estimate, then measure with your own
actors before you commit to a size.

The steps:

1. [Describe your workload](#1-describe-your-workload).
2. [Size the data plane](#2-size-the-data-plane).
3. [Size the control plane](#3-size-the-control-plane).
4. [Follow the best practices](#4-best-practices).

## 1. Describe your workload

| Input | Meaning |
| :---- | :---- |
| Actors | Every actor that exists, running or suspended |
| Peak running actors | The most actors running at the same time |
| Peak resume rate | The most resumes per second, with as many suspends |
| Actor memory | The ActorTemplate's memory limit. A suspended actor's snapshot holds the memory it uses plus files it wrote, so this limit is a safe estimate of snapshot size. |
| Actor CPU | The ActorTemplate's CPU limit |
| Ingress requests/s | Requests routed to actors through `atenet-router` |
| Sandbox class | gVisor or microVM. Each needs its own WorkerPool. |

Size for the peaks, not the averages: workers have to hold the most actors that
run at once, and the snapshot path has to keep up with the fastest resume rate.

If you don't know the peaks yet, estimate them:

```
peak running actors ≈ actors × fraction running on average × burst factor
peak resume rate    ≈ peak running actors ÷ seconds an actor stays running per resume
```

For example, take 100K actors with 10% running on average, so 10K, and bursts
of twice that, so a peak of 20K running actors. If each actor stays running for
100 s per resume, holding the peak takes 20K ÷ 100 s = 200 resumes/s.

Substrate doesn't suspend idle actors by itself: your application or
orchestrator is responsible for it.

## 2. Size the data plane

### Workers and nodes

A WorkerPool runs worker pods in its own namespace, and each worker hosts many
actors. A worker has room for an actor (a *slot*) while it hosts fewer than its
actor limit (1000) and the actor's memory and CPU fit in what's left of the
worker pod's limits.

```
slots per worker = the smallest of: 1000,
                   worker memory ÷ actor memory,
                   worker CPU ÷ actor CPU
workers          = peak running actors ÷ (slots per worker × 0.8)
```

The 0.8 leaves room for the scheduler to find a free slot.

For example, take a worker with 3 vCPU and 12 GiB of memory, and actors that
each need 0.25 vCPU and 0.75 GiB:

| | Value |
| :---- | :---- |
| Slots by CPU | 3 ÷ 0.25 = 12 |
| Slots by memory | 12 ÷ 0.75 = 16 |
| Slots per worker | 12, set by CPU |

With one worker per node, the number of workers is the number of nodes. Don't
give the worker all of the node's allocatable CPU and memory: leave room for
atelet (a DaemonSet in `ate-system`) and other system pods.

GKE supports up to 65,000 nodes per cluster and 110 pods per node by default.
For very large fleets, use larger nodes.

### Snapshot traffic

Each resume downloads the actor's snapshot from object storage, and each
suspend uploads one:

| Quantity | Formula |
| :---- | :---- |
| Snapshot bandwidth, each way | peak resume rate × actor memory |
| Node disk writes, average per node | 2 × peak resume rate × actor memory ÷ nodes. Unlike the network, both directions are disk writes: the downloaded snapshot on resume, and the checkpoint on suspend. |
| Object storage requests | per resume and its matching suspend: one download on resume; one upload of the new snapshot and one delete of the previous snapshot on suspend |
| Snapshots stored | one per actor |

### atelet

DaemonSet `atelet` in `ate-system`, one pod per node. It restores and
checkpoints the actors on its node. Its `objectstore-plugin` container moves
snapshots to and from object storage and compresses them, so its CPU grows with
the node's share of snapshot traffic.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| CPU and memory, `atelet` container | Request 50m CPU and 128 MiB; no limits | Raise the requests to what it uses on a full node at your peak resume rate. With small requests, a busy worker takes most of the node's CPU and slows restores, and when memory runs low, atelet can be evicted before workers that stay within their requests. Its requests come out of what the node has left after the worker, so size the worker to leave room for them. Don't set a memory limit: hitting it would kill atelet and stop resumes and suspends on the node. | Patch the DaemonSet |
| CPU and memory, `objectstore-plugin` container | Request 50m CPU and 128 MiB; no limits | Same as atelet. It needs more CPU with larger snapshots and higher resume rates. | Patch the DaemonSet |
| Node boot disk | Your node pool's | The disk each node boots from, which also holds what pods write to the node. Snapshots and the image cache live under `/var/lib/ate` on it. Size its throughput for the node disk writes above, and its capacity for snapshots in flight plus cached images. | The node pool's disk type and size, for example `--disk-type` and `--disk-size` on `gcloud container node-pools create` |
| `--drain-timeout` | 5 min | How long atelet keeps finishing restores and uploads when its pod stops, for example during a node upgrade. Raise it if large snapshots take longer to upload. | Patch the `atelet` container's args |

## 3. Size the control plane

For components that scale out, start from:

```
replicas = load ÷ (capacity per replica × 0.65), rounded up, plus 1
```

The 0.65 keeps replicas below their limit under uneven load and during
upgrades, and the extra replica covers losing one. The formula assumes
throughput grows in step with replicas, which stops being true when something
the replicas share runs out; each section below says what that is. Confirm the
gain with a measurement.

Every component runs in the `ate-system` namespace except
`podcertificate-controller`, which runs in `podcertificate-controller-system`.

Each section below lists what you may need to set. "Patch" means editing the
object with `kubectl` after the install; the install has no setting for it.

### atenet-router

Deployment `atenet-router`. Each pod runs Envoy and a router sidecar that looks
up each request's actor in ate-api-server. One pod with 4 CPU each for Envoy and
the sidecar serves about 15K short-lived and tiny requests/s.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| CPU and memory, both containers | None | Requests equal to limits. Start from 4 CPU each and measure. **Don't go above 8 CPU per container:** a pod stops scaling with CPU past that, so add replicas instead. | Patch the Deployment |
| Envoy `--concurrency` | Not set: one thread per node CPU | Equal to Envoy's CPU limit | Patch the Envoy container's command |
| Replicas | 1 | The formula above, with ingress requests/s as the load | `kubectl scale` |
| `--parked-request-max` | 1,024 per pod | Requests to suspended actors that may wait at once while workers are full. Raise it if you expect bursts of resumes. | Patch the router container's args |
| `--parked-request-budget` | 5 s | How long such a request may wait before it gets a 503 | Patch the router container's args |

### ate-api-server

Deployment `ate-api-server`. It serves the API, looks up actors for the router,
and runs every resume and suspend. With 2 CPU, a replica handles about 800 resume and suspend cycles/s;
with 4 CPU, a replica handles about 1,500 resume and suspend cycles/s;
with 8 CPU, a replica handles about 2,600 resume and suspend cycles/s.
**Don't go above 8 CPU per container:** a pod stops scaling with CPU past that, so add replicas instead.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| CPU and memory | None | Requests equal to limits. | Patch the Deployment |
| Replicas | 2 | The formula above, with resume and suspend cycles/s as the load | `kubectl scale` |
| Postgres pool size | max(4, CPUs) | Each replica opens up to its pool size plus 5 connections (3 for watches, 2 for migrations and maintenance), and all replicas together must stay under Postgres's `max_connections`. Shrink the pool when you add replicas. | Install setting `ateapi.postgres.poolMaxConns` |

All replicas share one Postgres, so once Postgres runs out of CPU, adding
replicas doesn't help. Size ate-api-server and Postgres together.

### Postgres

StatefulSet `postgres` stores every actor and
worker. It's one instance, so it scales up, not out. It needs about 4 ms of CPU
per resume and suspend cycle.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| CPU and memory | Request 2 CPU and 1 GiB; limit 16 CPU and 2 GiB | Enough CPU to stay under 80% at peak, and enough memory to cache the actor table. Set memory requests equal to limits. | Patch the StatefulSet |
| `max_connections`, `shared_buffers` | Postgres defaults (`max_connections` 100) | Raise `max_connections` to fit every ate-api-server's pool plus 5 per replica; raise `shared_buffers` with memory so the actor table stays cached | Add them to `postgresql.conf` in the `postgres-config` ConfigMap, then restart the `postgres` pod; both take effect only at startup |

### atenet-egress

Deployment `atenet-egress`. Every outbound TCP connection from an actor goes
through it, so it matters only if actors make outbound calls.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| CPU and memory | Request 200m CPU and 512 MiB; memory limit 768 MiB | Raise them if it's throttled or near its memory limit | Patch the Deployment |
| Replicas | 1 | Add replicas as actors' outbound connections and bandwidth grow; measure with your workload | `kubectl scale` |

### ate-controller

Deployment `ate-controller`. It manages WorkerPools and keeps Substrate's list
of workers in sync with the worker pods. It's not on the request path; its load
grows with the number of worker pods and peaks while the fleet is created.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| Replicas | 1 | Keep 1: it has no leader election, so extra replicas would do the same work twice | — |
| CPU and memory | None | Requests from what it uses while the fleet is created | Patch the Deployment |
| `--worker-sync-concurrency` | 2 | Raise it if new worker pods take long to show up as workers in large fleets | Patch the container's args |

### podcertificate-controller

Deployment `podcertificate-controller`. It signs a certificate for each new
worker pod and renews certificates before they expire. A new worker can't take
actors until it has one, so a slow controller slows down growing the fleet.

| Setting | Default | How to choose | How to set |
| :---- | :---- | :---- | :---- |
| Signing concurrency | 1 per signer | Certificates signed in parallel. Raise it for fleets of thousands of workers; 40 worked for 100K workers. | Install setting `podcert.workersPerSigner` |
| Kubernetes API rate limit | 5 requests/s, burst 10 | Raise it for fleets of thousands of workers, so signing isn't held back by the API client | Patch the container's args with `--kube-api-qps` and `--kube-api-burst` |
| Replicas | 1 | Replicas split the certificate requests between them. Add replicas if one replica at high concurrency still can't keep up. | `kubectl scale` |
| CPU and memory | None | Requests from what it uses while the fleet is created | Patch the Deployment |

## 4. Best practices

- **Set resources on every WorkerPool.** Set `spec.template.resources`, with
  memory requests equal to limits.
- **Keep the control plane off the worker nodes in large clusters.** With the
  `cordonControlPlane` install setting, Postgres runs on a node pool of its own,
  and the other control-plane components share a second, small node pool,
  spread across its nodes where possible. Create both pools first, each with a
  matching label and `NoSchedule` taint: `ate.dev/workloadType=ate-postgres`
  and `ate.dev/workloadType=ate-control-plane`. If a pool is missing, its pods
  stay Pending. See the [operator install guide](operator-install.md#settings).
