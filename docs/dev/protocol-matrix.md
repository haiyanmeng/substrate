# Protocol handling on the ingress and egress paths

How Substrate treats each wire protocol on the way *into* an actor (ingress) and on
the way *out* of an actor (egress), which identity is presented at each hop, and
exactly what Envoy does with the bytes.

Everything below is derived from the code and manifests on `main`, as of
`60073ecd` (2026-08-25):

| Concern | Source |
| --- | --- |
| Ingress Envoy config (xDS) | `cmd/atenet/internal/router/xds.go` |
| Ingress ext_proc | `cmd/atenet/internal/router/ingress/ingress.go` |
| Ingress terminator inside the worker | `internal/atunnel/ingress.go` |
| Actor packet interception | `internal/ateomnet/net.go` |
| Egress tunnel client | `internal/atunnel/client.go`, `internal/atunnel/original_dst_linux.go` |
| Egress Envoy config (shipped) | `manifests/ate-install/atenet-egress.yaml` |
| Egress Envoy config (experimental MITM) | `manifests/ate-install/atenet-egress-with-sdsmint.yaml` |
| Egress ext_proc | `cmd/atenet/internal/router/egress/egress.go` |
| Additional MITM-leg ext_proc (experimental) | `hack/experimental-additional-egress-extproc.sh` |
| Actor certificate minting | `cmd/ateapi/internal/actoridentity/actoridentity.go`, `internal/atunnel/credential.go` |
| DNS | `manifests/ate-install/atenet-dns.yaml`, `cmd/atenet/internal/dns/corefile.go` |
| Egress e2e coverage | `internal/e2e/suites/networking`, `internal/e2e/suites/egressmitm`, `internal/e2e/suites/egressauthz` |

Two Envoy egress configurations exist. `hack/install-ate.sh` selects between them
(`--experimental-use-sdsmint`); the **default is `atenet-egress.yaml`**, which is
pure CONNECT-to-TCP passthrough with no TLS interception. The MITM configuration
is described separately wherever the two differ.

Two install flags modify those configurations rather than replacing them:

* `--experimental-additional-egress-extproc-service NS/SVC:PORT` (requires
  `--experimental-use-sdsmint`) splices a second ext_proc filter into both MITM
  HTTP chains, over the `#ATE_MITM_EXTPROC_FILTER` markers. See §3 and §2.1.
* `--atenet-router=agentgateway` swaps the dataplane binary itself
  (`manifests/ate-install/agentgateway-egress`, a Kustomize overlay on
  `atenet-egress.yaml`). Everything in this document describes the Envoy
  dataplane, which is the default; the agentgateway comparison lives in
  `agentgateway-vs-envoy.md`.

Sections 4 and 5 are the per-protocol matrices — ingress and egress respectively.
They are independent: a protocol's ingress row says nothing about its egress row,
and most protocols behave completely differently in the two directions.

---

## 1. Topology

### 1.1 Ingress (inbound to an actor)

```
                  no client auth                    mTLS                    plaintext
                  (server TLS only)          (podidentity ↔ podidentity)     (veth)
 client  ───────────────────────►  atenet-router  ───────────────────►  atunnel  ──────►  actor
                                      (Envoy)                          (worker pod)     169.254.17.2
                                         │
                                         │ ext_proc (unix/TCP :50051)
                                         ▼
                                  atenet router --mode=ingress
                                    (park → resume actor → return
                                     dynamic metadata {local, port})
```

Envoy listeners on the router (`xds.go`):

| Listener | Port | Downstream TLS | Purpose |
| --- | --- | --- | --- |
| `ingress_http_listener` | 8080 (Service :80) | none | plaintext HTTP |
| `ingress_https_listener` | 8443 (Service :443) | serving cert via SDS `https_serving_cert`, **no client cert validation** | TLS HTTP |
| `connect_terminate` | 8081 | none | terminates HTTP CONNECT |
| `connect_terminate_tls` | 8444 | serving cert | terminates HTTP CONNECT over TLS |
| `main_internal` | internal listener | n/a | reinjection point for tunnelled bytes |

Each of the four socket listeners binds twice: a primary `0.0.0.0` socket plus an
`AdditionalAddresses` entry on `::` with `Ipv4Compat: false`
(`dualStackAdditionalAddresses`, `xds.go:1114`), and the `atenet-router` Service
is `ipFamilyPolicy: PreferDualStack`. Address family is orthogonal to everything
else in this document — no listener, filter, or ext_proc handler reads the peer
address — but it is why a listener dump shows two addresses per port.

### 1.2 Egress (outbound from an actor)

```
 actor ──TCP──► nftables REDIRECT ──► atunnel :15001 ──mTLS + CONNECT──► atenet-egress ──► origin
  │             (only TCP!)            SO_ORIGINAL_DST   (actor cert)        (Envoy)
  │
  └──UDP──► nftables MASQUERADE ─────────────────────────────────────────────────────► origin
            (unpoliced, bypasses the gateway entirely)
```

`InstallActorNftablesRules` in `internal/ateomnet/net.go` installs exactly two
relevant rules:

* `prerouting`: `ip saddr 169.254.17.2 && l4proto tcp` → `REDIRECT` to atunnel's
  egress port. REDIRECT preserves the original destination so atunnel can recover
  it with `SO_ORIGINAL_DST`.
* `postrouting`: `MASQUERADE` for everything else, "notably DNS over UDP, so
  hostname resolution continues to work."

**This single fact determines half of §5: only TCP is tunnelled and policed.
Every UDP flow — DNS, QUIC, anything else — leaves the worker pod by SNAT and
never touches the egress gateway or ext_proc.** There is a `TODO` in that file to
narrow the masquerade to the cluster resolver and drop the rest.

---

## 2. Identity ledger

Every credential presented anywhere on either path.

| # | Hop | Who presents | Identity | Verified by | Verified how |
| --- | --- | --- | --- | --- | --- |
| I1 | client → router :8443/:8444 | router (server) | servicedns serving cert (`servicedns.podcert.ate.dev/identity`) | client | ordinary WebPKI/cluster trust; **the client presents nothing** |
| I2 | router → atunnel :443 | router (client) | `spiffe://cluster.local/ns/ate-system/sa/atenet-router` (podidentity) | atunnel | `VerifyConnection` requires URI SAN == `--atunnel-client-identity` |
| I3 | router → atunnel :443 | atunnel (server) | worker podidentity SVID | router | `MatchTypedSubjectAltNames` URI **prefix** `spiffe://cluster.local/` (`--upstream-spiffe-prefix`) |
| I4 | atunnel → actor | none | none | — | plaintext HTTP/1.1 to `169.254.17.2:<port>` over the veth |
| E1 | atunnel → atelet credential broker | atelet (server) | `spiffe://cluster.local/ns/ate-system/sa/atelet` | atunnel | exact URI match **plus** matching NodeName/NodeUID |
| E2 | atunnel → egress gateway :443 | atunnel (client) | **actor cert**: `spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>` + `ActorIdentity{Atespace, ActorName, ActorUid, Purpose=atunnel}` under PEN OID `1.3.6.1.4.1.11129.2.12` | gateway Envoy, then egress ext_proc | Envoy: `require_client_certificate: true` against `/run/actor-id-ca-certs/ca.crt`. ext_proc: re-parses the chain from XFCC, re-verifies in Go, refuses `IsCA`, requires ClientAuth EKU and `Purpose == atunnel`, authorizes on **UID** and `ACTOR_STATE_RUNNING` |
| E3 | atunnel → egress gateway :443 | gateway (server) | servicedns serving cert | atunnel | `ServerName` = gateway host, trust bundle = servicedns clusterTrustBundle |
| E4 | MITM leaf (sdsmint only) | gateway (server, to the actor) | per-SNI leaf minted on demand by `sdsmintd` | the actor's own TLS stack | only succeeds if the actor image trusts the MITM CA |
| E5 | gateway → origin | gateway (client) | none (no client cert) | origin | — |
| E6 | gateway → origin (sdsmint TLS chain) | origin (server) | origin's real cert | gateway | `auto_sni` + `auto_san_validation` against `/etc/ssl/certs/ca-certificates.crt` |

Two consequences worth stating plainly:

* **Ingress is unauthenticated at the edge.** `ingress_https_listener` has no
  `validation_context` for downstream certs. The package doc on the ingress
  ext_proc says it: *"Everything reaching this handler is unauthenticated client
  input."* Authentication of the *caller* is not this layer's job; the actor
  identity asserted downstream (I2) is the router's own, not the caller's.
* **Egress is authenticated per actor**, and the actor's private key never leaves
  atunnel — only a CSR crosses the atelet unix socket (`internal/atunnel/credential.go`).

### 2.1 How identity crosses the CONNECT boundary

Envoy cannot read the custom `ActorIdentity` OID, so the whole peer certificate is
handed to ext_proc via `x-forwarded-client-cert` with
`forward_client_cert_details: SANITIZE_SET` and
`set_current_client_cert_details.chain: true`. The egress ext_proc reads the
`Chain=` value and re-parses it. See `docs/dev/egress-identity-filter-state.md`.

XFCC only reaches the CONNECT leg. Envoy terminates the CONNECT, so the tunnelled
request is a separate transaction and no header set on the outer request survives
into `mitm_listener` — while a header sent from *inside* the tunnel is actor-
controlled and would let one actor name another. The sdsmint manifest therefore
republishes the verified URI SAN as filter state on the CONNECT leg: a
`set_filter_state` filter writes `%DOWNSTREAM_PEER_URI_SAN%` to object key
**`ate.actor.identity`** (`envoy.string`, `shared_with_upstream: ONCE`,
`read_only: true`, `skip_if_empty: true`, `omit_empty_values: true`), and the
`mitm_internal` cluster carries it across the internal hop with an
`internal_upstream` transport socket. Two objects ride that hop:
`ate.actor.identity` and `envoy.network.transport_socket.original_dst_address`
(§3). Downstream, both MITM access logs emit it as the `actor` field, and the
optional ext_proc filter receives it as `request_attributes:
[filter_state['ate.actor.identity']]`.

None of this exists in the shipped `atenet-egress.yaml`, which has no MITM leg to
carry identity to.

On the ingress side the outer CONNECT authority is captured into filter state
(`dev.ate.authority`, factory key `envoy.string`, `shared_with_upstream: ONCE`) and
surfaced to ext_proc as the request attribute
`filter_state['dev.ate.authority']` — because after reinjection the inner
request's `:authority` is unrelated to the actor being addressed.

---

## 3. What Envoy actually does, per path

### Ingress, non-CONNECT (`ingress_http_listener`, `ingress_https_listener`)

1. Terminate TCP (and TLS, serving cert only).
2. HCM parses HTTP. Every HCM is `CodecType: AUTO` — the CONNECT ones say so
   explicitly, the others inherit it as the enum's zero value. See §3.1 for what
   `AUTO` can and cannot detect here.
3. `authorityFilterStateFilter` stores `%REQ(:AUTHORITY)%` in `dev.ate.authority`.
4. ext_proc call to the router sidecar with
   `request_attributes: [filter_state['dev.ate.authority']]` and the
   `envoy.filters.listener.original_dst` metadata namespace forwarded both ways.
5. ext_proc parses `<actor>.<atespace>.actors.resources.substrate.ate.dev`, parks
   the request, resumes the actor if needed, and returns dynamic metadata
   `{local: <workerIP>:443, port: <targetPort>}`. The port is **hardcoded 443** in
   `ingress.go` — worker `:444` is provisioned but never dialled by this dataplane.
6. `buildRoutes` injects
   `X-Ate-Target-Port: %DYNAMIC_METADATA(envoy.filters.listener.original_dst:port)%`.
7. `buildOriginalDstCluster` (`Cluster_ORIGINAL_DST`, keyed on the `local`
   metadata) dials the worker with upstream mTLS (I2/I3), **pinned to HTTP/1.1**
   via `HttpProtocolOptions`.

### 3.1 Codec selection, and why no ALPN means no h2 over TLS

`CodecType: AUTO` resolves the downstream codec in three steps: use the
ALPN-negotiated protocol if there is one; else sniff for the HTTP/2 connection
preface (`PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n`) and pick HTTP/2 on a match; else
HTTP/1.1. It never selects HTTP/3 — that needs a QUIC listener and
`CodecType: HTTP3`, neither of which exists (§4.8).

`buildDownstreamTlsTransportSocket` (`xds.go:1167`) sets **no `alpn_protocols`**;
the `DownstreamTlsContext` carries only an SDS certificate config. With no ALPN
list advertised the server selects no ALPN protocol, so standard clients fall
back to HTTP/1.1 on `:8443` and `:8444`. Step 1 of `AUTO` is therefore inert on
the TLS listeners, and **h2 reaches the router only as h2c prior-knowledge on the
plaintext listeners**, detected by preface sniffing. Advertising `h2` in
`alpn_protocols` is the change that would enable HTTP/2 over TLS on ingress.

### Ingress, CONNECT (`connect_terminate`, `connect_terminate_tls`)

The two listeners are twins. `buildConnectTerminateListener` (`xds.go:1224`) and
`buildConnectTerminateTLSListener` (`xds.go:1258`) differ in exactly three
things — listener name/stat prefix, port (`connectPlainTextPort` 8081 vs
`connectTLSPort` 8444), and the presence of
`TransportSocket: buildDownstreamTlsTransportSocket()` on the TLS one's filter
chain. Both call the same `buildConnectTerminateHCM`, so all HTTP behaviour below
is shared by construction. Creation is gated on `connectPlainTextPort > 0` and on
`connectTLSPort > 0 && certPath != ""` respectively (`xds.go:444-454`).

What the transport socket does and does not buy:

* On `:8081` the CONNECT authority — the target `IP:port` — is **cleartext on the
  wire**. On `:8444` it is inside TLS.
* `:8444` advertises no ALPN (§3.1), so clients fall back to HTTP/1.1 CONNECT.
  h2 CONNECT is reachable only as h2c prior-knowledge on `:8081`, which is the
  only place `allow_connect: true` has any effect.
* **Neither listener authenticates the client.** The TLS twin's context carries an
  SDS cert config and no `validation_context`, so it adds confidentiality and
  server identity, not client authentication (see I1 in §2).

1. HCM with `upgrade_configs: [{upgrade_type: CONNECT}]` and
   `http2_protocol_options.allow_connect: true`, `timeout: 0s`.
2. Authority captured to filter state as above.
3. Route `connect_matcher` → cluster `main_internal` — the tunnelled bytes are
   **reinjected into the `main_internal` internal listener**, which runs a full
   HCM and re-parses them as HTTP. The design assumption is explicit: *every
   CONNECT-tunnelled protocol this router understands is HTTP.*
4. From there it is identical to the non-CONNECT path (ext_proc → ORIGINAL_DST →
   atunnel over mTLS, HTTP/1.1).

### Egress, shipped (`atenet-egress.yaml`)

1. Listener `egress` on `[::]:443` with `ipv4_compat: true` — one dual-stack
   socket rather than the router's two, since nothing on this path reads the peer
   address (identity is the certificate) and IPv4 peers simply arrive as
   `::ffff:`. The admin socket is bound the same way. Filter chain named `egress`
   (the name must match `EgressFilterChainName` — it is the direction-dispatch
   key).
2. `require_client_certificate: true`, trusted CA = the actor-identity CA.
3. HCM `codec_type: HTTP1`, `upgrade_configs: [CONNECT]`,
   `forward_client_cert_details: SANITIZE_SET`, `chain: true`,
   `flush_log_on_tunnel_successfully_established: true`.
4. ext_proc (`failure_mode_allow: false`,
   `request_attributes: [xds.filter_chain_name]`) performs the authorization in
   §2/E2. Non-CONNECT methods are rejected outright.
5. Route `connect_matcher` → `egress_forward_proxy` (`dynamic_forward_proxy`,
   `V4_ONLY`). **The tunnel is opaque TCP from here on.** Envoy never looks at
   the tunnelled bytes.

### Egress, experimental MITM (`atenet-egress-with-sdsmint.yaml`, Envoy ≥ 1.37)

Listener A is as above, except the route target is the internal listener
`mitm_internal`, `timeout: 0s`, and two `set_filter_state` filters run, both with
`shared_with_upstream: ONCE`:

* `envoy.network.transport_socket.original_dst_address` ← `%REQ(:AUTHORITY)%`.
  The *behavioural* key: it tells `egress_tcp_passthrough` where to connect.
* `ate.actor.identity` ← `%DOWNSTREAM_PEER_URI_SAN%`. The *identity* key (§2.1).

`mitm_internal` carries both across the internal hop via an `internal_upstream`
transport socket wrapping a `raw_buffer` — there is no TLS on that hop, the
tunnelled TLS being terminated on `mitm_listener`. Omitting the wrapper fails
silently: the request proxies and every read of either object resolves to the
zero value.

Both HTTP chains carry an `#ATE_MITM_EXTPROC_FILTER` comment marker in their
`http_filters`, and the cluster list carries `#ATE_MITM_EXTPROC_CLUSTER`. They are
inert until `hack/install-ate.sh --experimental-additional-egress-extproc-service
NS/SVC:PORT`, which splices in an ext_proc filter (`failure_mode_allow: false`,
`message_timeout: 2s`, `request_attributes: [filter_state['ate.actor.identity']]`,
`disallow_system`/`disallow_is_error` mutation rules) and an mTLS `STRICT_DNS`
cluster pinned to TLS 1.3 and authenticated with the gateway's own pod identity.
That filter is the checkpoint that can police a *hostname, method, and path*,
which the CONNECT checkpoint — seeing only `IP:port` — cannot. The passthrough
chain carries no marker: a `tcp_proxy` has no HTTP filter chain, and an opaque
stream has no request to authorize.

Listener B `mitm_listener` (`internal_listener: {}`) has
`listener_filters: [tls_inspector, http_inspector]`,
`listener_filters_timeout: 1s`, `continue_on_listener_filters_timeout: true`
(so server-speaks-first protocols do not hang), and three filter chains:

| Chain | Match | Behaviour | Access log `leg` |
| --- | --- | --- | --- |
| 1 | `transport_protocol: tls` | HCM `mitm_http`. On-demand cert selection (`custom_tls_certificate_selector: on-demand`, DELTA_GRPC SDS to `sds_mint`, `certificate_mapper: sni`, both session resumptions disabled). `alpn: [h2, http/1.1]`. `upgrade_configs: [websocket]`. Routes: `content-type` prefix `application/grpc` → `egress_forward_proxy_grpc`, else `egress_forward_proxy` (**pinned HTTP/1.1**). | `mitm` |
| 2 | `raw_buffer` + `application_protocols: [http/1.0, http/1.1, h2c]` | HCM `mitm_cleartext`, `upgrade_configs: [websocket]` → `egress_forward_proxy_cleartext` (no upstream TLS, `use_downstream_protocol_config`). | `cleartext` |
| 3 | `raw_buffer` (fallback) | `tcp_proxy` → `egress_tcp_passthrough` (`type: ORIGINAL_DST`). Deliberately blind. | `passthrough` |

Chains 1 and 2 both log `actor: %FILTER_STATE(ate.actor.identity:PLAIN)%`, so a
MITM-leg log line names the actor as well as the destination. Chain 3's
`tcp_proxy` does not.

Pinning chain 1's default cluster to HTTP/1.1 is what lets WebSocket upgrades
survive; the cost is that non-gRPC h2 is downgraded and trailers are dropped.
Chain 2's cluster is the opposite — `use_downstream_protocol_config`, so
cleartext h2c is relayed as h2c and trailers survive (§5.9).

---

## 4. Per-protocol matrix: ingress

Inbound to an actor. Identities on every hop are I1–I4 in §2.

| Protocol | Ingress behaviour | § |
| --- | --- | --- |
| raw TCP | not served | 4.1 |
| raw UDP | not served | 4.1 |
| TLS (non-HTTP) | not served | 4.1 |
| SSH | not served | 4.1 |
| DNS | separate service (`atenet-dns`), not an actor ingress path | 4.2 |
| HTTP/1.0 | served | 4.3 |
| HTTP/1.1 GET | served — the mainline path | 4.4 |
| HTTP/1.1 CONNECT | terminated + reinjected into `main_internal` | 4.5 |
| HTTP/2 GET | h2c prior-knowledge only (no ALPN advertised) | 4.6 |
| HTTP/2 CONNECT | terminated + reinjected; `:8081` only in practice | 4.7 |
| HTTP/3 GET | **unsupported** | 4.8 |
| HTTP/3 CONNECT | **unsupported** | 4.8 |
| WebSocket over HTTP/1.1 | **rejected** — no `websocket` upgrade config | 4.9 |
| WebSocket over HTTP/2 | **rejected** | 4.9 |
| WebSocket over HTTP/3 | **unsupported** | 4.9 |

### 4.1 Protocols with no ingress path: raw TCP, raw UDP, non-HTTP TLS, SSH

None of these are served. Every ingress listener terminates in an HCM — there is
no `tcp_proxy` anywhere on the router and no UDP listener at all, so there is no
non-HTTP entry point to an actor. TLS ingress on `:8443`/`:8444` terminates into
an HCM too, so a TLS-wrapped non-HTTP protocol has nowhere to go once decrypted.

### 4.2 DNS

DNS is a service Substrate *provides*, not a path into an actor. `atenet-dns`
runs CoreDNS with an `atenet dns` controller sidecar; the `dns` Service exposes
53/UDP and 53/TCP.

`cmd/atenet/internal/dns/corefile.go` generates a wildcard `template IN A` zone
for `resources.ActorDNSSuffix`, matching `^<name>\.<atespace>\.<suffix>\.$` and
answering `{{ .Name }} 60 IN A <routerIP>`. So
`my-actor.demo.actors.resources.substrate.ate.dev` resolves to the router — which
is what makes the `:authority`-based actor lookup in the ingress ext_proc work at
all.

### 4.3 HTTP/1.0

Handled by the ordinary HCM on `:8080`/`:8443` like any other HTTP request.

### 4.4 HTTP/1.1 GET (and other ordinary methods)

The mainline path. Nothing on it is method-specific: `buildRoutes`
(`xds.go:788-794`) has one route matching `prefix: "/"` with no method matcher,
and ext_proc dispatches on `:authority`. POST, PUT, DELETE and HEAD behave
exactly as GET does. HTTP/1.1 splits three ways here — ordinary methods in this
section, CONNECT in §4.5, and `Upgrade: websocket` in §4.9, which is rejected.

Client → `:8080` or `:8443` (no client cert) → authority captured → ext_proc
resolves the actor, parks, resumes → ORIGINAL_DST cluster → mTLS to atunnel
`:443` (I2/I3) → `atunnel.Serve` verifies the router's URI SAN, re-checks that
the Host's actor is the one currently active (returns **421** with
`X-Ate-Assignment-Stale` if not), strips `X-Ate-Original-Host` and
`X-Ate-Target-Port`, and reverse-proxies to `http://169.254.17.2:<targetPort>`
over plain HTTP/1.1.

### 4.5 HTTP/1.1 CONNECT

`connect_terminate` (`:8081`) / `connect_terminate_tls` (`:8444`). Envoy
terminates the CONNECT, captures the outer authority into filter state, and routes
to the `main_internal` internal listener, where the tunnelled bytes are re-parsed
as HTTP by a second HCM. The inner request's `:authority` is ignored for actor
resolution — that is precisely why `dev.ate.authority` exists.

`internal/atunnel/ingress.go` also implements a CONNECT terminator on the worker
(`ServeConnect`, `DefaultConnectPort = 444`, `NextProtos = [h2, http/1.1]`,
hijack + `HTTP/1.1 200 Connection Established`). It is provisioned by
`workerpool_apply.go` but the Envoy dataplane hardcodes `:443`, so it is not on
the live ingress path today.

### 4.6 HTTP/2 GET (and other ordinary methods)

Reachable only as h2c prior-knowledge on the plaintext listeners — no
`alpn_protocols` is configured, so h2 cannot be negotiated over TLS on `:8443`
(§3.1).

Where it does arrive, note the downstream/upstream asymmetry:
`buildOriginalDstCluster` pins `HttpProtocolOptions` to **HTTP/1.1**, so an h2
request from the client is downgraded before it reaches atunnel. atunnel's
`Serve` is likewise an HTTP/1.1 reverse proxy to the actor.

### 4.7 HTTP/2 CONNECT

`buildConnectTerminateHCM` sets both `upgrade_configs: [{upgrade_type: CONNECT}]`
and `http2_protocol_options.allow_connect: true` — Envoy rejects h2 CONNECT
without the latter — so h2 CONNECT is accepted on the CONNECT-terminating
listeners and follows the same reinjection path as §4.5.

In practice it arrives on the plaintext `:8081` as h2c prior-knowledge; `:8444`
advertises no ALPN, so clients there fall back to HTTP/1.1 CONNECT (§3.1). That
makes `:8081` the only place `allow_connect: true` has any effect.

### 4.8 HTTP/3, all forms

**Unsupported.** There is no QUIC listener, no `envoy.transport_sockets.quic`,
and no UDP listener anywhere in the repo; HTTP/3 appears only as a row in the
comparison table in `agentgateway-vs-envoy.md`. `CodecType: AUTO` cannot reach
HTTP/3 either (§3.1) — it would need an explicit `CodecType: HTTP3` on a QUIC
listener.

Ingress simply has no HTTP/3 to serve. The egress side of the same gap is worse:
see §5.11.

### 4.9 WebSocket, all versions

**Rejected on ingress.** There is **no `websocket` entry in any ingress
`upgrade_configs`** — `xds.go` lists only `CONNECT`. Envoy answers or strips an
`Upgrade: websocket` request rather than establishing a tunnel, so WebSocket
ingress to an actor does not work on the current router config, over HTTP/1.1 or
HTTP/2. Over HTTP/3 it is unsupported for the reasons in §4.8.

Enabling the HTTP/1.1 form means adding
`upgrade_configs: [{upgrade_type: websocket}]` to the ingress HCMs; the upstream
cluster is already pinned to HTTP/1.1, which is what an upgrade needs. The HTTP/2
form (RFC 8441 extended CONNECT) would additionally need
`http2_protocol_options.allow_connect` on the ingress HCMs, plus ALPN so h2 can
be negotiated at all.

---

## 5. Per-protocol matrix: egress

Outbound from an actor. Identities on every hop are E1–E6 in §2.

**tunnelled** = TCP, REDIRECTed to atunnel, carried inside CONNECT with the actor
certificate, authorized by ext_proc. **bypass** = leaves the worker pod by
MASQUERADE with no identity, no authorization, and no log.

| Protocol | Shipped (`atenet-egress.yaml`) | sdsmint (experimental) | § |
| --- | --- | --- | --- |
| raw TCP | tunnelled, opaque | tunnelled → chain 3 passthrough | 5.1 |
| raw UDP | **bypass** | **bypass** | 5.2 |
| DNS | **bypass** (UDP) / tunnelled (TCP) | same | 5.3 |
| TLS (non-HTTP) | tunnelled, opaque | tunnelled → chain 1, MITM attempted, **breaks** | 5.4 |
| SSH | tunnelled, opaque | tunnelled → chain 3 passthrough | 5.5 |
| HTTP/1.0 | tunnelled, opaque | chain 2 cleartext | 5.6 |
| HTTP/1.1 GET | tunnelled, opaque | chain 1 (TLS) or chain 2 (cleartext) | 5.7 |
| HTTP/1.1 CONNECT | tunnelled opaquely, works | **broken** — no chain routes CONNECT | 5.8 |
| HTTP/2 GET | tunnelled, opaque | chain 1, downgraded to HTTP/1.1 unless gRPC | 5.9 |
| HTTP/2 CONNECT | as HTTP/1.1 CONNECT | as HTTP/1.1 CONNECT | 5.10 |
| HTTP/3 GET | **bypass** (UDP) | **bypass** | 5.11 |
| HTTP/3 CONNECT | **bypass** | **bypass** | 5.11 |
| WebSocket over HTTP/1.1 | tunnelled, opaque (works) | chains 1 and 2, `upgrade_configs: [websocket]` | 5.12 |
| WebSocket over HTTP/2 | tunnelled, opaque (works) | not supported | 5.13 |
| WebSocket over HTTP/3 | **bypass** | **bypass** | 5.14 |

### 5.1 Raw TCP

The canonical tunnelled case. The actor connects; nftables REDIRECTs to atunnel
`:15001`; atunnel recovers the original destination with `SO_ORIGINAL_DST` (IPv4
only) and issues `CONNECT <ip>:<port>` to the gateway over mTLS.
`validateDestination` in `internal/atunnel/client.go` **rejects hostnames** — the
CONNECT authority is always an IP literal, because the actor already resolved the
name itself:

```go
if net.ParseIP(host) == nil {
    return fmt.Errorf("...host must be an IP address")
}
```

Identities: E2 (actor cert) and E3 (gateway serving cert). Envoy terminates the
CONNECT and opens a plain TCP connection to the authority. In the shipped config
it never inspects a byte of the payload. Under sdsmint the bytes reach
`mitm_listener` and, being neither TLS nor HTTP, land on chain 3.

### 5.2 Raw UDP (not DNS)

Not tunnelled, not authorized, not logged. The nftables rule matches `l4proto
tcp` only, so UDP falls to the postrouting MASQUERADE and leaves with the worker
pod's IP. No actor identity is presented and ext_proc is never consulted.

This is the largest gap in the egress policy story and is the subject of the
`TODO` in `internal/ateomnet/net.go`.

### 5.3 DNS

The actor resolves names itself using the worker pod's `/etc/resolv.conf`, i.e.
cluster DNS. Over UDP that is a bypass (§5.2) — the explicitly intended
compatibility hole, since resolution has to work before the tunnel can carry
anything. DNS over TCP would be tunnelled like any other TCP flow.

A consequence worth drawing out: because the actor resolves first and the CONNECT
authority is therefore an IP literal, **the gateway never sees the hostname the
actor asked for** in the shipped config. Only the MITM chain recovers it, from
SNI or the `Host` header — which is why hostname-based policy
(`ALLOW_BY_HOSTNAME`) exists only downstream of MITM.

### 5.4 TLS, not HTTP

Shipped: tunnelled and opaque; the origin's certificate is presented directly to
the actor, end to end.

sdsmint: `tls_inspector` matches chain 1 on any ClientHello and Envoy **attempts
to MITM it** — it mints a leaf for the observed SNI and speaks HTTP to the
client. A non-HTTP TLS protocol fails here twice over: the actor rejects the
minted leaf unless it trusts the MITM CA, and even if it did, the payload is not
HTTP so `mitm_http` cannot parse it.
`docs/dev/non-http-egress-manual-test.md` records the first failure verbatim:
`TLS: server certificate not trusted`.

Making this pass through instead would mean qualifying chain 1 with
`application_protocols: ["h2", "http/1.1"]` and adding a bare
`transport_protocol: tls` chain to `egress_tcp_passthrough` — cheap, but it fails
open for TLS clients that omit ALPN.

### 5.5 SSH

Tunnelled TCP. Under sdsmint it lands in chain 3: SSH is server-speaks-first, so
`tls_inspector`/`http_inspector` time out after 1s and
`continue_on_listener_filters_timeout: true` lets the connection fall through to
the blind `tcp_proxy`. Empirically confirmed in
`docs/dev/non-http-egress-manual-test.md`:

```
{"leg":"passthrough","sni":null,"upstream":"140.82.113.3:22", ...}
[egress] authority=140.82.113.3:22 peer_san=spiffe://substrate-actor.local/atespace/demo/actor/my-sandbox-1 code=200 flags=DC
```

The GitHub host key fingerprint is unchanged, proving there is no interception.
That log line is also the clearest single piece of evidence that the CONNECT peer
is the **actor**, not the worker.

### 5.6 HTTP/1.0

Shipped: opaque. sdsmint: `http_inspector` lists `http/1.0` explicitly in chain
2's `application_protocols`, so cleartext HTTP/1.0 is parsed and forwarded by
`egress_forward_proxy_cleartext`.

### 5.7 HTTP/1.1 GET (and other ordinary methods)

Shipped: opaque — HTTPS is end-to-end between the actor and the origin.

sdsmint: chain 1 if the actor speaks TLS (MITM, requires the actor to trust the
MITM CA), chain 2 if cleartext. Chain 1 routes `application/grpc` to
`egress_forward_proxy_grpc` and everything else to `egress_forward_proxy`.

Both halves are now under automated e2e (§6): `TestActorEgressHTTPS` on the MITM
lane is the chain 1 case, and `TestActorEgressMITMTrust` is the trust
precondition on its own — an actor whose image does not carry the MITM CA fails
the handshake, which is the failure §5.4 turns on.

### 5.8 HTTP/1.1 CONNECT

A CONNECT issued *by the actor* — an actor with `HTTP_PROXY` set — is just TCP
payload: it is tunnelled inside atunnel's own CONNECT to the gateway. Shipped
config: opaque, so it works.

Under sdsmint it does **not** work. Chain 1 is excluded outright, since
`transport_protocol: tls` needs a ClientHello and a cleartext CONNECT is
`raw_buffer`; chain 1 is reachable only in the rare proxy-over-TLS case
(`HTTPS_PROXY=https://...`), where the tunnel really does open with a
ClientHello. But neither HTTP chain can route a CONNECT regardless:

* Neither HCM declares `upgrade_type: CONNECT` — chain 1 and chain 2 both list
  only `websocket` (manifest lines 279, 414).
* Both route configs match only `prefix: "/"` (lines 338-352, 445). A CONNECT
  request has no `:path`, so a prefix match cannot match it. Every CONNECT route
  in this repo uses `connect_matcher` for exactly this reason — egress listener
  A, and `buildConnectRoutes` on ingress.

One outcome is not decidable from config: if `http_inspector` does not recognise
`CONNECT` as an HTTP method, the connection carries no application protocol and
falls to chain 3, the blind passthrough — in which case the actor's CONNECT
reaches the real proxy and works. Envoy's method list appears to include CONNECT,
which would instead put it on chain 2 and fail it. Worth settling empirically with
the harness in `docs/dev/non-http-egress-manual-test.md`, which does not currently
cover a proxy-configured actor.

### 5.9 HTTP/2 GET (and other ordinary methods)

Shipped: opaque, so h2 works end to end.

sdsmint: chain 1 negotiates h2 with the actor (`alpn: [h2, http/1.1]`) but the
default upstream cluster is pinned to HTTP/1.1 — only `application/grpc` content
types route to `egress_forward_proxy_grpc` and keep h2 (the `prefix` match also
catches `application/grpc-web`, deliberately). Non-gRPC h2 is downgraded and
trailers are dropped. The pinning is deliberate: it is what lets WebSocket
upgrades survive (§5.12).

Cleartext h2c is the exception, and it takes chain 2, not chain 1:
`egress_forward_proxy_cleartext` sets `use_downstream_protocol_config`, so h2c in
is h2c out and trailers survive. `TestActorEgressGRPC` exercises exactly that —
unary, server-stream, and bidi against an in-cluster cleartext-h2 origin,
asserting the gRPC status arrives in trailers — on both gateway variants (§6).

### 5.10 HTTP/2 CONNECT

Same as §5.8 — the actor's CONNECT is opaque payload either way.

Note this concerns only CONNECT *issued by the actor*. atunnel's own client always
speaks HTTP/1.1 CONNECT to the gateway, and the gateway's listener A is
`codec_type: HTTP1` with no `allow_connect`, so h2 CONNECT is never used on the
egress leg itself.

### 5.11 HTTP/3, all forms

**Bypass, in both configurations** — and the failure mode is worse than
"unsupported". Because interception is TCP-only, an actor speaking HTTP/3 to
UDP/443 silently leaves the pod by MASQUERADE (§5.2) rather than failing closed.
No actor identity, no ext_proc, no log line.

Any actor image with a QUIC-capable client is an egress-policy hole today, and
QUIC-capable clients typically try UDP/443 *first*, falling back to TCP only if it
fails. Practical mitigations, none currently implemented: drop UDP/443 in the
postrouting chain (which forces the TCP fallback and closes the hole), or complete
the `TODO` restricting the masquerade to the cluster resolver.

### 5.12 WebSocket over HTTP/1.1

Shipped: opaque, so it works.

sdsmint: chains 1 and 2 both declare
`upgrade_configs: [{upgrade_type: websocket}]`, without which Envoy would answer
the Upgrade itself instead of proxying it. The HTTP/1.1 pinning on
`egress_forward_proxy` exists specifically so the upgrade survives to the origin,
and `stream_idle_timeout: 0s` on `mitm_http` keeps long-lived sockets open.

### 5.13 WebSocket over HTTP/2 (RFC 8441 extended CONNECT)

Shipped: opaque, works.

sdsmint: not supported, and the manifest says so in a comment — the `websocket`
upgrade config *"covers the HTTP/1.1 path only; WebSockets over h2 are RFC 8441
extended CONNECT and would additionally need
`http2_protocol_options.allow_connect`."*

### 5.14 WebSocket over HTTP/3

Bypass, for the reasons in §5.11.

---

## 6. What CI actually exercises

Most of §5 is read off the config; this is the subset a test would catch a
regression in. The egress suites run on both gateway variants — `E2E_EGRESS_MITM=1`
selects the sdsmint lane and the demo actor that trusts the MITM CA — and each
lane runs once per sandbox class (gVisor and micro-VM).

| Suite / test | Covers | Variants |
| --- | --- | --- |
| `networking` `TestActorEgress` | cleartext HTTP/1.1 egress, §5.7 | both |
| `networking` `TestActorEgressHTTPS` | TLS egress: end-to-end shipped, MITM under sdsmint, §5.7 | both |
| `networking` `TestActorEgressNonStandardPort` | egress to a non-80 port, §5.7 | both |
| `networking` `TestActorEgressGRPC` | h2c + trailers, unary/server-stream/bidi, §5.9 | both |
| `networking` `TestActorDirectAccess`, `TestActorArbitraryPortAccess` | ingress HTTP/1.1, §4.4 | n/a |
| `egressmitm` `TestActorEgressMITMTrust` | the MITM CA reaching the actor's trust store, §5.4/§5.7 | sdsmint only (skips otherwise) |
| `egressauthz` | the gateway refusing a non-actor workload and an unknown actor, E2 in §2 | passthrough lanes; the behaviour does not depend on the variant |
| `cmd/atenet/internal/sdsmint` `manifest_test.go` | the MITM CA mounting only on the `sdsmint` container | unit |

`TestActorEgressMITMTrust` also carries a system-roots negative control, which is
what makes the positive case mean something: without it, an actor that happened to
trust the origin directly would pass whether or not interception occurred.
Ordering in `pr-workflow.yaml` is load-bearing — `--experimental-use-sdsmint`
swaps the gateway cluster-wide, so it runs after the passthrough lanes, whose
`TestActorEgressHTTPS` asserts end-to-end TLS with the origin.

What has **no** automated coverage, and rests on config reading plus
`docs/dev/non-http-egress-manual-test.md`: non-HTTP TLS under MITM (§5.4), SSH
(§5.5), actor-issued CONNECT (§5.8), WebSocket in either direction (§4.9, §5.12,
§5.13), and every UDP bypass (§5.2, §5.3, §5.11). The UDP rows are the ones worth
noting — a regression there fails *open*, silently, and no test would say so.

---

## 7. Known documentation drift

Two root-level design docs disagree with the shipped code. The code is correct.

* `egress-data-path.md` uses ports 8443 / 19600 / 19601. Those come from the
  `experiment-flag-extproc` branch; the shipped manifests use 443 and 50051. Its
  filter-state object key `ate.actor` is also wrong, but only in name now: the
  key **`ate.actor.identity`** is on `main` in
  `atenet-egress-with-sdsmint.yaml` (§2.1), having landed after that doc was
  written. See `docs/dev/egress-identity-filter-state.md`, which tracks the
  implemented shape.
* `egress-authn.md` states that the CONNECT leg presents the *worker* SVID. It
  presents the **actor** certificate — see `internal/atunnel/credential.go`,
  `prepareActorEgress` in `cmd/ateom-gvisor/main.go`, and the observed
  `peer_san=spiffe://substrate-actor.local/...` in the access log quoted in §5.5.
