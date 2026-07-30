# How the sdsmint PoC works

A walkthrough of the mechanism. For *results* — what the PoC proves, the
measured answers, and the benchmarks — see [README.md](README.md).

---

## 1. The problem

A substrate actor wants to `GET https://api.stripe.com/v1/charges`. We want to
allow that while still enforcing egress policy — which means the egress proxy has
to *see* the request: the method, the path, the headers. But it's TLS, so the
proxy sees an opaque byte stream and can act on nothing finer than "a TCP
connection to some IP on :443".

The standard answer is to MITM: terminate the client's TLS at the proxy, inspect
and police the plaintext, then re-originate a second TLS connection to the real
origin. For the client's TLS to succeed, the proxy has to present a certificate
for `api.stripe.com` that the client trusts — so we install a MITM CA in the
actor's trust store and sign our own leaf for `api.stripe.com`.

That's the easy part. The hard part is **which certificate, and when**:

- The destination set is open-ended. Actors dial whatever they dial; we can't
  pre-generate a leaf per host.
- We only learn the hostname *mid-handshake*, from the SNI in the ClientHello.
- We'd rather not put the MITM CA private key inside every data-plane proxy pod.
  A CA trusted by every actor is the single most dangerous key in the system, and
  the router is the component most exposed to actor traffic.

So: mint a leaf **per SNI, on demand**, from a signing key that lives somewhere
other than the proxy.

## 2. Why this design

The alternatives are to teach the proxy to sign for itself, or to write a custom
C++ certificate provider. Neither is necessary: two Envoy extensions that
already ship in the box compose into exactly the required behaviour.

- **`on_demand_secret`** — a *certificate selector*. Instead of picking from a
  static list of certs configured on the listener, it pauses the handshake,
  fetches a secret by name over SDS, and resumes once it arrives.
- **`cert_mappers.sni`** — a *certificate mapper*. It decides what name to ask
  for. This one returns the connection's SNI.

Compose them and Envoy's behaviour becomes: *"take the SNI from the ClientHello,
ask my SDS server for a secret with that name, pause the handshake until it
arrives, then complete the handshake with it."* Which is exactly the required
semantics, with no custom C++ filter.

What's left to build is the SDS server — and because it's a separate process,
**the CA key never enters the data plane**. Envoy only ever receives short-lived
leaf keys, over a unix socket. That property is the whole reason to prefer this
over teaching the proxy to sign.

The tradeoff, quantified in the README: SDS is now on the critical path of every
first-contact host.

## 3. The moving parts

```
   actor                    Envoy 1.37+                      sdsmintd
     │                          │                                │
     │  ClientHello             │                                │
     │  SNI=api.stripe.com      │                                │
     ├─────────────────────────►│                                │
     │                          │ tls_inspector reads SNI        │
     │                          │ sni mapper: name="api.stripe.com"
     │                          │ on_demand_secret: cache miss   │
     │                     ┌────┤ ***handshake pauses***         │
     │                     │    │                                │
     │                     │    │  DeltaDiscoveryRequest         │
     │                     │    │  subscribe:[api.stripe.com]    │
     │                     │    ├───────── UDS, HTTP/2 ─────────►│
     │                     │    │                                │ allowlist?
     │                     │    │                                │ cache?
     │                     │    │                                │ CA.Sign()
     │                     │    │  DeltaDiscoveryResponse        │
     │                     │    │  Resource{name, version=serial}│
     │                     │    │◄───────────────────────────────┤
     │                     └───►│ ***handshake resumes***        │
     │  Certificate             │                                │
     │  CN=api.stripe.com       │                                │
     │◄─────────────────────────┤                                │
     │                          │                                │
     │  GET /v1/charges         │  ← plaintext: policy applies here
     ├─────────────────────────►│
                                │  re-originates TLS to the real
                                │  api.stripe.com, verifying its
                                │  cert against the system trust
                                │  store (auto_sni + auto_san_validation)
```

The key insight in the diagram: **"SDS resource name" and "hostname" are the same
string.** The `sni` mapper is what makes them equal, and everything downstream —
the allowlist, the cache key, the SAN, the `Secret.Name` — keys off that one
identifier.

## 4. Walking the code

Four files, roughly 200 lines of substance. Each layer adds one concern.

### `ca.go` — the signing key, and nothing else

`CA` holds the MITM cert and key. `Sign(host, ttl)` issues a leaf: fresh P-256
keypair every call, `CN = SAN = host`, `IsCA=false`, `KeyUsage=DigitalSignature`,
`EKU=ServerAuth`, `NotBefore` backdated a minute for clock skew. It returns
`MintedCert{CertChainPEM, PrivateKeyPEM, NotAfter, Serial}` with the chain as
`[leaf, CA]`.

Two deliberate choices:

- **`key` is typed `crypto.Signer`, not `*ecdsa.PrivateKey`.** A KMS- or
  HSM-backed signer substitutes without touching `Sign`. Getting the CA key out
  of a file is the main production hardening step, and this is the seam that
  makes it a drop-in rather than a rewrite.
- **An IP literal in the SNI goes in `IPAddresses`, not `DNSNames`.** SNI isn't
  supposed to carry IP literals; some clients send them anyway, and a leaf with
  an IP in `DNSNames` is rejected.

`GenerateCA` exists so the PoC can run without external key material. It supports
`PermittedDNSDomains` (marked critical) — a name-constrained CA, so that even a
total compromise of this service can't impersonate hosts outside the constrained
domains.

### `minter.go` — policy and cache in front of the key

`GetCertificate(ctx, host)` is the whole interface. In order:

1. **Validate.** `AllowGlobs` matches the host against operator-supplied
   patterns. Denials are logged at WARN — a burst of them is the signal that
   something is probing the CA.
2. **Cache lookup**, keyed by host, TTL-bounded.
3. **Sign on miss** — *outside the lock*. Keygen is slow enough that holding the
   map lock across it would serialise every handshake in the process. The cost is
   that two concurrent misses on the same host may both sign; one wins the cache
   slot and the loser's cert is still perfectly valid.
4. **Evict** expired entries, then LRU down to `cap`.
5. **Audit**: one structured line per issuance with host, serial, notAfter.

Two things worth calling out:

**`NewMinter` errors if `Validate` is nil.** Not "defaults to allow-all" — errors.
A minting service with no allowlist is an unrestricted impersonation oracle for
every host its CA is trusted for, and that should be impossible to reach by
forgetting a field. `sdsmintd` likewise refuses to start without at least one
`--allow`.

**`*` matches exactly one DNS label.** The first implementation used
`path.Match`, where `*` spans dots — so `*.example.com` would have matched
`a.b.evil.example.com`. An operator writing `*.example.com` does not expect to
authorise unbounded subdomain depth, so `matchLabels` compares label-wise and
requires the label counts to line up. `checkHostSyntax` separately rejects names that should never reach
the signer at all: empty, >253 bytes, containing `*` or path separators or
whitespace, leading/trailing dots, or empty labels.

### `server.go` — speaking delta xDS

`DeltaSecrets` is the real implementation; the SotW methods exist for
completeness (more on that below).

**Per-stream state.** Delta xDS is stateful per stream. `deltaStream.names` maps
name → `{version, touched}`: the version the client last accepted, which is what
makes incremental updates and correct unsubscribes possible, and the last time
the client showed interest, which is what idle withdrawal keys off.

**Version = certificate serial.** A re-mint always looks like a new version to
Envoy; a cache hit always looks like the same one. No separate counter to keep
consistent with reality.

**One send goroutine.** gRPC forbids concurrent `Send` on a stream, and rotation
pushes race with responses to inbound requests. All sends funnel through
`sendCh`, and the main loop is a single `select` over the receive channel, the
rotation ticker, the send-error channel, and `ctx.Done()`.

**Refusal is `removed_resources`, not a NACK.** The intuitive design is to NACK
a name that fails validation — but NACK is a *client* action in xDS; a server has
no NACK to send. The server-side way to say "this name will not be issued" is to
return it in `removed_resources`, which per the Envoy docs also cancels the data
plane's subscription for that name. The paused handshake then fails, which is the
intended outcome for a disallowed host. The code carries this correction as a
comment.

**Inbound NACKs are logged loudly.** A request carrying `error_detail` means
Envoy rejected a certificate we minted — a real bug, not routine traffic.

**`initial_resource_versions` is honoured.** On stream reconnect Envoy replays
what it already holds, so we adopt those names rather than re-pushing
everything — and a replay counts as the client showing interest, so a name that
survives a reconnect is not immediately withdrawn as idle.

**Rotation.** With `--rotate`, a ticker at 2/3 of TTL re-mints every live
subscription and pushes the replacements. This is not an optimisation — see §6.

**Idle withdrawal.** With `--idle`, a second ticker at a quarter of that window
withdraws names the client has not asked about in `--idle`, via
`removed_resources`, and releases the leaf from the mint cache at the same time
through an optional `Forgetter` interface — so reclamation is not one-sided.
Two design points that are easy to get backwards:

- **A rotation push does not count as activity.** It is the server talking to
  the client, not the client saying the name still matters. If it refreshed the
  timestamp, a server running with `--rotate` would never reclaim anything while
  appearing to work. Conversely, `rotateAll` skips names the next sweep is about
  to drop, so a rotation tick costs the *active* set rather than the *live* one.
- **There is no server-observable idleness to key on.** Once Envoy holds an
  on-demand secret it never mentions the name again, however much traffic flows
  for it. So `--idle` is a wall-clock bet that a name nobody has *asked* for
  recently is a name nobody needs. The bet is cheap to lose — the cost is
  exactly one cold handshake — which is what makes it takeable, and the harness
  asserts that rather than assuming it.

### `cmd/sdsmintd/main.go` — the daemon

UDS by default — a local-only channel, because leaf private keys transit this
connection — `chmod 0600`, stale-socket cleanup on start. Flags for TTL, cache
cap, rotation, idle withdrawal, log level, and the required repeatable
`--allow`.

One non-obvious thing, and it cost a debugging session: **`GracefulStop()` cannot
be used unconditionally.** It waits for in-flight RPCs to finish, but an xDS
stream is long-lived *by design* and only ends when Envoy closes it. Calling it
on SIGTERM deadlocks shutdown permanently. The daemon gives it a 2s grace period,
then falls back to `Stop()`. Worth remembering for any other long-lived-stream
service.

## 5. Why delta, not state-of-the-world

`StreamSecrets`/`FetchSecrets` are implemented, but the bootstraps configure
`DELTA_GRPC`, and that choice is load-bearing rather than stylistic:

|  | SotW | Delta |
|---|---|---|
| Refuse a name | only by omission — ambiguous with "still working on it" | `removed_resources` — explicit, and cancels the subscription |
| Add one host | resend every secret held | send one resource |
| Rotate one host | resend every secret held | send one resource |

A SotW response is the complete set for a type URL, so there is no way to say
"this one name is gone" — the closest you get is dropping it from the set, which
Envoy can't distinguish from a server that isn't done yet. With an open-ended
host set, "resend everything on every change" also degrades badly. `buildSotW`
notes the limitation in a comment where the refusal path silently `continue`s.

## 6. Three things that surprised us

All three are measured in the README; they're repeated here because they're
consequences of the *design*, not incidental findings.

**Envoy opens one DELTA_GRPC stream per secret name, and holds it open.** Not
one stream carrying many subscriptions, which is what the server was originally
written to expect. 3,120 live secrets produced 3,120 concurrent streams on the
single SDS connection. Streams are concurrent requests, so **the live host count
is a concurrent-request count against the SDS cluster** — and Envoy's default
`max_requests` circuit breaker of 1024 becomes a silent ceiling on distinct
hosts, past which no secret is ever delivered and handshakes fail ~15 s later
with `initial fetch timed out`. Nearly every capacity prediction made from the
other model was wrong in a way this one explains.

**Envoy has no TTL for an on-demand secret.** It caches what it was given,
indefinitely, until the server sends a new version or a removal. The leaf's own
`notAfter` does not trigger a re-fetch. So if the server doesn't push, a leaf
just expires under a live subscription and handshakes start failing with no
recovery. `rotateFraction = 2/3` exists for that reason — the minting server owns
the rotation clock, and there is no fallback if it doesn't.

**A cold SNI with SDS down stalls forever.** The selector pauses the handshake
and Envoy never gives up — measured still waiting after 3 minutes. The client
hangs; it never gets an error. A filter-chain `transport_socket_connect_timeout`
bounds it (5s → clean failure in 5.03s), which makes that setting **required, not
optional**, in any real deployment. Note that cached hosts are unaffected: an SDS
outage doesn't degrade egress broadly, it wedges exactly the actors dialing a
host not already in cache.

## 7. Config that fails silently

Three bootstrap details that produce wrong behaviour rather than errors:

- **`tls_inspector` is mandatory.** Without that listener filter Envoy never
  reads the ClientHello, so the `sni` mapper has nothing to map and *every*
  connection collapses to `default_value`. Every client gets a certificate for
  the wrong host — a subtle wrong-cert bug, not a startup failure.
- **Session resumption must be disabled.** A resumed session skips the handshake,
  and therefore skips certificate selection entirely. The two
  `disable_*_session_resumption` fields sit on `DownstreamTlsContext`, *not*
  inside `common_tls_context`.
- **`node.id` and `node.cluster` are required**, or the subscription is rejected
  at startup with `TlsCertificateSdsApi: node 'id' and 'cluster' are required`.
  This one at least fails loudly.

Also: `default_value` must itself be in the allowlist, or every no-SNI connection
fails.

## 8. How it's tested

**Unit** (`go test ./poc/sdsmint/...`, 54 tests, race-enabled): CA round-trips
and leaf shape; cache hit/expiry/eviction/concurrency; allowlist semantics
including the one-label wildcard. `server_test.go` drives the delta protocol
through fake gRPC streams — subscribe, refuse, bare ACK, rotation, unsubscribe,
`initial_resource_versions`, wrong type URL, inbound NACK. `idle_test.go` covers
withdrawal from both ends: what expires a name, what keeps one alive (a
subscribe and a reconnect replay, but deliberately not a rotation push), and
that a withdrawn host is served again on the next request.

**End-to-end** (`hack/run-poc.sh`): a real Envoy 1.37.5 binary against a real
`sdsmintd`. The harness asserts on Envoy's own admin stats
(`listener.mitm.on_demand_secret.{cert_requested,cert_updated,cert_active}`) and
on `openssl s_client` output, so results are measured rather than inferred from
"curl exited 0". It version-gates on 1.37 and fails with a clear message on older
builds.

The three experiments each target one of the questions that could not be settled
on paper, and are designed so the answer is a *number*: `cert_requested +1` vs
`+4` settles shared-vs-per-worker cache; a changed serial under a held
subscription settles push-vs-TTL rotation; two timing legs settle the SDS-down
failure mode.

**At scale** (`hack/run-scale.sh`): nine phases against a real Envoy, each
isolating one question, with a static-certificate control so every number is a
difference rather than an absolute. This is where the one-stream-per-secret
model and the unbounded live set were found, and where `--idle` is measured
against a control arm. See the README for the results.

## 9. What this is not

- **Not wired into `atenet-router` or any manifest.** Deliberately — the router
  pins Envoy `v1.30-latest` and these extensions need 1.37+. That version gate is
  the finding; bumping it is a separate decision.
- **Not a production SDS server.** The cache is per-process, so a replicated
  deployment mints once per replica, and it does not survive a restart — which
  matters, because a restart replays every stream at once. Rotation and idle
  withdrawal are shared tickers rather than per-name deadlines. There's no mTLS
  on the SDS channel (UDS + 0600 instead) and no CA rotation story.
- **Not a policy engine.** It proves the proxy can *see* the plaintext. What to
  do with it — allow, deny, log, rewrite — is the existing egress-policy question
  and is untouched here.
