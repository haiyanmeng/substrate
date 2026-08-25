# Carrying actor identity from the CONNECT leg to the MITM leg

How to make the actor's verified identity readable on `mitm_listener`, using
`set_filter_state` sourced from the mTLS peer certificate.

Target file: **`manifests/ate-install/atenet-egress-with-sdsmint.yaml`**. The
MITM listener moved there from `atenet-egress.yaml` at commit `00714070`; the
plain manifest is now the pre-MITM config and has none of the anchors below.

**Steps 1-3 are in that manifest now**, and step 4 exists behind
`--experimental-additional-egress-extproc-service`. What follows is why each
piece is shaped the way it is, not work to do; the appendix is still
unimplemented.

## The problem

`mitm_listener`'s `DownstreamTlsContext` has no `validation_context` and no
`require_client_certificate` — it terminates the actor's TLS to the
*destination*, so there is no client certificate on that leg, no XFCC for the
HCM to synthesize, and nothing to parse. The checkpoint that can see the real
hostname cannot see who is asking.

Headers do not solve it. Envoy terminates the CONNECT, so the outer and inner
requests are separate HTTP transactions and nothing set on the CONNECT reaches
the inner one. A header sent *inside* the tunnel is worse than useless — it is a
channel the actor controls end to end, so it would let one actor name another.

Filter state is the sound carrier, and it takes two edits. The second one fails
**silently** when omitted: the request proxies, the object is simply absent, and
every read resolves to the zero value.

## 1. Set the filter state from the peer certificate

A new filter in the `egress` listener's `http_filters`, before
`envoy.filters.http.router` (`:208`):

```yaml
              # The actor's identity, carried across the CONNECT/MITM boundary.
              # The value comes from the peer certificate Envoy already verified
              # against the actor-identity CA above, so it is not something the
              # actor can write -- unlike any header, which on this path is
              # attacker-controlled input.
              - name: envoy.filters.http.set_filter_state
                typed_config:
                  "@type": type.googleapis.com/envoy.extensions.filters.http.set_filter_state.v3.Config
                  on_request_headers:
                  - object_key: ate.actor.identity
                    factory_key: envoy.string
                    # One internal hop -- this listener's upstream IS
                    # mitm_listener -- so ONCE is sufficient. TRANSITIVE would
                    # widen the scope for nothing: egress_forward_proxy dials a
                    # real socket, not an internal connection.
                    shared_with_upstream: ONCE
                    # Nothing downstream may overwrite an authenticated identity.
                    read_only: true
                    # No object at all beats an empty one: a reader can tell
                    # "absent" from "present and blank".
                    skip_if_empty: true
                    format_string:
                      text_format_source:
                        inline_string: "%DOWNSTREAM_PEER_URI_SAN%"
                      # Without this an absent SAN renders as "-", which is a
                      # non-empty string and fails open at whatever reads it.
                      omit_empty_values: true
```

Notes on each field that is easy to get wrong:

- **`factory_key: envoy.string`** is what makes the object a `StringAccessor`.
  Left unset, `factory_key` defaults to the value of `object_key`, and Envoy
  looks for a registered factory by that name. See
  [Choosing `factory_key`](#choosing-factory_key) for why `envoy.string` is the
  only correct choice here, and for the one place in this manifest that
  deliberately does the opposite.
- **`%DOWNSTREAM_PEER_URI_SAN%`** is the same operator the CONNECT access log
  already uses at `:110`. It is derived from the TLS layer, which
  `require_client_certificate: true` and `trusted_ca` enforce before any filter
  runs — so this needs no ext_proc, no metadata, and no particular filter
  ordering.
- **Do not source this from a header.** `%REQ(x-ate-actor-key)%` and anything
  like it reads actor-controlled input, which defeats the entire point.

## 2. Carry it onto the internal listener

`shared_with_upstream` puts the object on the *upstream* connection. Getting it
onto the internal listener's *downstream* connection requires
`internal_upstream` on the `mitm_internal` cluster (`:500`):

```yaml
      - name: mitm_internal
        connect_timeout: 1s
        # Copies filter state shared with the upstream onto the internal
        # listener's downstream connection. Without this the object set above
        # never reaches mitm_listener, with no error anywhere.
        transport_socket:
          name: envoy.transport_sockets.internal_upstream
          typed_config:
            "@type": type.googleapis.com/envoy.extensions.transport_sockets.internal_upstream.v3.InternalUpstreamTransport
            transport_socket:
              name: envoy.transport_sockets.raw_buffer
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.transport_sockets.raw_buffer.v3.RawBuffer
        load_assignment:
          cluster_name: mitm_internal
          ...
```

`transport_socket` is required — you are wrapping the socket that would
otherwise be implicit, and there is no TLS on this hop, so it is `raw_buffer`.

Unlike `passthrough_metadata`, there is no list of keys to enumerate. From the
proto: *"All filter state objects that are shared with the upstream connection
are also shared with the downstream internal connection using this transport
socket."*

This is the edit most likely to be forgotten and the hardest to diagnose,
because omitting it looks exactly like the filter in step 1 not running.

### `ONCE` vs `TRANSITIVE`

```
NONE       — not shared with the upstream internal connections
ONCE       — shared with the upstream internal connection
TRANSITIVE — shared with the upstream internal connection and any internal
             connection upstream from it
```

The path is one internal hop, so `ONCE` is correct. Reach for `TRANSITIVE` only
if another internal listener is ever chained behind `mitm_listener`.

## 3. Verify before building on it

The cheapest check needs no new filter. Add to `mitm_listener`'s `json_format`
(`:296` for the TLS leg, `:425` for the cleartext one):

```yaml
                      actor: "%FILTER_STATE(ate.actor.identity:PLAIN)%"
```

Drive one request and confirm the field is populated with the SPIFFE ID. If it
is empty, step 1 did not run or step 2 is missing — both present identically.

`PLAIN` is not decoration and not the default; omitting it prints nothing
useful. See [The `%FILTER_STATE%` operator](#the-filter_state-operator).

## 4. Read it from an ext_proc on the MITM leg

Behind `--experimental-additional-egress-extproc-service`, the filter block
spliced over the `#ATE_MITM_EXTPROC_FILTER` markers
(`hack/experimental-additional-egress-extproc.sh:96`) reads it with:

```yaml
    request_attributes:
    - filter_state['ate.actor.identity']
```

Subscript the attribute. Bare `filter_state` yields the whole CEL map, which
ext_proc flattens to the literal string `"CelMap value"` — present, non-empty,
and carrying nothing.

On the receiving side, `ProcessingRequest.attributes` is a `map<string, Struct>`
keyed by the *filter* that produced the entry. Do not hardcode that key; iterate
the map and look for the attribute name inside each `Struct`, the way
`extproc/dispatch.go:77` already does.

The value is a whole URI —
`spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>` — not two
fields. Validate the trust domain and the path shape rather than splitting on
`/`, and note that Envoy comma-joins if a certificate ever carries more than one
URI SAN.

## Reference: `factory_key` and `%FILTER_STATE%`

The two knobs above that look cosmetic and are not. Both fail by rendering
empty, which on this path is indistinguishable from steps 1 and 2 being broken.

### The `%FILTER_STATE%` operator

```
%FILTER_STATE(KEY:SERIALIZATION):MAX_LENGTH%
```

`SERIALIZATION` selects how the object is turned into log text:

| | |
|---|---|
| `PLAIN` | `serializeAsString()` — the raw string |
| `TYPED` | `serializeAsProto()`, rendered as JSON. **The default when `:SERIALIZATION` is omitted** |
| `FIELD:name` | one named field out of the proto |

An `envoy.string` object is a `StringAccessor`: it has a string and no proto.
`TYPED` therefore has nothing to render, so `%FILTER_STATE(ate.actor.identity)%`
— the spelling that looks like it should just work — logs empty. That is the
same output as the object never having been set, so the default silently
reproduces the exact failure step 3 exists to detect. Write `:PLAIN`.

`MAX_LENGTH` truncates. Leave it off: these SPIFFE IDs run ~70 characters, and a
truncated identity still parses as a plausible one.

### Choosing `factory_key`

`factory_key` names a registered `filter_state.object` factory; Envoy enumerates
the whole registry at startup, so the authoritative list is in the proxy's own
log rather than in any doc:

```sh
kubectl logs -n ate-system deploy/atenet-egress -c envoy | grep -A40 'filter_state.object'
```

The live v1.37 gateway registers exactly 16. Fifteen of them are **behavioural**
— writing one changes what the proxy does:

| Factory | Effect of writing it |
|---|---|
| `envoy.network.upstream_server_name` | overrides upstream SNI |
| `envoy.network.upstream_subject_alt_names` | overrides SAN verification |
| `envoy.network.transport_socket.original_dst_address` | sets the address dialled |
| `envoy.upstream.dynamic_host`, `envoy.upstream.dynamic_port` | redirect the upstream |
| `envoy.tcp_proxy.cluster` | reroutes to another cluster |
| `envoy.tcp_proxy.disable_tunneling`, `envoy.tcp_proxy.per_connection_idle_timeout_ms` | change tunnelling and timeout |
| `envoy.network.ip`, `envoy.network.application_protocols`, `envoy.network.network_namespace` | connection/ALPN/netns properties |
| `envoy.filters.listener.original_dst.local_ip`, `…remote_ip` | original-destination resolution |
| `envoy.ratelimit.hits_addend` | rate-limit accounting |
| `envoy.router.debug_config` | router debug behaviour |
| **`envoy.string`** | **nothing — an inert string container** |

`envoy.string` is the only inert entry, and inertness is the property being
selected for. An authenticated identity should mean something to the readers you
choose and nothing to Envoy; any other factory couples the identity to proxy
behaviour, so a rename or a copy-paste turns an audit field into a routing
decision. Picking one of the other 15 to carry a SPIFFE ID would, at best, be a
type error the proxy accepts.

Leaving `factory_key` unset is not neutral either — it defaults to `object_key`,
so `object_key: ate.actor.identity` alone looks for a factory by that name, finds
none, and rejects the config.

**The deliberate opposite is in this same manifest.** The second
`set_filter_state` filter (`:199`) writes
`envoy.network.transport_socket.original_dst_address` with no `factory_key` at
all, precisely so it resolves to that behavioural factory: the passthrough chain
has no SNI and no Host, and this is what makes it dial the CONNECT authority.
Two filters, adjacent, opposite intents — the identity one must stay inert, the
address one must not be.

### There is no hashing factory, and no `string_hashed`

Filter state objects can influence the upstream connection pool key, which makes
"store a hash instead of the raw string" sound like a configurable choice. It is
not one. `string_hashed` does not exist in Envoy, and the 16 entries above are
the complete `filter_state.object` registry — there is no hashing factory among
them. `envoy.load_balancing_policies.ring_hash` and
`envoy.matching.matchers.consistent_hashing` are the names that come up in a
search; both are different extension categories and neither is selectable as a
`factory_key`. Getting a hashed object would mean a C++ type implementing
`hashKey()`, not a config edit.

If the goal is a hashed value rather than a hashed object, Envoy already exposes
one to the same `format_string`: `%DOWNSTREAM_PEER_FINGERPRINT_256%`, the SHA-256
of the peer certificate. Note what it identifies — the certificate, not the
actor. It changes on every rotation, so it is a correlation handle, not a
replacement for the SPIFFE ID.

### Unverified

Two claims adjacent to the above that could not be checked from this repo, kept
separate from the findings on purpose:

- **The wire type on the ext_proc side.** `filter_state['ate.actor.identity']`
  surfaces a `StringAccessor`, but whether it arrives in the
  `ProcessingRequest.attributes` `Struct` as a `string_value` or a `bytes_value`
  is not pinned down by the proto. Check it when the ext_proc lane is first
  deployed, and have the receiver handle both rather than assuming.
- **Whether `envoy.string` contributes to the upstream pool key.** Filter state
  objects that implement `hashKey()` fragment the connection pool. Envoy's
  string accessor is very unlikely to implement it — meaning this object should
  *not* cause per-actor pool fragmentation — but that was not verified against
  Envoy source. Treat it as the first thing to check if pool reuse on the
  internal hop ever looks wrong.

## Two things this leaves open

### The UID is not carried, and cannot be

The SPIFFE ID is built at `cmd/ateapi/internal/actoridentity/actoridentity.go:203`
as `atespace/<a>/actor/<n>` — name only. `ActorUid` lives solely in the
`ActorIdentity` X.509 extension under the private PEN OID
`1.3.6.1.4.1.11129.2.12` (`internal/substratex509/substratex509.go:143`), and
Envoy has no primitive for reading an arbitrary OID out of a peer certificate.

Names are reusable; UIDs are not. Checking the UID against a live actor is what
stops a certificate outliving its actor being deleted and recreated under the
same name. The CONNECT-leg ext_proc already does that check, so a name arriving
on the MITM leg is backed by it — **provided the MITM leg treats filter state as
context and never re-derives authorization from the name alone.** If that stops
being true, see the appendix.

### Both `mitm_listener` chains receive it

Filter state is a connection-level object and both chains sit behind
`mitm_internal`, so the `raw_buffer` chain (`:281`) gets it too. Anything that
consumes it must be registered on both chains, or the cleartext path is an
unpoliced route to the same forward proxy — the cleartext leg reads its
destination straight from the `Host` header and mints nothing, so it has no SNI
to cross-check either.

## Appendix: carrying the UID via dynamic metadata

Only needed if the MITM leg must authorize on the UID rather than the name.
Costs a Go change and two more config edits, and reintroduces an ordering
dependency — the whole reason to prefer `%DOWNSTREAM_PEER_URI_SAN%` when it
suffices.

**Publish it.** In `cmd/atenet/internal/router/egress/egress.go`, replacing the
bare allow:

```go
// egressMetadataNamespace is the dynamic-metadata namespace the verified
// identity is published under. It must match the ext_proc filter's
// metadata_options.receiving_namespaces.untyped entry and the
// %DYNAMIC_METADATA(...)% lookup in the set_filter_state filter, both in
// manifests/ate-install/atenet-egress-with-sdsmint.yaml. A mismatch is silent:
// the metadata is dropped and the MITM leg sees empty filter state.
const egressMetadataNamespace = "ate.egress"

return extproc.Result{
	Response: &extprocv3.HeadersResponse{
		Response: &extprocv3.CommonResponse{},
	},
	DynamicMetadata: &structpb.Struct{
		Fields: map[string]*structpb.Value{
			egressMetadataNamespace: structpb.NewStructValue(&structpb.Struct{
				Fields: map[string]*structpb.Value{
					"actor_uid": structpb.NewStringValue(identity.ActorUid),
				},
			}),
		},
	},
}, nil
```

`extproc.Result` already carries `DynamicMetadata` (`extproc/handler.go:69`) and
the mux already attaches it to the `ProcessingResponse`
(`extproc/extproc.go:174`), so no plumbing is needed. Add
`"google.golang.org/protobuf/types/known/structpb"` to the imports.

**Accept it.** Writing is denied by default — per the `MetadataOptions` proto,
*"Set to empty or leave unset to disallow writing any received dynamic metadata.
Receiving of typed metadata is not supported."* On the CONNECT-leg ext_proc,
after `request_attributes` (`:152`):

```yaml
                  metadata_options:
                    receiving_namespaces:
                      untyped: ["ate.egress"]
```

**Convert it.** A third `FilterStateValue` alongside the one in step 1. Unlike
that one, this filter must now run **after** ext_proc: placed before it, it
formats a namespace that has not been written yet and silently yields empty.

```yaml
                  - object_key: ate.actor.uid
                    factory_key: envoy.string
                    shared_with_upstream: ONCE
                    read_only: true
                    skip_if_empty: true
                    format_string:
                      text_format_source:
                        inline_string: "%DYNAMIC_METADATA(ate.egress:actor_uid)%"
                      omit_empty_values: true
```

## Verified against

`go-control-plane/envoy@v1.37.0` for every proto claim (`FilterStateValue` and
its `SharedWithUpstream` enum, `MetadataOptions`, `InternalUpstreamTransport`,
`ProcessingResponse`), and `manifests/ate-install/atenet-egress-with-sdsmint.yaml`
plus `cmd/atenet/internal/router/` at commit `00714070`. Line anchors will drift
as that manifest changes.

The `factory_key` and `%FILTER_STATE%` reference was added later, against
`8eb9dcc1` (branch `experiment-flag-extproc`, which is also where the object was
renamed `ate.actor` → `ate.actor.identity`, and where the `actor:` access-log
field was introduced — that field is **not on `main` and not deployed**). The
16-entry registry table was read off a running `envoyproxy/envoy:v1.37-latest`
in `deploy/atenet-egress`, not from Envoy source, so re-derive it with the
command above rather than trusting it across an image bump.

Related: `docs/dev/egress-mitm-extproc-io.md` for the ext_proc I/O contract on
this listener, and `docs/dev/egress-pluggable-extproc-contract.md:275-372`
("Placement B") for the operator-pluggable design this feeds.
