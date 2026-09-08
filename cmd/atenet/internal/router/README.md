# router

Router has several responsibilities:

* Serves Envoy xDS configuration when `--atenet-router=envoy` (the default).
  With `--atenet-router=agentgateway`, the sidecar uses a static ConfigMap and
  atenet does not start an xDS server.
* ext_proc server for the dataplane. To make the deployment and debugging easier, we will run this component together
  with the router, but this will be split later into its own component.
  * ext_proc will call into the ATE gRPC API to get the set of relevant backends (specific the worker IP) and
    route the traffic accordingly
  * Make sure the interface with ATE API is pluggable so that we can test with a mock ATE API.
* Runs an xDS server for the Envoy deployment that defines the Cluster information for the ATEs.
  * the xDS configuration will configure Envoy to send traffic to ext_proc
* Parks requests whose actor cannot be served immediately due to transient
  worker-pool saturation, retrying the resume until the actor is routable or a
  bounded wait elapses, instead of failing fast. See
  [docs/request-parking.md](../../../../../docs/request-parking.md).
* Drains gracefully on SIGTERM: flips `/readyz` so the Service stops sending
  new connections, waits out endpoint propagation (`--drain-delay`), drains the
  dataplane's established connections (Envoy only — driven over its admin API;
  agentgateway manages its own termination), gracefully stops the ext_proc
  server so parked requests finish normally (`--drain-timeout`, derived from
  the parking budget), then writes a drain-complete marker that releases the
  dataplane container's `preStop` hook. See `drain.go` and `envoydrain.go`.
* Authenticates actor identity on egress: on every CONNECT, the egress
  gateway's ext_proc handler re-verifies the actor's client certificate against
  the actor-identity CA, reads the `ActorIdentity` X.509 extension out of it,
  and checks the certified UID against the ATE API.
* Decides per connection whether the egress gateway intercepts TLS, from the
  actor's `tls_interception_exemptions`. See
  [TLS interception exemptions](#tls-interception-exemptions).
* Serves arbitrary-port ingress: a client reaches a port on the actor other
  than its default (80) by sending an HTTP CONNECT to
  `<actor-dns>:<port>` on `--port-connect`/`--port-connect-tls`, rather than
  naming the port some other way. Envoy terminates the CONNECT and reinjects
  the tunneled bytes into an internal listener that runs the same ext_proc
  path as ordinary traffic, so each request inside a long-lived tunnel still
  resumes the actor and re-routes independently if it moves workers. Only
  HTTP(S) traffic over the tunnel is supported today -- see `xds.go`'s
  `connect_terminate`/`main_internal` listeners and
  `ingress.Handler.HandleRequestHeaders`.

## packages

The ext_proc server handles both traffic directions, and they apply opposite
trust models — egress derives the actor identity from a client certificate the
gateway verified against the actor-identity CA, ingress treats every request
header as unauthenticated client input — so the two are kept in separate
packages that cannot reach into each other:

* `extproc` — the mux, and nothing else. It terminates the ext_proc stream,
  decides which direction a request arrived on, dispatches to the `Handler`
  registered for that direction, and records latency and outcome. It also owns
  the vocabulary both handlers share (`RequestMetadata`, `Result`, `ReqError`).
  It imports neither handler package.
* `ingress` — resume, park, and route to the actor's worker.
* `egress` — certificate-based actor-identity authentication for outbound
  CONNECTs, and the exemption set each one resolves to.
* `egressxds` — the xDS server that renders those exemption sets into the
  egress gateway's dispatch listener. It depends on `egress`, not the other way
  around: `egress` reaches it through a one-method `ExemptionRegistry`
  interface, so the handler can be tested without a dataplane.

Direction is decided by the filter chain the dataplane says accepted the
request (`xds.filter_chain_name`, an Envoy attribute the egress gateway is
configured to send), never by anything in the request itself, so a client
cannot pick the egress path by crafting one. `router` itself does the wiring.

## adding a dataplane attribute

The filter-state objects and request attributes a proxy carries alongside
a request are declared once, in `extproc/attributes.go`. 

### name it

A new key substrate owns is rooted at `dev.ate.`, then a dotted path naming the
thing it carries: `dev.ate.<area>.<thing>`, or `dev.ate.<thing>` when there is
no area to disambiguate. These keys live in a namespace owned by the proxy that
carries them — Envoy filter state, agentgateway CEL — and shared with whatever
else a deployment configures alongside substrate, so the reverse-DNS root is
what keeps them from colliding.

A key someone else owns keeps *their* reverse-DNS root; do not re-home it under
`dev.ate.` to make the set look uniform. `xds.filter_chain_name` is Envoy's own
attribute and stays in Envoy's namespace. A key defined by a vendor's extension
or by an additional ext_proc service a deployment splices in belongs under that
vendor's own root, on the same reasoning that gives substrate `dev.ate.`. The
prefix is a claim about who may rename the key, so getting it wrong means
substrate is renaming something it does not own, or holding a name it never
reserved.

### wire it up

1. **Declare the constant** in `extproc/attributes.go`.
2. **Set it in the dataplane.**
3. **Ask for it.** ext_proc only receives the attributes named in its filter's
   `request_attributes`. A key that is set but not requested arrives as nothing
   at all, which reads the same as a key that was never set.
4. **Read it** through `RequestMetadata.Attribute`.

### keep it trustworthy

An attribute is only as trustworthy as the thing that set it. Every value here
comes from something the dataplane itself derived — a peer certificate Envoy
verified against the actor-identity CA, an `:authority` captured before the
request entered a tunnel — never from a client header, which an actor controls
end to end. That is what makes filter state a sound carrier across the
CONNECT/MITM boundary in the first place, and a new attribute sourced from a
header gives the property back.

## modes

One binary serves both directions. `--mode` selects which:

| `--mode` | ext_proc handlers | xDS server | Kubernetes access |
| --- | --- | --- | --- |
| `ingress` | ingress | ingress dataplane (`--port-xds`) | yes |
| `egress` | egress | dispatch listener only (`--port-egress-xds`) | none |
| `all` (default) | both | both | yes |

The two xDS servers are separate, on separate ports, because `--mode=all` runs
both. The egress one serves exactly one listener to one node and reads nothing
from Kubernetes.

The mux refuses a direction this instance was not started to serve (404) rather
than falling back to the other handler, which would run the request through the
wrong trust model.

Ingress and egress are deployed separately today — `atenet-router` fronts the
ingress dataplane, `atenet-egress` the egress gateway — because the two scale
independently, not because they need separate binaries.

`--atenet-router` selects the dataplane for both Deployments. Each gateway has
its own static configuration because ingress and egress scale independently.

## TLS interception exemptions

Under an sdsmint install the egress gateway terminates and re-originates every
TLS connection an actor opens, so the actor sees a leaf the gateway minted
rather than the origin's. Some destinations cannot be reached that way at all:
a pinned certificate, or mutual TLS the actor holds the client key for. An
actor's `EgressPolicy.tls_interception_exemptions` lists the hostnames whose
TLS the gateway must leave alone.

**An exemption is not an authorization.** It says *how* a destination is
reached, not *whether*. `rules` is still what decides that.

### how a connection is dispatched

Envoy can only match an SNI against literals it was configured with; there is
no matcher that searches a list carried on the connection. So the patterns have
to be in the config, and only a name for them can travel with the request.

1. `NewExemptionSet` folds a policy's patterns to lowercase, drops any trailing
   dot, sorts and deduplicates them, and hashes the result. Two actors that
   exempt the same names get the same **set ID**, and share one rendering.
2. On each CONNECT the egress handler reads the actor's policy, resolves its
   set, and registers it with `egressxds`. `Register` blocks until the gateway
   has acknowledged a snapshot containing that set — naming a set the gateway
   does not have yet would dispatch the connection to a filter chain that does
   not exist.
3. The handler returns the set ID as dynamic metadata. A `set_filter_state`
   filter in the bootstrap copies it into filter state, which is what survives
   the hop into an internal listener.
4. `egress_dispatch`, served over LDS by `egressxds`, matches on that filter
   state to pick the set's subtree, then on SNI within it. A hit goes to the
   passthrough chain, which dials the original destination unmodified. Anything
   else goes to the MITM chain.

A pattern is either an exact hostname or a single leftmost-label wildcard:
`*.cdn.example.com` covers `assets.cdn.example.com` and neither
`cdn.example.com` nor `a.b.cdn.example.com`.

This is the Envoy gateway only. `--atenet-router=agentgateway` runs a different
proxy with no equivalent of `filter_chain_matcher`, and its MITM variant
intercepts everything; an actor's exemptions there are ignored, which fails in
the safe direction but is not the behavior the policy asked for.

### failure modes

Every one of them intercepts, which is the behavior of the gateway without this
feature at all: no policy, an empty exemption list, an unreachable control
plane, no gateway subscribed to LDS, a NACKed or unacknowledged snapshot, an
unknown set ID on the connection, an SNI outside the set, or no SNI at all.
Failing the other way would hand an actor an untapped tunnel by breaking
something.

The only case that fails the CONNECT rather than intercepting it is a policy
lookup that errors for a reason other than `NotFound`, which the handler
already treats as a control plane it cannot vouch for.

### bounds

atenet has no way to enumerate egress policies, so `egressxds` learns sets from
live traffic instead. That means it also has to forget them: it holds at most
512 sets, evicting the least recently used, and sweeps any set unused for 30
minutes. An evicted set is re-registered by the next CONNECT that needs it.

Snapshot versions carry a per-process epoch, so a gateway reconnecting after an
atenet restart cannot have a stale version mistaken for an acknowledgement of
the current one.

## status page

Serve a `/statusz` page on port 8080.

Contents:

* Global flags values
* Command line args
* Last 100 queries served
* Build tag
