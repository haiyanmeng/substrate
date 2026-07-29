# sdsmint: a PoC for on-demand per-SNI certificate minting

This is a working proof of concept for the design in [`mint.md`](../../mint.md):
let a substrate actor dial an arbitrary HTTPS destination while the egress proxy
still sees and polices the request, by having Envoy MITM the connection with a
leaf certificate **minted per SNI, on demand**.

It implements the doc's recommended Option C — Envoy's shipped
`on_demand_secret` certificate selector plus the `sni` certificate mapper,
backed by a custom Go SDS server that does the signing. The MITM CA private key
never enters the data plane.

The PoC's job was to de-risk the design: prove the config works on a real Envoy,
and turn the doc's three open questions into measured answers. All three are
answered below, along with three corrections to the doc.

**[EXPLAINER.md](EXPLAINER.md) explains how it works** — the mechanism, the
connection lifecycle, and a walkthrough of the code. This file covers what it
proves.

## Running it

```sh
go test ./poc/sdsmint/...          # unit tests, no Envoy needed
./poc/sdsmint/hack/run-poc.sh      # hermetic end-to-end: 14 assertions
./poc/sdsmint/hack/run-poc.sh --forward-proxy   # + real MITM of example.com: 16
```

The harness downloads `envoy-1.37.5-linux-x86_64` into `poc/sdsmint/__run/`
(gitignored), generates a throwaway CA, starts `sdsmintd`, and runs the checks
and experiments below. `--forward-proxy` adds one leg that needs outbound
internet. `--keep` leaves the processes up for poking at.

Last full run: **16 passed, 0 failed** (Envoy 1.37.5, Linux x86-64).

## What it proves

| | Result |
|---|---|
| A leaf is minted per SNI, on demand | `a.mitm.example` and `b.mitm.example` each get their own leaf — correct CN, SAN matching the SNI, **distinct serials**. `cert_requested=3`, one subscription per name. |
| `prefetch_secret_names` works | `default.mitm.example` is minted at config load, before any request arrives. |
| The hostname allowlist really blocks | A host outside `--allow` fails the handshake with `error:0A000410:SSL routines::ssl/tls alert handshake failure`, is audited server-side, and leaves **no live subscription** (`cert_active` unchanged). |
| Full MITM re-origination works | Through the `dynamic_forward_proxy` bootstrap: real `example.com` content fetched, HTTP 200, while the client is served our leaf (`issuer=CN=sdsmint PoC MITM CA`). The upstream leg is still verified against the system trust store via `auto_sni` + `auto_san_validation`, so the MITM does not weaken origin authentication. |

## Answers to mint.md's open questions

### 1. How does an on-demand secret expire or rotate?

**Rotation is push-driven. Envoy has no TTL of its own for an on-demand
secret.** Measured: with `--rotate` and a 6s TTL, `cert_updated` went `2 -> 4`
and the served serial changed under a held subscription. The leaf's own
`notAfter` does not cause Envoy to re-fetch — nothing expires client-side.

Consequences for the design:

- The minting server owns the rotation clock. `sdsmintd --rotate` re-mints and
  pushes at ~2/3 of TTL for every live subscription.
- Eviction is server-driven too: a name returned in `removed_resources` cancels
  the data plane's subscription for it.
- This is why the doc's `DELTA_GRPC` choice is load-bearing rather than a
  stylistic preference — SotW has no way to withdraw a single name.

### 2. Is the secret cache per-worker or shared?

**Shared.** Measured: 12 concurrent connections to one SNI across
`--concurrency 4` produced `cert_requested +1`, not `+4`.

So the memory footprint of a large live host set scales with the number of
*hosts*, not hosts × workers, and the signing load is one mint per host rather
than one per host per worker. This is the good case for the design; it was the
doc's main scaling worry.

### 3. What happens when the SDS server is down?

Two distinct behaviours, and the second one is the finding that matters:

- **Cached names keep working.** A host whose secret Envoy already holds still
  served HTTP 200 with `sdsmintd` killed. There is no per-connection dependency
  on SDS.
- **Cold names stall forever.** This is worse than "fails". The selector pauses
  the handshake waiting for SDS and **Envoy never gives up** — measured still
  waiting after the client's full 20s budget in the harness, and after 3 minutes
  by hand. The client hangs; it never gets an error.

  The fix is a filter-chain-level `transport_socket_connect_timeout`. With
  `5s` set, the same request fails in **5.03s** with
  `error:0A000126:SSL routines::unexpected eof while reading`. The harness proves
  both legs.

  **`transport_socket_connect_timeout` is therefore required, not optional, in
  any real deployment of this design.** Without it an SDS outage does not
  degrade egress — it wedges every actor that dials a host not already in cache.

  The shipped bootstraps deliberately omit it so the harness can demonstrate the
  default; both carry a comment saying so.

## Corrections to mint.md

1. **"NACK names that fail validation" is backwards.** NACK is a *client* action
   in xDS — a server cannot NACK. The correct way for the server to say "this
   name will not be issued" is to return it in `removed_resources`. The PoC does
   that, and Check 2 confirms it produces a clean handshake failure with no
   lingering subscription.
2. **The design needs Envoy 1.37+.** `on_demand_secret` and `cert_mappers.sni`
   first shipped in 1.37 — `extensions_build_config.bzl` has 0 matches in
   v1.35.13 and v1.36.9, 3 in v1.37.5. On anything older the config is rejected
   outright. `manifests/ate-install/atenet-router.yaml:177` currently pins
   `v1.30-latest`, so **the router's Envoy must be bumped before this design can
   ship**. The harness version-gates and fails with a clear message.
3. **Open question #1 was already answerable from the docs**, and the PoC
   confirms it: *"A resource removal sent via the xDS response will cancel the
   data plane subscription for the specific secret name."*

## Two config requirements that are easy to miss

- **`tls_inspector` is mandatory.** Without that listener filter Envoy never
  reads the ClientHello, so the `sni` mapper has nothing to map and every
  connection silently collapses to `default_value`. It fails as a subtle
  wrong-certificate bug, not as an error.
- **Session resumption must be disabled.** A resumed session skips the handshake
  and therefore skips certificate selection entirely. `disable_stateless_session_resumption`
  and `disable_stateful_session_resumption` sit on `DownstreamTlsContext`, *not*
  inside `common_tls_context`.
- `node.id` and `node.cluster` are required, or the SDS subscription is rejected
  with `TlsCertificateSdsApi: node 'id' and 'cluster' are required`.

## Layout

```
poc/sdsmint/
  ca.go            CA, LoadCA, GenerateCA, Sign -> MintedCert. key is a
                   crypto.Signer so a KMS/HSM signer drops in unchanged.
  minter.go        allowlist + bounded TTL cache + issuance audit log
  server.go        delta SDS: per-stream subscriptions, initial_resource_versions,
                   removed_resources for refusals, optional rotation timer
  cmd/sdsmintd/    the daemon; UDS by default, chmod 0600
  testdata/        envoy-bootstrap.yaml (hermetic), -fwdproxy.yaml (real egress)
  hack/run-poc.sh  the end-to-end harness
```

Zero new Go dependencies — everything needed is already vendored, so `vendor/`
is untouched. The two on-demand protos are only referenced from YAML.

## Security controls implemented

From mint.md's list: hostname allowlist (`--allow`, and `sdsmintd` refuses to
start without one), issuance audit log (one structured `slog` line per mint with
host, serial, notAfter), short leaf TTLs (`--ttl`, default 5m), CA name
constraints (`--ca-name-constraint`), local-only channel (unix socket, mode
0600), and `crypto.Signer` indirection so the CA key can live in a KMS.

## Non-goals

This is not wired into `atenet-router` or any manifest, per the plan. The
version gate above is a finding to report, not a change made here.

## Known rough edges

- `sdsmintd`'s cache is per-process; a replicated deployment mints once per
  replica. Fine for the PoC, worth deciding on before it ships.
- The rotation timer pushes to every live subscription on one shared ticker
  rather than per-name deadlines.
- `GracefulStop` cannot be used unconditionally: an xDS stream never ends on its
  own, so the daemon falls back to a hard stop after a 2s grace period. Worth
  remembering for any other long-lived-stream service in the repo.
