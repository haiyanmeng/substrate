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
| E1 | atunnel → atelet credential broker | atunnel (client) | worker podidentity SVID | atelet | `verifyClientOnSameNode` requires the worker's `PodIdentity` NodeName/NodeUID to equal atelet's own node incarnation; the handler re-reads the peer certificate before minting |
| E2 | atunnel → atelet credential broker | atelet (server) | `spiffe://cluster.local/ns/ate-system/sa/atelet` | atunnel | exact URI match **plus** matching NodeName/NodeUID |
| E3 | atunnel → egress gateway :443 | atunnel (client) | **actor cert**: `spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>` + `ActorIdentity{Atespace, ActorName, ActorUid, Purpose=atunnel}` under PEN OID `1.3.6.1.4.1.11129.2.12` | gateway Envoy, then egress ext_proc | Envoy: `require_client_certificate: true` against `/run/actor-id-ca-certs/ca.crt`. ext_proc: re-parses the chain from XFCC, re-verifies in Go, refuses `IsCA`, requires ClientAuth EKU and `Purpose == atunnel`, authorizes on **UID** and `ACTOR_STATE_RUNNING` |
| E4 | atunnel → egress gateway :443 | gateway (server) | servicedns serving cert | atunnel | `ServerName` = gateway host, trust bundle = servicedns clusterTrustBundle |
| E5 | MITM leaf (sdsmint only) | gateway (server, to the actor) | per-SNI leaf minted on demand by `sdsmintd` | the actor's own TLS stack | only succeeds if the actor image trusts the MITM CA |
| E6 | gateway → origin | gateway (client) | none (no client cert) | origin | — |
| E7 | gateway → origin (sdsmint TLS chain) | origin (server) | origin's real cert | gateway | `auto_sni` + `auto_san_validation` against `/etc/ssl/certs/ca-certificates.crt` |

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

### The ingress config at a glance

Everything `xds.go` builds, and how a request moves through it. The four socket
listeners collapse into two HCM shapes, and the CONNECT pair rejoins the other
two after reinjection.

```
                    ┌───────────────────────────────────────┐
                    │ atenet router --mode=ingress          │
                    │ ADS ──► LDS · RDS · CDS · SDS         │
                    └───────────────────┬───────────────────┘
                                        │ snapshot
════════════════════ ENVOY ═════════════▼══════════════════════════════

 downstream client
      │
      ├───────────────┬───────────────┬───────────────┐
      ▼               ▼               ▼               ▼
  :8080           :8443           :8081           :8444
  ingress_http_   ingress_https_  connect_        connect_
  listener        listener        terminate       terminate_tls
      │               │               │               │
      │        SDS serving cert       │        SDS serving cert
      │        ✗ validation_context   │        ✗ validation_context
      │        ✗ alpn_protocols       │        ✗ alpn_protocols
      │               │               │               │
      └───────┬───────┘               └───────┬───────┘
              ▼                               ▼
   buildHcm(captureAuthority        buildConnectTerminateHCM
            =true)                  codec AUTO · allow_connect
   codec AUTO · route: RDS          upgrade_configs: [CONNECT]
                                    http_filters:
                                      set_filter_state
                                        dev.ate.authority
                                      router     ← no ext_proc
                                            │
                                    connect_matcher → main_internal
                                    timeout 0s
                                            │
                                            ▼
                                    cluster main_internal (STATIC)
                                      envoy_internal_address
                                      internal_upstream(raw_buffer)
                                      passthrough_metadata: Host /
                                        …listener.original_dst
                                            │
                                            ▼
                                    listener main_internal
                                      listener_filters:[original_dst]
                                      buildHcm(captureAuthority
                                               =false)
              │                             │
              └──────────────┬──────────────┘
                             ▼
      ════════ shared tail: all four entry points ════════

  http_filters:
    1. set_filter_state dev.ate.authority = %REQ(:AUTHORITY)%
         (ingress_* only; main_internal skips it)
    2. ext_proc ─────────────────────────► cluster ate-cluster
         request_attributes:                 STATIC 127.0.0.1:50051
           filter_state['dev.ate.authority'] HTTP/2, connect 250ms
         metadata fwd + recv:                circuit breaker:
           …listener.original_dst              max_requests
         request_headers SEND, rest SKIP/NONE
       ◄── dynamic metadata {local: <workerIP>:443, port: <target>}
           after parse authority → park → resume actor
    3. router
                             │
                             ▼
  RDS substrate_routes → vhost "*" → match { prefix: "/" }
    request_headers_to_add:
      X-Ate-Target-Port: %DYNAMIC_METADATA(…original_dst:port)%
    timeout: --route-timeout   idle_timeout: routeIdleTimeout()
                             │
                             ▼
  cluster actor_original_dst
    ORIGINAL_DST · CLUSTER_PROVIDED
    lb_config.metadata_key: …original_dst → "local"
    transport_socket: upstream mTLS (podidentity)
      validate URI SAN prefix spiffe://cluster.local/
    HttpProtocolOptions: explicit HTTP/1.1    ← downgrades h2
                             │
                             ▼
                 atunnel :443 (worker pod)
```

Three things the diagram settles that the prose does not spell out:

* **The CONNECT HCM has no ext_proc filter.** `buildConnectTerminateHCM`
  (`xds.go:901`) lists exactly two: the authority capture and the router. Actor
  resolution, parking, and resume happen only on `main_internal`'s HCM, so a
  CONNECT request crosses two HCMs and reaches ext_proc once, at the second.
* **One HCM builder serves both shapes, parameterized only by
  `captureAuthority`.** `main_internal` passes `false` (`xds.go:844`) for the
  reason given in §2: the outer CONNECT already shared the right value.
* **Two different mechanisms cross the internal-listener hop.**
  `dev.ate.authority` is *filter state* (`shared_with_upstream: ONCE`); the
  ORIGINAL_DST address is *host metadata*, carried by the `internal_upstream`
  transport socket's `passthrough_metadata` (`xds.go:717-728`). `Host` kind is
  correct there precisely because an internal hop has no real
  `SO_ORIGINAL_DST`.

Omitted from the diagram: the stdout access logger on every HCM, and the OTLP
tracing provider (`otel_collector_cluster`), attached to every HCM when
`--otlp-host` is set. Neither affects the request path.

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

### The egress config at a glance

Two configurations, one contract. Both terminate the actor's CONNECT on a
listener called `egress` behind the same mTLS, and both authorize it with the
same ext_proc call. They diverge entirely in what happens to the tunnel
afterwards.

```
 ── SHIPPED: manifests/ate-install/atenet-egress.yaml ──────────────────

 actor (atunnel egress client)
      │  mTLS: presents an actor-identity client cert
      ▼
 listener egress   [::]:443, ipv4_compat: true
   filter_chain "egress"        ← the name IS the ext_proc dispatch key
     downstream TLS: servicedns leaf (watched_directory rotation)
                     require_client_certificate: true
                     trusted_ca /run/actor-id-ca-certs/ca.crt
      │
      ▼
 HCM egress_connect · codec_type HTTP1 · upgrade_configs [CONNECT]
   forward_client_cert_details SANITIZE_SET, chain: true   → XFCC
   flush_log_on_tunnel_successfully_established: true
   http_filters:
     1. ext_proc ─────────────────► cluster ext_proc_server
          request_attributes:         STATIC 127.0.0.1:50051, h2
            xds.filter_chain_name     (atenet router --mode=egress)
          failure_mode_allow: false   timeout 2s
        ◄── 403 on deny · 503 if the router is down · else continue
     2. dynamic_forward_proxy (egress_dns_cache, V4_ONLY)
     3. router
      │
      ▼
 route  connect_matcher → egress_forward_proxy
      │
      ▼
 cluster egress_forward_proxy · dynamic_forward_proxy · CLUSTER_PROVIDED
      │
      ▼
 origin IP:port     ← opaque TCP from here on. Envoy never reads a byte
                      of the tunnel, so IP:port is all that was policed.
```

```
 ── EXPERIMENTAL MITM: atenet-egress-with-sdsmint.yaml (Envoy ≥ 1.37) ──

 actor (atunnel egress client)
      │  mTLS: presents an actor-identity client cert
      ▼
 LISTENER A  egress  0.0.0.0:443
   filter_chain "egress" — downstream TLS identical to the shipped config
   HCM egress_connect · codec_type HTTP1 · upgrade_configs [CONNECT]
     1. set_filter_state ate.actor.identity = %DOWNSTREAM_PEER_URI_SAN%
          read_only · skip_if_empty · shared_with_upstream ONCE
     2. ext_proc ──► ext_proc_server        (403 deny, fails closed)
     3. set_filter_state
          envoy.network.transport_socket.original_dst_address
            = %REQ(:AUTHORITY)%   · shared_with_upstream ONCE
     4. router
   route  connect_matcher → mitm_internal · timeout 0s
      │
      ▼
 cluster mitm_internal
   envoy_internal_address: mitm_listener
   internal_upstream(raw_buffer)   ← the only carrier for both
                                     filter-state objects above
      │
      ▼
 LISTENER B  mitm_listener · internal_listener {} · no socket anywhere
   (needs bootstrap_extensions: envoy.bootstrap.internal_listener —
    without it the listener silently fails to load)
   listener_filters: tls_inspector, http_inspector
   listener_filters_timeout 1s + continue_on_listener_filters_timeout
     (a server-speaks-first client sends nothing, is tagged raw_buffer,
      and lands on chain 3 instead of deadlocking)
      │
      ├─ transport_protocol: tls ───────────────────────► CHAIN 1
      ├─ raw_buffer + application_protocols ────────────► CHAIN 2
      │    [http/1.0, http/1.1, h2c]
      └─ raw_buffer, nothing more specific matched ─────► CHAIN 3

 CHAIN 1   HCM mitm_http                            log leg=mitm
   downstream TLS: custom_tls_certificate_selector: on-demand
     └─ DELTA_GRPC SDS ──► cluster sds_mint
          unix pipe /var/run/sdsmint/sdsmint.sock, h2,
          max_requests 32768 (a cap on the LIVE SECRET SET, not a
          burst limit — Envoy holds each stream open)
        certificate_mapper: sni
          default sni-required.egress.ate.invalid (no-SNI clients)
        both stateless and stateful resumption disabled
   alpn_protocols [h2, http/1.1] · upgrade_configs [websocket]
   transport_socket_connect_timeout 5s
   #ATE_MITM_EXTPROC_FILTER · dynamic_forward_proxy · router
   routes: content-type prefix application/grpc
             → egress_forward_proxy_grpc
           else prefix "/" → egress_forward_proxy

 CHAIN 2   HCM mitm_cleartext                       log leg=cleartext
   no transport socket — the Host header is already in the clear
   upgrade_configs [websocket]
   #ATE_MITM_EXTPROC_FILTER · dynamic_forward_proxy · router
   route prefix "/" → egress_forward_proxy_cleartext

 CHAIN 3   tcp_proxy mitm_passthrough               log leg=passthrough
   no HTTP filters and no marker — an opaque stream has no request
   → egress_tcp_passthrough

 ── upstream clusters, all dialling the origin ─────────────────────────

  egress_forward_        egress_forward_    egress_forward_
  proxy                  proxy_grpc         proxy_cleartext
  dyn_fwd_proxy          dyn_fwd_proxy      dyn_fwd_proxy
  upstream TLS,          upstream TLS,      NO upstream TLS
   public roots           public roots      use_downstream_
  auto_sni +             auto_sni +          protocol_config
   auto_san               auto_san           (h1 or h2c, as sent)
  explicit HTTP/1.1      auto_config h1+h2  allow_insecure_
   (WebSockets live,      (trailers live)    cluster_options
    h2 trailers die)

  egress_tcp_passthrough · type ORIGINAL_DST · dials the
    original_dst_address filter state set back on listener A
```

Structurally, MITM adds exactly three things: a second listener reachable only
over an internal address, on-demand per-SNI certificate minting so the tunnelled
TLS can be terminated, and filter state as the only sound way to carry the
actor's identity and original destination across that hop. Everything else —
the `:443` socket, the mTLS, the CONNECT termination, the ext_proc
authorization — is the same config in both files.

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
   §2/E3. Non-CONNECT methods are rejected outright.
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

The **fix** column rates each gap. Priority is a reading of the evidence in the
sections below, not a commitment — the facts stand whether or not you agree with
the ranking.

| | Priority | | Effort |
| --- | --- | --- | --- |
| **P0** | fails open — traffic leaves with no identity, no authorization, no log | **S** | config only, plus a test; 1–3 days |
| **P1** | blocks a plausible mainstream use, or blocks defaulting sdsmint on | **M** | config plus code, or changes spanning components; 1–3 weeks |
| **P2** | real gap, but a workaround exists | **L** | a new listener stack or subsystem; 3+ weeks |
| **P3** | no known demand | | |
| **—** | no gap to close | | |

§4.10 and §5.15 cost each row against those bands, in engineer-days.

| Protocol | Ingress behaviour | Fix | § |
| --- | --- | --- | --- |
| raw TCP | not served | P3 · L | 4.1 |
| raw UDP | not served | P3 · L | 4.1 |
| TLS (non-HTTP) | not served | P3 · M | 4.1 |
| SSH | not served | P3 · L | 4.1 |
| DNS | separate service (`atenet-dns`), not an actor ingress path | — | 4.2 |
| HTTP/1.0 | served | — | 4.3 |
| HTTP/1.1 GET | served — the mainline path | — | 4.4 |
| HTTP/1.1 CONNECT | terminated + reinjected into `main_internal` | — | 4.5 |
| HTTP/2 GET | h2c prior-knowledge only (no ALPN advertised) | P1 · M | 4.6 |
| HTTP/2 CONNECT | terminated + reinjected; `:8081` only in practice | P3 · S | 4.7 |
| HTTP/3 GET | **unsupported** | P3 · L | 4.8 |
| HTTP/3 CONNECT | **unsupported** | P3 · L | 4.8 |
| WebSocket over HTTP/1.1 | **rejected** — no `websocket` upgrade config | P1 · S | 4.9 |
| WebSocket over HTTP/2 | **rejected** | P2 · M | 4.9 |
| WebSocket over HTTP/3 | **unsupported** | P3 · L | 4.9 |

Nothing on ingress fails open — every gap here is a client that cannot connect or
cannot upgrade, which is why the column tops out at P1. Two of them are cheap and
worth doing: WebSocket over HTTP/1.1 is one `upgrade_configs` line against an
upstream already pinned to HTTP/1.1, and it currently fails *silently* (§4.9). The
ALPN gap is the more consequential one — without `alpn_protocols` on `:8443` there
is no h2 over TLS at all, so gRPC cannot reach an actor over the TLS listener.
Advertising ALPN is one line; carrying h2 the rest of the way is not, since both
the ORIGINAL_DST cluster and atunnel's reverse proxy are HTTP/1.1 (§4.6). The four
non-HTTP rows share a deeper blocker: actor resolution keys on `:authority`, and a
raw TCP or SSH stream carries no name to resolve. TLS is the exception, since SNI
would serve.

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

### 4.10 What each ingress fix costs

Engineer-days for one person already fluent in this repo, covering config, code,
unit test, e2e, and review — excluding design approval and rollout. Ranges are
wide exactly where an empirical question is unsettled, and those are named below.

Three properties of the current code set the floor for every row.

**Every ingress dataplane change lands twice.** Envoy is programmed from
`xds.go`; agentgateway is *statically* configured (`config.go:44-50`) from
`manifests/ate-install/components/agentgateway/configmap.yaml`, which hand-rolls
the same four binds in a different schema. A one-line Envoy change is one line,
plus a YAML edit, plus a second CI lane — or a deliberate decision to leave the
agentgateway lane behind while it stays opt-in behind `--atenet-router=agentgateway`.
**Every number below assumes Envoy only; add 30–50% to carry agentgateway.**

**Actor resolution needs a name, and only the addressing problem is hard.**
`parseActorRef` (`ingress.go:202`) reads the name out of `:authority`, and
resume-and-park hangs off `HandleRequestHeaders` — an ext_proc *HTTP* filter
callback, with the parking lot (`h.parking.enter`, `ingress.go:124`) holding the
request open while the worker pool drains. None of that survives a protocol with
no request. The *plumbing* to replace it is nevertheless mostly off-the-shelf;
see "the L4 path is cheaper than it looks" below. What is not off-the-shelf is
deciding how a nameless stream identifies its actor, and that is what keeps the
`L` on §4.1.

**The last two hops are HTTP/1.1 by construction.** `buildOriginalDstCluster`
pins `Http1ProtocolOptions` and says why: *"The atunnel ingress server terminates
TLS and reverse-proxies to the actor over HTTP/1.1"* (`xds.go:760-776`). atunnel
is an `httputil.ReverseProxy` over `http.Server` (`internal/atunnel/ingress.go:130`).
Any row needing something other than h1 at the actor pays for four hops, not one.

**Nothing tests the TLS listeners, and no certificate matches the name clients
use.** `RouterClient` port-forwards and speaks plaintext; no e2e touches `:8443`
or `:8444` at all (§6 lists only the two plaintext ingress tests). Worse, the
servicedns signer issues SANs of exactly one shape — `<svc>.<ns>.svc`
(`servicednssigner.go:130-138`) — so the router serves a certificate for
`atenet-router.ate-system.svc`, while CoreDNS points
`<actor>.<atespace>.actors.resources.substrate.ate.dev` at that same router
(§4.2). A verifying TLS client that addresses an actor by name gets a hostname
mismatch. **Every TLS-terminating row inherits an unpriced prerequisite here**,
and §2's I1 ("ordinary WebPKI/cluster trust") glosses over it.

**The e2e harness cannot carry UDP.** It reaches the router through
`kubectl port-forward` (`internal/portforward`, `router_client.go:50-52`), which
is TCP-only. Any QUIC row needs a new way into the cluster — NodePort, hostPort,
or an in-cluster driver pod — before it needs a QUIC client. This is an *ingress*
constraint only: egress tests dial outward from an actor that is already inside
the cluster.

| Row | § | Matrix | Standalone | Incremental | What dominates |
| --- | --- | --- | --- | --- | --- |
| raw TCP | 4.1 | L | 2–3 wk | — | addressing design; whether the L4 filter fires before client data |
| SSH | 4.1 | L | 2–3 wk | +0 d, or blocked | server-speaks-first — it hits the trigger question head-on |
| TLS (non-HTTP) | 4.1 | M | 1.5–2 wk **+ certs** | +2–4 d after raw TCP | SNI supplies the name *and* the trigger — but see the cert gap |
| raw UDP | 4.1 | L | 5–7 wk | +2–3 wk after raw TCP | datagram transport router→worker; no UDP anywhere today |
| HTTP/2 GET — ALPN negotiated | 4.6 | M | 2–3 d | — | one line; proving it needs a TLS client the harness lacks |
| HTTP/2 GET — h2 to the actor | 4.6 | M | 2–3 wk | +2–3 wk after ALPN | four hops to convert; h2c server in the counter demo |
| HTTP/2 CONNECT | 4.7 | S | 0.5 d | +0 d with ALPN | the same shared line; `allow_connect` already set |
| HTTP/3 GET | 4.8 | L | 3–5 wk | — | the harness and the Service, not the Envoy config |
| HTTP/3 CONNECT | 4.8 | L | — | +3–5 d after h3 GET | routing only |
| WebSocket over HTTP/1.1 | 4.9 | S | 2–3 d | — | verifying ReverseProxy's 101 path; a WS endpoint in the demo |
| WebSocket over HTTP/2 | 4.9 | M | 1.5–2 wk | +3–5 d after ALPN + WS/h1 | `allow_connect` on `buildHcm`; extended CONNECT → h1 Upgrade |
| WebSocket over HTTP/3 | 4.9 | L | — | +2–3 d after h3 | nothing new |

**The ALPN line is shared, so §4.6 and §4.7 are one edit.**
`buildDownstreamTlsTransportSocket` (`xds.go:1167`) — "shared by every
TLS-terminating listener", per its own comment — is the transport socket for both
`buildHttpsListener` (`:8443`) and `buildConnectTerminateTLSListener` (`:8444`).
Adding `alpn_protocols` there closes §4.6's negotiation gap and §4.7's in the same
line; they cannot be scheduled apart without splitting the function.
`allow_connect: true` is already set on the CONNECT HCM (`xds.go:901`), so §4.7
needs no further code at all — its incremental cost is a test.

**§4.6 is two different projects, and the matrix's single `M` conflates them.**
Advertising ALPN (2–3 d) gets h2 negotiated and downgraded to h1 at the cluster —
but what that buys is bounded by the cert gap above: it can be *proved* only with
verification disabled, because no certificate the router holds matches the name
an h2 client would dial. Carrying h2 to the actor — what gRPC ingress needs —
converts four hops: the ORIGINAL_DST cluster's protocol options, the
router→atunnel ALPN, `Serve`'s TLS config (`ServeConnect` already sets
`NextProtos: [h2, http/1.1]` at `ingress.go:209`; `Serve` does not), and the
ReverseProxy transport — plus an h2c server in the counter demo to test against.

**`S` is days, not hours, because of the test.** §4.9's config change really is
one `UpgradeConfigs` entry on `buildHcm`, which today sets neither
`UpgradeConfigs` nor `Http2ProtocolOptions`. The cost sits elsewhere: Go's
`ReverseProxy` has handled 101 upgrades since Go 1.12, but this one carries a
custom `Rewrite` (`ingress.go:131`) and has never been exercised against an
upgrade; the counter demo has no WebSocket endpoint; and `networking` runs once
per sandbox class. Budget a day for that verification to come back negative and
turn a one-line fix into an atunnel change.

**The L4 path is cheaper than it looks, because three pieces already line up.**
Envoy has a *network* ext_proc filter (`envoy.filters.network.ext_proc`), and its
config is field-for-field the shape `buildHcm` already emits for the HTTP one —
`failure_mode_allow`, `message_timeout`, `metadata_options`, plus
`connection_attributes` where the HTTP filter has `request_attributes`. Its
`ProcessingResponse.dynamic_metadata` is settable by the server, which is exactly
how the current handler steers the ORIGINAL_DST cluster. `tcp_proxy` then carries
the stream with `tunneling_config`, whose own proto documentation gives
`hostname: "%DYNAMIC_METADATA(tunnel:address)%"` as the worked example — so the
router can wrap a raw stream in a CONNECT aimed wherever resolution just pointed.
And the far end of that CONNECT already exists: `ServeConnect`
(`internal/atunnel/ingress.go:204`, `DefaultConnectPort = 444`) hijacks and
relays raw bytes with half-close, is provisioned by `workerpool_apply.go`, and is
unused today only because the dataplane hardcodes `:443` (§4.5). The raw relay to
the actor is built and deployed; it is not on the critical path. What remains is
a sibling of `ingress.Handler` reusing `ActorResumer` and the parking lot
unchanged, with `message_timeout` covering the parking budget the way
`SetExtProcMessageTimeout` does now. Neither `tcp_proxy` nor `network_ext_proc`
is vendored — `vendor/.../filters/network/` holds only
`http_connection_manager` — but that is a `go mod vendor` chore, not a design
one. Call the plumbing 1–1.5 weeks.

**What is left is one design question and one empirical one, and they are the
estimate.** *Addressing:* a raw stream carries no name, and port-per-actor — the
only scheme that works without one — means a Kubernetes Service enumerating a
port per actor, which is a reconciliation and scaling problem rather than a
coding one. Pre-declaring a fixed port range dodges it, at the cost of a ceiling
on concurrent raw-TCP actors. *The trigger:* the L4 filter processes **data**, so
a client that sends nothing until greeted never produces a `read_data` message
and its actor is never resumed. Whether the filter also fires on connection
establishment decides whether SSH works at all. That is the ingress twin of
§5.8's `http_inspector` question and deserves the same half day against a scratch
Envoy before anyone commits to a number.

**So §4.1 is two rows, not three.** raw TCP and SSH stand or fall together on the
trigger question — SSH is the server-speaks-first case in its purest form, so it
is free once raw TCP lands or it is blocked outright, with nothing in between.
Non-HTTP TLS escapes both problems: SNI is a name and a ClientHello is
client-first data, so it needs the shared L4 plumbing and neither the addressing
scheme nor a favourable trigger answer. That makes it the one row in §4.1 worth
costing on its own, and the cheapest way to put the L4 path into production at
all.

**The TLS cert gap is a project nobody has scoped, and two rows depend on it.**
Non-HTTP TLS ingress has to choose: pass the TLS through to the actor, so the
*actor* needs a serving certificate for its own DNS name and nothing provisions
one; or terminate at the router, so the router needs SANs covering actor names
and the signer emits `<svc>.<ns>.svc` only. Either branch is a
certificate-provisioning change outside this matrix, which is why that row
carries **+ certs** rather than a number. §4.6's ALPN row escapes it only because
ALPN negotiation can be demonstrated with verification off.

**HTTP/3's cost is the harness, not Envoy.** Downstream QUIC is mature in Envoy:
a `udp_listener_config`, `quic_options`, a QUIC transport socket and
`CodecType: HTTP3` in `xds.go` is roughly a week, and the certificate already
arrives over SDS. The rest is everything around it — a second Service port (all
four at `atenet-router.yaml:363` are `protocol: TCP`), whatever fronts it in each
environment, and a way for the suite to send UDP at all, which port-forward
cannot. That reordering is why this is 3–5 weeks rather than the 5–7 a "no UDP
anywhere" reading suggests. It still downgrades to h1 upstream, so §4.6 remains a
prerequisite for it to be useful past the router.

**Raw UDP ingress keeps 5–7 weeks, for a reason HTTP/3 does not share.** An Envoy
UDP listener does not solve the router→worker hop: that leg is mTLS TCP with no
datagram framing, and unlike HTTP/3 — which terminates at the router and
continues upstream as HTTP — there is no CONNECT-shaped tunnel to borrow.

Read down the *incremental* column and the ingress backlog is smaller than the
matrix suggests. Both P1s and the lone S come to a little over one engineer-week
combined — ALPN, WebSocket over HTTP/1.1, and h2 CONNECT arriving free with the
first. Of what remains, only raw UDP at 5–7 weeks is genuinely a subsystem; the
L4 story is a week of plumbing wrapped in two unanswered questions, and HTTP/3 is
mostly harness work.

The caveat that outranks all of them: **the TLS listeners are untested and serve
a name no client uses.** That is not on the matrix, because it is not a protocol
gap — but it caps what §4.6 and the TLS row in §4.1 can actually deliver, and it
should be settled before either is scheduled.

---

## 5. Per-protocol matrix: egress

Outbound from an actor. Identities on every hop are E1–E7 in §2.

**tunnelled** = TCP, REDIRECTed to atunnel, carried inside CONNECT with the actor
certificate, authorized by ext_proc. **bypass** = leaves the worker pod by
MASQUERADE with no identity, no authorization, and no log.

Priority and effort codes are as in §4.

| Protocol | Shipped (`atenet-egress.yaml`) | sdsmint (experimental) | Fix | § |
| --- | --- | --- | --- | --- |
| raw TCP | tunnelled, opaque | tunnelled → chain 3 passthrough | — | 5.1 |
| raw UDP | **bypass** | **bypass** | **P0** · M | 5.2 |
| DNS | **bypass** (UDP) / tunnelled (TCP) | same | P1 · M | 5.3 |
| TLS (non-HTTP) | tunnelled, opaque | tunnelled → chain 1, MITM attempted, **breaks** | P1 · S | 5.4 |
| SSH | tunnelled, opaque | tunnelled → chain 3 passthrough | — | 5.5 |
| HTTP/1.0 | tunnelled, opaque | chain 2 cleartext | — | 5.6 |
| HTTP/1.1 GET | tunnelled, opaque | chain 1 (TLS) or chain 2 (cleartext) | — | 5.7 |
| HTTP/1.1 CONNECT | tunnelled opaquely, works | **broken** — no chain routes CONNECT | P2 · M | 5.8 |
| HTTP/2 GET | tunnelled, opaque | chain 1, downgraded to HTTP/1.1 unless gRPC | P2 · M | 5.9 |
| HTTP/2 CONNECT | as HTTP/1.1 CONNECT | as HTTP/1.1 CONNECT | P2 · with 5.8 | 5.10 |
| HTTP/3 GET | **bypass** (UDP) | **bypass** | **P0** · S | 5.11 |
| HTTP/3 CONNECT | **bypass** | **bypass** | **P0** · with 5.11 | 5.11 |
| WebSocket over HTTP/1.1 | tunnelled, opaque (works) | chains 1 and 2, `upgrade_configs: [websocket]` | — | 5.12 |
| WebSocket over HTTP/2 | tunnelled, opaque (works) | not supported | P3 · S | 5.13 |
| WebSocket over HTTP/3 | **bypass** | **bypass** | **P0** · with 5.11 | 5.14 |

Every P0 in this document is on egress, and all of them are the same bug seen from
different angles: interception is TCP-only, so UDP leaves by MASQUERADE with no
identity, no ext_proc call, and no log line. **HTTP/3 is the one to fix first** —
not because it is the largest hole but because it is the one a normal actor image
walks into by accident, since QUIC-capable clients try UDP/443 before falling back
to TCP. Dropping UDP/443 in the postrouting chain forces that fallback and closes
it in a few lines. The general UDP case (§5.2) is larger only because DNS has to
keep working through whatever closes it, which is what makes §5.3 a prerequisite
rather than a gap of its own.

Below P0, the ordering is about unblocking sdsmint: non-HTTP TLS (§5.4) is the one
row where enabling the MITM gateway actively breaks traffic that works today, and
the fix is cheap — though, as that section notes, it fails open for TLS clients
that omit ALPN, so it trades a correctness bug for a policy one. Actor-issued
CONNECT (§5.8) is rated M less for the config than for the open empirical question
in front of it: whether `http_inspector` claims a cleartext CONNECT decides whether
the connection lands on chain 2 and fails or on chain 3 and works, and no test
settles it.

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

Identities: E3 (actor cert) and E4 (gateway serving cert). Envoy terminates the
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

### 5.15 What each egress fix costs

Same basis as §4.10 — engineer-days for one person fluent in this repo, covering
config, code, unit test, e2e, and review; excluding design approval and rollout.

Seven properties of the egress path set the floor here, and they are not the
ingress ones.

**The egress config exists in three hand-maintained copies.**
`atenet-egress.yaml` (393 lines) and `atenet-egress-with-sdsmint.yaml` (1023) are
*independent full files* selected by `atenet_egress_manifest()`
(`hack/install-ate.sh:241`), not a base and an overlay; `agentgateway-egress/`
layers the shared `components/agentgateway` over the first. Listener A, the
downstream mTLS, the CONNECT termination and the ext_proc filter are duplicated
verbatim between the two Envoy files. §7 already records a bug caused by exactly
this — the `ipv4_compat` divergence. Assume any change to the shared part is
written two or three times.

**Three Envoy versions are deployed.** Shipped egress is
`envoyproxy/envoy:v1.34-latest`, sdsmint egress `v1.37-latest`, the router
`v1.39-latest`. The shipped egress gateway is the oldest component in the
deployment, so any fix that needs a post-1.34 feature on the passthrough lane
costs a version bump and a re-qualification before it costs anything else.

**Egress e2e is four lanes, not one.** `pr-workflow.yaml` runs `networking` four
times — passthrough and MITM, each on gVisor and micro-VM — and `egressmitm`
twice. Every new egress assertion is paid for four times in wall-clock and flake
surface, and the lane ordering is load-bearing (§6).

**The nftables rows land once, not once per sandbox class.** The four-lane CI
matrix invites a ×2 for gVisor and micro-VM, and there isn't one: both classes
call the same `ateomnet.SetupActorNetwork`, from `cmd/ateom-gvisor/main.go:625`
and `:894` and from `cmd/ateom-microvm/run.go:401` and `restore.go:277`, and it
calls `InstallActorNftablesRules` at `net.go:571`. One edit, four lanes of
verification. Nor does the DNS carve-out need a new flag threaded through those
four call sites: the worker pod's `/etc/resolv.conf` holds the cluster resolver
and is already read in-process by `writeGuestResolvConf` (`run.go:228`).

**For the UDP rows the test is the deliverable.** The `forward` chain is already
there with policy accept (`net.go:280-295`), so a drop is a rule, not a chain,
and the demo actor already has the shape a UDP probe needs — `/grpc` sits beside
`/` in the same mux (`demos/egress/main.go:152`), so a `/udp` endpoint is a
handler, not a fixture. Two things are genuinely missing rather than cheap: there
is no UDP or destination-port matcher to compose against (`TCPProtocol` at
`net.go:346` is the only L4 helper, and nothing reaches into the transport
header), and the forward chain's unconditional accept (`net.go:289-295`) has to
be inserted around rather than appended to. Neither is hard. What the estimate
actually buys is a fixture that proves a packet *did not* leave, because a
regression here fails open and silently and no existing test would say so.

**Four of the non-UDP rows are sdsmint-only, which relaxes the first three
floors.** §5.4, §5.8/§5.10, §5.9 and §5.13 all read "shipped: opaque, works" and
fail only under interception. Their fix therefore lands in
`atenet-egress-with-sdsmint.yaml` alone — one file, not three — is qualified
against Envoy v1.37 rather than the shipped v1.34, and is exercised by the
`E2E_EGRESS_MITM=1` lanes rather than all four. The duplication and version
floors above bite the UDP rows and the shared listener, not these. What they buy
in exchange is bounded: they matter exactly as much as defaulting sdsmint on
does.

**Egress does not inherit the ingress harness's UDP problem.** §4.10 prices raw
UDP ingress partly for lack of any way to get a datagram to the cluster —
`internal/portforward` is TCP-only. Egress never needs one: the suite drives the
actor over ordinary HTTP through the router (`postThroughEgressActor`) and the
actor originates the datagram from inside the cluster itself. The assertion is a
JSON field in the actor's reply, plus a gateway access log. That asymmetry is why
UDP costs days here and weeks there.

#### Closing a hole is not the same project as policing it

Every P0 admits two fixes, and the matrix prices only one of them. **Closing**
means making UDP fail shut — reject the datagrams and let the actor fall back to
TCP, which the tunnel already polices. **Policing** means carrying UDP through
the gateway with an actor identity attached. Closing is days. Policing is over a
month. The P0s are P0 because traffic escapes unlogged, and closing removes that
entirely, so the table prices both.

| Row | § | Matrix | Standalone | Incremental | What dominates |
| --- | --- | --- | --- | --- | --- |
| raw UDP — close | 5.2 | M | 4–5 d | — | the DNS carve-out, and an e2e that proves a drop |
| raw UDP — police | 5.2 | M | 5–7 wk | +4–6 wk after close | CONNECT-UDP end to end; a new ext_proc shape |
| DNS | 5.3 | M | 2–4 d | +0 d with UDP-close | pinning the masquerade to the cluster resolver — the same rule |
| HTTP/3 GET — close | 5.11 | S | 2–3 d | — | `reject`, not `drop`, so the client fails over at once |
| HTTP/3 CONNECT, WS over h3 | 5.11, 5.14 | S | — | +0 d | the same rule; nothing protocol-specific |
| HTTP/3 — police | 5.11 | — | 6–8 wk | +1–2 wk after UDP-police | QUIC on the gateway; no demand |
| TLS (non-HTTP) | 5.4 | S | 2–3 d | — | an in-cluster non-HTTP TLS origin to test against |
| HTTP/1.1 and h2 CONNECT | 5.8, 5.10 | M | 4–6 d | — | 0.5 d to settle `http_inspector` first; the fix branches on the answer |
| HTTP/2 GET | 5.9 | M | 1–1.5 wk | — | un-pinning chain 1 without breaking WebSockets |
| WebSocket over HTTP/2 | 5.13 | S | 1–2 d | — | one `allow_connect` on `mitm_http` |
| raw TCP, SSH, HTTP/1.0, HTTP/1.1 GET, WS over h1 | 5.1, 5.5–5.7, 5.12 | — | 0 | — | no gap |

**§5.3 has no independent cost.** The DNS row and the UDP-close row are one
nftables edit seen twice: narrowing the masquerade to the cluster resolver is
precisely what makes dropping everything else safe. §5's preamble already calls
§5.3 a prerequisite rather than a gap; the corollary is that it disappears into
§5.2's close. Its `M` belongs to the policing project, not to the row.

**§5.11's `S` is real, and it is the best-value row in this document.** The one
design choice worth making deliberately: `drop` leaves a QUIC client waiting out
its own timeout before racing TCP, while `reject with icmp port-unreachable`
makes the fallback immediate. Same rule count either way, and `expr.Reject` is in
the nftables library already; the `forward` chain is `ChainTypeFilter` on
`ChainHookForward` (`net.go:281-288`), which is one of the hooks reject is legal
in — it would not be from either NAT chain. Standalone the row needs a UDP
destination-port matcher that does not exist yet; done as part of the UDP close
it needs nothing at all, because a blanket non-DNS UDP drop already covers 443.

**§5.4's `S` survives, and the origin is cheaper than it was.** The config is two
chain edits in one manifest — qualify chain 1 with `application_protocols`, add a
bare `transport_protocol: tls` chain to `egress_tcp_passthrough`. Nothing in CI
speaks non-HTTP TLS today (§6 lists it under no automated coverage, resting on
the manual harness), so the balance of the estimate is standing up an origin —
but `e2e.DeployServerPod` now renders any `--listen=:<port>` binary into a Pod
and a Service from one shared template, so that is a small `main` and a struct
literal. The one snag: the template's readiness probe is HTTP GET or gRPC only
(`serverpod.go:144-153`), and a raw TLS listener answers neither, so it wants a
`tcpSocket` variant — an hour in `serverReadinessProbe`, not a new fixture. The
row also trades a correctness bug for a policy one, as §5.4 says: no-ALPN TLS
clients then pass through unexamined.

**§5.8's `M` is mostly the unknown.** Half a day with
`docs/dev/non-http-egress-manual-test.md` decides whether there is anything to
fix at all — if `http_inspector` does not claim `CONNECT`, the connection lands
on chain 3 and already works. The 4–6 d assumes the unfavourable answer, and
assumes the sdsmint scoping above: one manifest, and the two MITM lanes. Settle
the question before scheduling the row — the favourable answer takes it to zero.

**§5.9 is not the cluster swap it looks like.** Putting `auto_config` or
`use_downstream_protocol_config` on `egress_forward_proxy` restores h2 and
trailers and breaks every WebSocket (§5.12) — the HTTP/1.1 pin is load-bearing,
deliberately. The fix is a route-level split on the `upgrade` header so the two
cases reach different clusters, which makes the estimate testing rather than
config.

**Policing UDP is an `L`, not an `M`.** RFC 9298 CONNECT-UDP needs h2 extended
CONNECT, and listener A is `codec_type: HTTP1` with no `allow_connect`. The
destination moves out of `:authority` and into the request path, so
`HandleRequestHeaders` changes shape — it rejects non-CONNECT outright
(`egress.go:97`) and reads the destination from `:authority` (`egress.go:131`).
atunnel gains a UDP listener and per-flow demux where today `validateDestination`
requires an IP literal for a TCP dial (`client.go:208-221`). The egress Service
publishes 443/TCP only (`atenet-egress.yaml:391-393`). That is a subsystem.

The ordering these numbers suggest is not the one the matrix implies. Every P0
collapses into **under one engineer-week total** if the goal is to stop traffic
escaping unpoliced: reject UDP/443, pin the masquerade to the cluster resolver,
drop the rest. That is one change to one function, and it closes §5.2, §5.3,
§5.11 and §5.14 together — converting the only fail-open rows in this document
into fail-closed ones. The 5–7 week policing project buys UDP *support*, which
nothing currently demands; it should not be what keeps the P0s open.

Everything left after that week is sdsmint-only, and none of it is fail-open. So
the egress backlog is really two decisions, not eleven rows: whether to close UDP
now (a week, and the answer is obviously yes), and whether sdsmint is going to
default on (which, if yes, makes §5.4, §5.8, §5.9 and §5.13 a further 2.5–4
engineer-weeks, and if no, makes them documentation).

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
| `egressauthz` | the gateway refusing a non-actor workload and an unknown actor, E3 in §2 | passthrough lanes; the behaviour does not depend on the variant |
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

One drift between the two manifests rather than between doc and code: the
shipped `atenet-egress.yaml` binds both `:443` and the admin `:15000` to `::`
with `ipv4_compat: true`, and comments that this is load-bearing for the kubelet
dialling the pod IP. `atenet-egress-with-sdsmint.yaml` still binds both to
`0.0.0.0`, so it works only on an IPv4 cluster. Harmless today — the MITM
variant is experimental and CI runs IPv4 — but the two files should converge.
