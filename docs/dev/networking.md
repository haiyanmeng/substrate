# Networking in Agent Substrate

An orientation guide for someone new to the project. It covers the whole path a
byte takes — client to actor and back out again — the concepts from Envoy,
Linux, and Kubernetes you need to follow that path, and where in the tree each
piece lives.

For vocabulary (Actor, Atespace, Worker, atelet, ateom, atenet, atunnel) read
[glossary.md](../glossary.md) first. For the credential story in depth, read
[actor-identity.md](../actor-identity.md).

---

## 1. The shape of the problem

Substrate maps a large set of **actors** onto a small set of ready **worker
pods**. An actor is a virtual thing: it has a stable name, but at any moment it
is either hibernated (running nowhere) or activated on some worker pod, and it
migrates between workers over its lifetime.

That single fact drives every networking decision in the project:

- **You cannot use a Kubernetes Service per actor.** There are far more actors
  than Services can scale to, and an actor usually has no pod at all.
- **Routing has to be resolved per request, at request time**, because the
  answer changes — and because the act of routing is what *wakes the actor up*.
- **Nothing can trust the pod's identity to mean the actor's identity**, because
  a worker pod hosts many actors over its life.

So: one gateway address for every actor, a header that names which actor a
request is for, a gateway that calls out to the control plane on every request
to find (or create) the actor's current worker, and a per-actor credential that
is minted fresh on each activation.

---

## 2. The whole path, end to end

```
                    ┌─────────────────────────────────────────┐
   in-cluster       │  atenet-router pod        (ate-system)  │
   client   ──①──▶  │  ┌────────┐ ─②─▶ ┌──────────────────┐  │
                    │  │ Envoy  │ ◀─── │ atenet router    │  │
                    │  │        │      │  xDS + ext_proc  │──┼──③──▶ ateapi
                    │  └───┬────┘      └──────────────────┘  │      (ResumeActor)
                    └──────┼──────────────────────────────────┘
                           │ ④ mTLS, dst = worker IP :443
                           ▼
                    ┌─────────────────────────────────────────┐
                    │  worker pod                             │
                    │   ┌──────────────────┐                  │
                    │   │ atunnel Server   │ ⑤                │
                    │   └────────┬─────────┘                  │
                    │            │ plain HTTP over veth       │
                    │   ╔════════▼═════════╗                  │
                    │   ║ sandbox (gVisor  ║                  │
                    │   ║  or micro-VM)    ║                  │
                    │   ║   actor app      ║                  │
                    │   ╚════════╤═════════╝                  │
                    │            │ ⑥ outbound TCP             │
                    │   nftables REDIRECT → :15001            │
                    │   ┌────────▼─────────┐                  │
                    │   │ atunnel Egress   │                  │
                    │   └────────┬─────────┘                  │
                    └────────────┼────────────────────────────┘
                                 │ ⑦ mTLS + HTTP CONNECT
                                 ▼
                    ┌─────────────────────────────────────────┐
                    │  atenet-egress pod        (ate-system)  │
                    │  ┌────────┐ ──▶ ┌──────────────────┐    │
                    │  │ Envoy  │     │ atenet --mode=   │────┼──▶ ateapi
                    │  └───┬────┘     │       egress     │    │   (verify actor)
                    └──────┼──────────└──────────────────┘────┘
                           │ ⑧
                           ▼  the internet
```

| # | Hop | Section |
|---|---|---|
| ① | Client connects to the ingress gateway, naming the actor in a header | [§3](#3-naming-and-addressing), [§4](#4-the-ingress-gateway) |
| ② | Envoy asks the ext_proc server where to send this request | [§4](#4-the-ingress-gateway) |
| ③ | ext_proc resumes the actor through ateapi, gets a worker IP | [§4](#4-the-ingress-gateway) |
| ④ | Envoy dials that worker directly over mTLS | [§5](#5-gateway-to-worker) |
| ⑤ | atunnel checks the actor is the one active here, proxies in | [§6](#6-inside-the-worker-pod-atunnel-ingress) |
| ⑥ | The actor's outbound TCP is transparently intercepted | [§7](#7-actor-egress-nftables-and-the-tunnel) |
| ⑦ | atunnel opens a CONNECT tunnel to the egress gateway | [§7](#7-actor-egress-nftables-and-the-tunnel) |
| ⑧ | The gateway authenticates the actor, applies policy, forwards | [§8](#8-the-egress-gateway) |

---

## 3. Naming and addressing

An actor is named by a **request header**, not by the URL:

```
ate-target-actor: <atespace>/<actor>
```

`TargetActorHeader` and its parser `ParseTargetActor` live in
`internal/atenet/headers.go:29`. The **atespace is part of the reference**
because an actor name is only unique within its atespace. An atespace is a
global-scoped Substrate isolation boundary — *not* a Kubernetes namespace.

Naming the actor out-of-band from `:authority`/`Host` is what lets **one gateway
address serve every actor**. The client connects to the ingress gateway by
whatever address it is published on, and the header decides which actor the
request is for. Two consequences worth internalizing:

- **Nothing resolves per actor.** There is no DNS record, Service, or endpoint
  per actor, so nothing has to be created when an actor is created or moved.
  Migration is invisible to the client because the address never changes.
- **A nonexistent actor is a 404 from the gateway**, discovered at request time.
  Addressing carries no existence information.

> **This replaced a per-actor DNS scheme.** Actors used to have a name of the
> form `<actor>.<atespace>.actors.resources.substrate.ate.dev`, resolved by a
> CoreDNS Corefile that `atenet dns` generated with a single wildcard template.
> That subcommand, the `ActorDNSSuffix`/`ActorDNSName`/`ParseActorDNSName`
> helpers, and the Corefile generator are all gone. If you find a doc, comment,
> or diagram describing actor DNS names, it predates this change.

The port on the actor is addressed separately — see `connect_terminate` in
[§4](#4-the-ingress-gateway).

---

## 4. The ingress gateway

Deployment `atenet-router` in `ate-system`
(`manifests/ate-install/atenet-router.yaml`). Two containers:

| Container | Role |
|---|---|
| `atenet-router` | xDS server (18000), ext_proc gRPC server (50051), status (4040), metrics (9090) |
| `envoy` | The dataplane: 8080 http, 8443 https, 8081 connect, 8444 connect-tls, 9901 admin |

Service `atenet-router` (ClusterIP) publishes 80, 443, 8081, 8444, 4040
(`atenet-router.yaml:327`).

The Envoy container boots from a small static ConfigMap that does nothing but
point at the sidecar's xDS server; **everything real is served dynamically** by
`cmd/atenet/internal/router/xds.go`.

### The listeners

| Listener (`xds.go:78-109`) | Purpose |
|---|---|
| `ingress_http_listener` | Plain HTTP workload traffic |
| `ingress_https_listener` | TLS-terminating workload traffic |
| `connect_terminate` | Terminates client `CONNECT <host>:<port>` (`xds.go:1261`) |
| `connect_terminate_tls` | Same, over TLS (`xds.go:1295`) |
| `main_internal` | Envoy **internal listener** — no socket. CONNECT-tunneled bytes are reinjected here |

`connect_terminate` exists to solve arbitrary-port ingress. The actor is named
by `ate-target-actor`, but that leaves nowhere to say "and I want port 9000 on
that actor." A client instead sends a `CONNECT` naming the port in the
authority; Envoy terminates the tunnel, captures the outer authority and the
target actor into filter state, and pushes the tunneled bytes into
`main_internal`, which runs the *same* ext_proc path as ordinary traffic.

The filter-state capture is load-bearing: the internal-listener hop is a new
transaction, so the original headers do not survive it. Each request inside a
long-lived tunnel resumes the actor and re-routes independently — if the actor
migrates mid-tunnel, the next request follows it.

### ext_proc: the routing decision

[**Envoy ext_proc**](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
is an HTTP filter that opens a bidirectional gRPC stream to an external service
and lets it inspect and mutate the request mid-flight. Substrate uses it as the
place where "which pod?" gets answered, because the answer requires an
authenticated control-plane call and possibly *starting a workload*.

`ingress.Handler.HandleRequestHeaders`
(`cmd/atenet/internal/router/ingress/ingress.go:78`) is the whole ingress
decision. In order:

1. **Read the actor reference** (`ingress.go:89`). `routingValue` checks the
   `ate-target-actor` header first, then falls back to the
   `dev.ate.target.actor` filter-state attribute — because after a CONNECT hop
   the header no longer exists on the inner request. A reference that does not
   parse is a **404**, not a 400.
2. **Pick the target port** (`ingress.go:97-108`). 80 unless a CONNECT authority
   names another. For CONNECT the authority comes from filter state, because by
   then `md.Host` belongs to the inner request.
3. **Enter the parking lot** (`ingress.go:113`). If the worker pool is
   momentarily saturated, the request *waits* here rather than failing — see
   [request-parking.md](../request-parking.md). A full lot sheds immediately, so
   the gateway applies backpressure instead of queueing without bound.
4. **`ResumeActor`** against ateapi. This is the step that wakes a hibernated
   actor, and it returns the worker pod IP it landed on. A non-IP value is a 500.
5. **Publish the destination in dynamic metadata** — `{local: "<workerIP>:443",
   port: "<actorPort>"}` under `envoy.filters.listener.original_dst`
   (`ingress.go:152`).
6. **Overwrite `ate-target-actor`** with the resolved reference
   (`ingress.go:163`), *"so a client-provided value cannot select a different
   actor after this request has been resolved."* atunnel re-reads the same
   header at the far end, so the value it authorizes against has to be the one
   the router resolved.
7. **Leave `Host`/`:authority` alone.** Actor addressing does not live there, so
   the client's `Host` is passed through to the actor untouched.

The actor's default port is 80 (`ingress.go:47`).

`X-Ate-Target-Port` is *not* set here. atunnel is a separate process and cannot
read Envoy dynamic metadata, so the **route** materializes the port back into a
header (`xds.go:838-847`):

```
X-Ate-Target-Port: %DYNAMIC_METADATA(envoy.filters.listener.original_dst:port)%
```

### Everything here is untrusted

The `ingress` package doc (`ingress.go:15`) is explicit: everything reaching this
handler is unauthenticated client input. The sibling `egress` package has the
opposite trust model — an identity carried by a CA-verified client certificate.
The two are kept in separate packages that cannot import each other, and the
`extproc` mux imports neither. See `cmd/atenet/internal/router/README.md`.

Direction is decided by **which filter chain the dataplane says accepted the
connection** (`xds.filter_chain_name`), never by anything in the request, so a
client cannot talk its way onto the egress path.

---

## 5. Gateway to worker

The cluster is `actor_original_dst`
(`xds.go:buildOriginalDstCluster`, line 767), an
[**ORIGINAL_DST cluster**](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/original_dst)
in `CLUSTER_PROVIDED` LB mode reading its destination from the dynamic-metadata
key ext_proc just wrote.

Why this cluster type rather than EDS or a Service: there is no stable set of
endpoints to discover. The destination is computed per request and is
different for the next request. ORIGINAL_DST is Envoy's escape hatch for "dial
exactly this address, I already decided."

The comment at `xds.go:101-104` names the important property: **the cluster does
not derive the destination from `:authority`**, so the client's `Host` survives
to atunnel untouched and actor identity travels in an explicit header instead.

Transport is mTLS with SPIFFE URI validation against the worker's pod identity.
The cluster also mirrors the downstream protocol upstream, so an HTTP/2 client's
gRPC keeps trailers and streaming end to end.

---

## 6. Inside the worker pod: atunnel ingress

`internal/atunnel/ingress.go`. Package doc: *"carries actor ingress and egress
through an ateom worker pod."* It runs inside the ateom process
(`cmd/ateom-gvisor/main.go:270`), on the pod side of the sandbox boundary.

### The activation model

Both halves of atunnel are long-lived across actor activations, but only carry
traffic for the actor **currently** assigned to this worker. `Activate` /
`Deactivate` swap an `active` pointer under a mutex, with a `WaitGroup` so
teardown can wait for in-flight work and a per-activation `context` that cancels
everything at once (`Activate`, `ingress.go:410`). *There can be only one active
actor per worker.*

### The authorization check

`authorize` (`ingress.go:475`) runs on every request:

```go
ref, err := atenet.ParseTargetActor(r.Header.Get(atenet.TargetActorHeader))
if err != nil {
    return ..., false      // → 421 + X-Ate-Assignment-Stale: true
}
...
if active == nil || active.ref != ref {
    return ..., false      // → 421 + X-Ate-Assignment-Stale: true
}
```

It authorizes on the **same `ate-target-actor` header the router overwrote**
(§4 step 6). That pairing is the point: the router resolves the actor and then
pins the header, so the value atunnel checks cannot have been chosen by the
client.

This is the correctness guard for the entire multiplexing model. The gateway
resumed the actor and got a worker IP, but the assignment can change between
that answer and the packet arriving. atunnel refuses rather than delivering one
actor's request into another actor's sandbox.

**421 Misdirected Request** is the right status — the client reached a server
that cannot serve this actor — and `X-Ate-Assignment-Stale: true` (`reject`,
`ingress.go:499`) exists so the router can distinguish atunnel's routing
rejection from a 421 the actor app produced on its own.

After authorizing, the reverse proxy's `Rewrite` hook deletes both
`ate-target-actor` and `X-Ate-Target-Port` before forwarding
(`ingress.go:126-139`), so the actor never sees the routing vocabulary. The
client's `Host` is preserved (`pr.Out.Host = pr.In.Host`).

### Protocol mirroring

`protocolMirrorTransport` (`newProtocolMirrorTransport`, `ingress.go:211`):
gRPC — HTTP/2 `POST` with `application/grpc` — goes upstream as
prior-knowledge h2c, because it needs trailers and full duplex. Everything else
is translated down to HTTP/1.1 even if it arrived as HTTP/2, so an HTTP/1.1-only
actor keeps working behind an HTTP/2 gateway. `isGRPC` deliberately excludes
gRPC-Web, which runs over HTTP/1.1.

### Worker ports

| Port | Listener |
|---|---|
| 443 | atunnel ingress HTTPS reverse proxy (`--atunnel-listen-address`) |
| 8443 | atunnel CONNECT ingress (`--atunnel-connect-listen-address`) |
| 15001 | atunnel egress, target of the nftables REDIRECT |

The CONNECT ingress listener on 8443 is opened and served
(`cmd/ateom-gvisor/main.go:294`). **Which listener is used depends on the
dataplane:**

- **Envoy** (the default) always targets `:443` — `targetAddr` is pinned to port
  443 in `ingress.go:146` — and carries the actor port in `X-Ate-Target-Port`.
  Even a CONNECT-tunneled request arrives at atunnel's *HTTPS* listener as an
  ordinary request.
- **agentgateway** (an optional component) uses only the CONNECT listener:
  `connectTargetPort: 8443`, and the config says so outright
  (`manifests/ate-install/components/agentgateway/configmap.yaml:72-73`).

That is why `atunnel.DefaultConnectPort` (`ingress.go:43`) has no Go callers —
its only consumer configures it in YAML. `client.go:33` still carries a
`TODO(liorlieberman): support/use CONNECT on Ingress as well` for the Envoy
path.

---

## 7. Actor egress: nftables and the tunnel

### The sandbox's network

`internal/ateomnet/net.go` builds a veth pair with a hardcoded point-to-point
`/30` (`net.go:40-47`):

```
sandbox netns                worker pod netns
  eth0  169.254.17.2/30        ateom0 169.254.17.1/30    eth0 <pod IP>
  default via 169.254.17.1
```

The actor sees a normal-looking `eth0` with a default route. Its packets land on
`ateom0` in the worker pod's netns and are *routed* from there — which is why
`EnableIPv4Forwarding` (`net.go:172`) has to turn on `ip_forward`, going as far
as temporarily remounting `/proc/sys` read-write to do it.

The `/30` being a literal constant is the addressing consequence of one active
actor per worker.

### The nftables rules

`InstallActorNftablesRules` (`net.go:203`) builds this table with the
`github.com/google/nftables` netlink library. Rendered as `nft` syntax:

```nft
table ip ateom_actor {
        chain prerouting {
                type nat hook prerouting priority dstnat; policy accept;
                ip saddr 169.254.17.2 meta l4proto tcp redirect to :15001
        }

        chain postrouting {
                type nat hook postrouting priority srcnat; policy accept;
                ip saddr 169.254.17.2 masquerade
        }

        chain forward {
                type filter hook forward priority filter; policy accept;
                ip saddr 169.254.17.2 meta l4proto udp udp dport != 53 counter drop
                accept
        }
}
```

**prerouting — the interception.** NAT base chains are conntrack-driven: only
`NEW` packets traverse them, so this rule sees the SYN and conntrack replays the
translation for the rest of the connection. REDIRECT is a special DNAT that
rewrites the destination to an address of the *ingress* interface plus the given
port, turning "forward out eth0" into "deliver locally" so the connection lands
on atunnel. It is chosen over plain DNAT specifically because **conntrack keeps
the original tuple**, recoverable with `getsockopt(SOL_IP, SO_ORIGINAL_DST)`
(`internal/atunnel/original_dst_linux.go`). That recovered address becomes the
CONNECT authority the egress gateway applies policy to.

atunnel's own connection out to the gateway is sourced from the pod's real IP,
not `169.254.17.2`, so the source match excludes it and there is no loop.

**The redirect is conditional.** `ActorEgressRedirectRule` returns `nil` for
port 0 (`net.go:337`), and `egressRedirectPort` returns 0 unless the actor's
spec names an `EgressGateway` (`cmd/ateom-gvisor/main.go:1200`). An actor
without one gets the chain with no rules in it, and all its TCP falls through to
the masquerade. Interception is opt-in per actor.

Note what the rule matches: source IP and `IPPROTO_TCP`, with **no port
predicate**. Every TCP port is redirected, including 53.

**postrouting — the compatibility escape hatch.** `169.254.17.2` is link-local
and unroutable, so anything that *does* get routed out needs its source
rewritten to the pod IP. In practice this carries everything the redirect does
not catch: UDP, ICMP — most importantly **DNS over UDP** (`net.go:251`).

**forward — the UDP narrowing.** `actorNonDNSUDPDropRule` (`net.go:352`) drops
actor UDP egress to every destination port but 53, with a counter, so a workload
that legitimately needs UDP shows up as a rising count in `nft list table ip
ateom_actor` rather than as an unexplained timeout. Order matters and the code
says so (`net.go:264`): the catch-all `accept` is appended after the drop.

The trailing `accept` is otherwise a no-op — in nftables an `accept` only
terminates *its own* chain, and the packet still traverses every other base
chain at the hook, so it cannot override a `drop` elsewhere. It states the
intent explicitly in the ateom-owned table.

Three things follow from DNS leaving outside the tunnel:

1. The actor resolves names itself, so the CONNECT authority the gateway sees is
   **always an address, never a hostname**. This is the root of the
   hostname-vs-address distinction in `EgressPolicy`.
2. The UDP rule narrows by **port only, not destination**. `udp dport 53` to any
   address is permitted and masqueraded, so it remains a policy bypass.
3. **DNS over TCP is broken for actors with an egress gateway.** TCP/53 has no
   exemption, so it is redirected into the tunnel, where a raw DNS stream offers
   no hostname and matches only a `cidrs` or `all` rule — in practice nothing,
   so it is denied. UDP/53 to the same resolver always succeeds. The visible
   symptom is that names resolve until an answer is large enough to set TC=1 and
   the resolver retries over TCP.

**The table as a unit.** Everything lives in `ateom_actor`
(`ActorNftTableName`), so nothing ever mutates CNI or kube-proxy chains, install
is idempotent (it opens with `RemoveActorNftablesRules`, `net.go:218`), and
teardown between activations is a single `DelTable`.

### The tunnel

`internal/atunnel/egress.go` + `client.go`. Per intercepted connection
(`Egress.handle`, `egress.go:241`):

1. Recover the original destination via `SO_ORIGINAL_DST`.
2. Refuse if there is no activation, or if the actor certificate has expired —
   *"Expiry blocks only new tunnels"* (`egress.go:250`); established connections
   drain.
3. `active.dialer.DialContext(ctx, destination)` — mTLS to the egress gateway,
   then `CONNECT <ip>:<port>`.
4. `copyBothWays` (`egress.go:285`).

Failures are typed so the actor side can tell them apart:
`ErrGatewayHandshake` (refused at TLS, the front door, `client.go:48`) versus
`ConnectRejectedError{StatusCode, Status, Message}` (authenticated fine, then
declined, `client.go:55`) — the latter is what an `EgressPolicy` denial looks
like from inside the sandbox.

### Policy is fetched and cached, not checked inline

`cmd/atenet/internal/router/egress/policycache.go` fetches
`GetActorEgressPolicy` per actor and caches it for `DefaultPolicyCacheTTL`
(10s), which bounds how stale a decision can be. Concurrent fetches are
singleflighted, the fetch timeout sits under the ext_proc `message_timeout` so a
slow control plane is a 503 rather than an Envoy timeout, and the *absence* of a
policy is cached too, so a flood of denied requests does not hammer ateapi.

### The per-activation certificate

`BrokerCertificateSource` (`internal/atunnel/credential.go`) generates **one
ECDSA P-256 key per activation**, and the key never leaves atunnel — only the
CSR crosses the node-local atelet broker socket. `ExpectedActorUID` stops a mint
started for an old activation from installing the newly assigned actor's
certificate. `renew` refreshes on a schedule.

---

## 8. The egress gateway

Deployment `atenet-egress` in `ate-system`
(`manifests/ate-install/atenet-egress.yaml`). Also two containers: an Envoy
(443 for CONNECT, 15000 admin) and the **same atenet binary** started with
`--mode=egress` (`atenet-egress.yaml:684`), serving only the egress ext_proc
handler — no xDS server, no Kubernetes access, and bound to `127.0.0.1:50051`.

The manifest is explicit that ingress and egress are separate Deployments
because they scale independently, not because they need separate binaries: one
instance can serve both with `--mode=all` (`atenet-egress.yaml:679`).

Its ServiceAccount has **no RBAC bound to it at all**, deliberately
(`atenet-egress.yaml:29-37`): with no xDS server there is no reason for this pod
to touch the Kubernetes API, and it reaches the control plane only over ateapi.

Unlike the ingress gateway, this Envoy's config is **static YAML in a
ConfigMap** — there is no xDS here.

On every CONNECT, `egress.Handler` re-verifies the actor's client certificate
against the actor-identity CA in Go, reads the `ActorIdentity` X.509 extension
out of it, and checks the certified UID against ateapi. Without
`--actor-identity-ca-file` the router has no roots and denies every CONNECT with
a 503 (`atenet-egress.yaml:695`).

Outbound, Envoy uses a
[`dynamic_forward_proxy`](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/http/http_proxy)
cluster, which resolves and dials whatever the CONNECT names rather than
requiring a preconfigured upstream.

### The MITM variant

`manifests/ate-install/atenet-egress-with-sdsmint.yaml` is the same gateway plus
TLS interception, so policy can be applied to the decrypted request rather than
just the address. It runs `atenet sdsmint`
(`cmd/atenet/internal/sdsmint/`), a **minting SDS server**: Envoy asks it over
the [SDS](https://www.envoyproxy.io/docs/envoy/latest/configuration/security/secret)
protocol for a certificate matching the SNI the actor requested, and sdsmint
mints one on the spot from the gateway's own CA.

That leaf chains to no public root, so **an actor that validates against only
the public roots breaks under this install** — every HTTPS request fails with a
certificate error until the gateway CA is projected into the actor's filesystem
and its TLS client is pointed at it.

This replaces the single egress chain with an internal `mitm_listener`
(`atenet-egress-with-sdsmint.yaml:278`) fronted by `tls_inspector` and
`http_inspector`, and three filter chains selected by what the client sends
first:

| Chain | Selected when | Policed on |
|---|---|---|
| `egress_tls_mitm` (`:302`) | a TLS ClientHello | SNI, then the decrypted Host and path |
| `egress_cleartext` (`:520`) | recognizable HTTP | Host and path, read directly |
| `egress_passthrough` (`:648`) | neither — the `raw_buffer` catch-all | nothing; relays opaquely to the address the CONNECT leg allowed |

The passthrough chain decides nothing: its `tcp_proxy` dials the
`original_dst_address` filter state the CONNECT leg produced, and with none
present the connection is closed before a byte moves (`UH` in the log). An
opaque stream names no host, so only a `cidrs` or `all` rule can allow it.

The inspectors are given `listener_filters_timeout: 1s` with
`continue_on_listener_filters_timeout: true`, because a server-speaks-first
protocol (SSH, SMTP, MySQL) would otherwise deadlock waiting for a client that
is itself waiting for the origin.

So the same actor's traffic is evaluated once at CONNECT (by address) and, on
the MITM and cleartext legs, again on the decrypted request (by hostname and
path). See [egress-trust-bundle.md](../egress-trust-bundle.md).

---

## 9. Identity and TLS

Four distinct credentials, easily confused:

| Credential | Who holds it | Issued by | Names |
|---|---|---|---|
| Service DNS cert | Control-plane services | `servicedns.ate.dev/identity` signer | a Kubernetes Service DNS name |
| Pod identity cert | Every Substrate pod, incl. workers | `podid.ate.dev/identity` signer | the pod / KSA — "equivalent to a KSA token" |
| **Actor identity cert** | atunnel, per activation | ateapi, via the atelet broker | the *actor*: atespace, name, UID |
| sdsmint leaf | The MITM egress gateway | sdsmint's local CA | whatever SNI the actor asked for |

The first two come from `cmd/podcertcontroller`, which implements both signers
against local CAs and is explicitly a placeholder for upstream Kubernetes
signers (`cmd/podcertcontroller/main.go:15-21`).

The distinction that matters: **pod identity describes the host, actor identity
describes the actor.** A worker pod runs many actors over its life, so its pod
certificate cannot authorize anything actor-specific. That is the entire reason
the `ActorIdentity` exchange exists — see
[actor-identity.md](../actor-identity.md).

Where mTLS is actually used:

- Ingress gateway → atunnel (§5): gateway presents pod identity, validates
  atunnel's SPIFFE URI.
- atunnel → egress gateway (§7): atunnel presents the **actor** certificate.
  This is the only place the actor identity is used on the wire.
- Both atenet containers → ateapi: pod identity client cert, servicedns trust
  bundle (`atenet-egress.yaml:688-690`).

### Dataplane attribute keys

Declared once in `cmd/atenet/internal/router/extproc/attributes.go`:

| Key | Direction | Carries |
|---|---|---|
| `dev.ate.target.actor` (`:32`) | ingress | The target actor, captured at `connect_terminate` for the internal-listener hop |
| `dev.ate.connect.authority` (`:33`) | ingress | The outer CONNECT authority, which is where the actor port comes from |
| `dev.ate.actor.identity` (`:44`) | egress | The actor identity established on the CONNECT leg |
| `dev.ate.egress` (`:52`) | egress | Dynamic-metadata namespace for the allowed passthrough destination |
| `dev.ate.extproc.direction` (`:77`) | both | Explicit direction hint |
| `xds.filter_chain_name` (`:90`) | both | Envoy's own attribute; the authoritative direction signal |
| `connection.requested_server_name` (`:115`) | egress | The SNI `tls_inspector` read |

The naming rule (README, "adding a dataplane attribute"): keys Substrate owns
are rooted at `dev.ate.`; keys someone else owns keep *their* reverse-DNS root.
`xds.filter_chain_name` stays in Envoy's namespace and is not re-homed.

---

## 10. Concepts to have in hand

Non-obvious things this project leans on. Skim the ones you don't know before
reading the code.

**Envoy**

- [**xDS**](https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol) —
  the config API. The ingress Envoy boots nearly empty and gets everything from
  `xds.go` over ADS. The egress Envoy is fully static.
- [**ext_proc**](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_proc_filter)
  — an HTTP filter that streams the request to an external gRPC service, which
  can mutate it or reject it. Substrate's routing brain. It fails closed in both
  directions: the egress manifests set `failure_mode_allow: false` explicitly
  (`atenet-egress.yaml:182`, `:348`) and the ingress xDS leaves it at the proto
  default, which is the same. If the ext_proc server is down, requests fail
  rather than bypassing it.
- **Filter state and dynamic metadata** — two per-request side-channels. Filter
  state carries values *into* ext_proc; dynamic metadata carries them *out* to
  other filters and clusters.
- [**Internal listeners**](https://www.envoyproxy.io/docs/envoy/latest/configuration/other_features/internal_listener)
  — a listener with no socket, reachable only from inside the same Envoy. Used
  to loop CONNECT-tunneled bytes back through the full filter chain.
- [**ORIGINAL_DST cluster**](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/load_balancing/original_dst)
  — "dial the address someone already computed" instead of load-balancing over
  discovered endpoints.
- **`UH` response flag** — "no healthy upstream," the flag you will see in
  access logs when a cluster has nowhere to send a request.

**HTTP**

- [**`CONNECT`**](https://developer.mozilla.org/en-US/docs/Web/HTTP/Methods/CONNECT)
  — asks a proxy to open a raw TCP tunnel to `host:port`. Used twice here, for
  different reasons: to name a non-default actor port on ingress, and as the
  actual transport for all actor egress.
- **`421 Misdirected Request`** — "I can't serve this authority." atunnel's
  answer when the actor has moved.

**Linux**

- **nftables hooks and priorities** — `prerouting`/`postrouting` for NAT,
  `forward` for routed packets, with `dstnat`(-100) and `srcnat`(100) ordering.
  NAT chains only see conntrack-`NEW` packets.
- **`SO_ORIGINAL_DST`** — reads a redirected connection's pre-NAT destination
  back out of conntrack. The keystone of transparent egress interception.
- **REDIRECT vs DNAT vs TPROXY** — REDIRECT is DNAT to the local box; it is
  chosen here because it preserves the original tuple for the above.
- **veth pairs and netns** — the sandbox gets one end, the worker pod the other.

**Kubernetes and identity**

- **EndpointSlices** — the router has RBAC to watch them
  (`atenet-router.yaml:26`) for dataplane endpoint discovery.
- [**SPIFFE**](https://spiffe.io/) — identity as a URI SAN in an X.509 cert.
  Upstream TLS validates the SPIFFE ID rather than a DNS name.
- **PodCertificate signers** — `podcertcontroller` issues short-lived certs to
  pods without ever writing a Secret.

---

## 11. Cheat sheet

### Ports

| Where | Port | What |
|---|---|---|
| Service `atenet-router` | 80 / 443 | HTTP / HTTPS workload traffic |
| | 8081 / 8444 | CONNECT / CONNECT-over-TLS (arbitrary-port ingress) |
| | 4040 | status |
| atenet-router container | 18000 | xDS |
| | 50051 | ext_proc |
| | 9090 | metrics |
| ingress Envoy | 9901 | admin |
| Service `atenet-egress` | 443 | CONNECT from atunnel |
| egress ext-proc container | 50051 | ext_proc (bound `127.0.0.1`) |
| egress Envoy | 15000 | admin |
| worker pod | 443 | atunnel ingress HTTPS (Envoy targets this) |
| | 8443 | atunnel CONNECT ingress (agentgateway only) |
| | 15001 | atunnel egress (REDIRECT target) |
| sandbox | — | `eth0` = 169.254.17.2/30, gw 169.254.17.1 |

### Headers

| Header | Set by | Read by |
|---|---|---|
| `ate-target-actor` | the client, then **overwritten** by ingress ext_proc (`ingress.go:163`) | ext_proc to route; atunnel `authorize` to admit |
| `X-Ate-Target-Port` | the ingress route config (`xds.go:838-847`) | atunnel, to pick the actor port |
| `X-Ate-Assignment-Stale` | atunnel `reject` (`ingress.go:499`) | the router, to tell a routing 421 from an app 421 |

Both `ate-target-actor` and `X-Ate-Target-Port` are deleted before the request
reaches the actor (`internal/atunnel/ingress.go:126-139`).

### Where things live

| | |
|---|---|
| `cmd/atenet/internal/router/xds.go` | Every Envoy listener and cluster on ingress |
| `cmd/atenet/internal/router/extproc/` | The ext_proc mux and shared vocabulary |
| `cmd/atenet/internal/router/ingress/` | Resume, park, route |
| `cmd/atenet/internal/router/egress/` | Actor-identity authn, egress policy, policy cache |
| `cmd/atenet/internal/sdsmint/` | Minting SDS server for TLS interception |
| `internal/atenet/headers.go` | `ate-target-actor` and its parser |
| `internal/atunnel/` | Both halves of the in-pod tunnel |
| `internal/ateomnet/net.go` | veth, forwarding, nftables |
| `manifests/ate-install/atenet-*.yaml` | Both gateways |

### Reading order

1. `docs/glossary.md` — vocabulary.
2. This document.
3. `cmd/atenet/internal/router/README.md` — why ingress and egress are separate
   packages, and how to add a dataplane attribute.
4. `docs/request-parking.md` — the saturation path through ingress.
5. `docs/actor-identity.md` — the credential in full.
6. `docs/egress-trust-bundle.md` — TLS interception.

---

## 12. Known gaps

Worth knowing before you trust a mental model built from the above:

- **IPv4 only** throughout the actor dataplane: the nftables table family, the
  payload offsets, the `/30`, and `SO_ORIGINAL_DST`. `net.go:208` tracks
  dual-stack as one piece of work.
- **UDP egress is narrowed by port, not destination** (`net.go:352`). `udp
  dport 53` to *any* address is permitted and masqueraded, so it is still an
  unmediated path out of the pod.
- **DNS over TCP is broken under an egress gateway.** No exemption from the
  redirect, and a raw DNS stream matches no hostname rule, so the truncation
  fallback is denied while UDP/53 to the same resolver succeeds. See §7.
- **DNS is not policy-controlled.** The actor resolves names itself, so
  hostname-based egress rules only bind where TLS is intercepted.
- **Ingress has no authorization.** The edge listeners require no client
  credential, and nothing between the gateway and the actor asks who the caller
  is — atunnel's check is a misrouting guard, not an access control decision.
  `internal/authz/model.fga` declares the relations but has no Go
  implementation.
- **Egress drain and long-lived tunnels are unresolved.** The drainer polls
  `downstream_cx_active` to zero, which suits short ingress request/response
  connections but not a CONNECT tunnel atunnel holds open for the life of an
  actor's connection (`atenet-egress.yaml:625-630`).
- **`docs/architecture.md` opens by saying much of it is aspirational.** Treat
  it as direction, not description.
