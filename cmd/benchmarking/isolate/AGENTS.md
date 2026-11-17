# Isolate-mode fakes

`fake-atelet` and `fake-workersync` replace the data plane so benchmarks
measure ate-api-server and Postgres. `fake-atelet` takes atelet's place, and
answers for the worker behind it. `fake-workersync` takes the place of
ate-controller's WorkerPool controller and worker syncer. For what they do and
how benchmarks tune them, see "Isolate Mode" in `docs/guides/benchmarking.md`.
Update that section when you change behavior a benchmark user can see.

## Fidelity

- ate-api-server must not be able to tell the fakes from the real components.
  Match the responses, callbacks, error codes, and load it sees. Nothing else
  needs to be real: no workload runs and no snapshot holds data.
- Match the contract, not just the types. When you change a fake's response,
  read the `ateletpb` proto comment, the real implementation (`cmd/atelet`,
  `cmd/atecontroller`), and the ate-api-server code that reads it, such as
  `cmd/ateapi/internal/controlapi/workflow_*.go`.
- Say so in a comment where a fake differs from the real component, and why.

## fake-atelet

- Every upload writes one placeholder object under the snapshot URI. Tagging
  copies an actor's snapshot, and ate-api-server refuses to copy an empty one.
- fake-atelet calls ate-api-server under this pod's atelet identity. It mints
  actor certificates on Run and Restore when an egress gateway is named, and
  relays capacity reports. Keep these calls: their load is part of what the
  benchmark measures.
- The relay admits only fake-workersync's SPIFFE ID and passes requests and
  status codes through unchanged. It makes no decisions; ate-api-server checks
  that the Worker is on this node.
- `--delay` is the data plane's share of each call. Certificate mints add to it.
  Add a per-call delay flag only for a call that a benchmark times.

## fake-workersync

- The controller lists Workers from ate-api-server only at startup (`adopt`). A
  pass with nothing changed and nothing draining makes no ate-api-server
  calls, so a steady fleet adds no load. Keep it that way.
- Every reconcile step is idempotent, so the next pass retries a failed one.

## Shared

- Worker names, node placement, and the relay port come from
  `internal/fakeworker`. Both binaries must agree on them, so change them there.
- Both fakes keep their state in memory only. The resulting restart behavior
  is a known limitation, documented in `fake-atelet/epochs.go` and the guide.
