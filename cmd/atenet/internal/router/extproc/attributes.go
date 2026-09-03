// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package extproc

// Substrate's dataplane attribute namespace: the filter-state objects and CEL
// request attributes the gateways carry alongside a request, declared here once
// so a key means the same thing in the Go that reads it and in the dataplane
// configuration that sets it.
//
// Every substrate-owned key is rooted at the reverse-DNS "dev.ate." prefix.
// These keys live in namespaces shared with the proxies that carry them -- Envoy
// filter state, agentgateway CEL -- where a vendor-qualified root is what keeps
// substrate's keys from colliding with anyone else's. That is a different
// constraint from the telemetry attributes in internal/ateattr, which are
// substrate's own metric dimensions and stay on dotted "ate.". Neither is the
// "ate.dev/" slash form, which is Kubernetes labels only.
const (
	// AuthorityFilterStateKey is the filter-state key holding an ingress
	// request's :authority, set by xds.go's authorityFilterStateFilter.
	// Ingress-only: it names the actor a request is addressed to.
	AuthorityFilterStateKey = "dev.ate.authority"
	// AuthorityFilterStateAttribute is the CEL expression ext_proc evaluates to
	// read AuthorityFilterStateKey back out.
	AuthorityFilterStateAttribute = "filter_state['" + AuthorityFilterStateKey + "']"

	// ActorIdentityFilterStateKey is the filter-state key holding the actor
	// identity the egress gateway read from the peer certificate it verified
	// against the actor-identity CA, as that certificate's SPIFFE URI SAN.
	// Egress-only, set on the CONNECT leg in
	// manifests/ate-install/atenet-egress-with-sdsmint.yaml. The CONNECT-leg
	// handler does not read it — it authenticates the certificate itself — but
	// it is the only sound carrier of the actor's identity across the
	// CONNECT/MITM boundary, so the MITM handler names the actor from it.
	ActorIdentityFilterStateKey = "dev.ate.actor.identity"
	// ActorIdentityFilterStateAttribute is the CEL expression ext_proc
	// evaluates to read ActorIdentityFilterStateKey back out.
	ActorIdentityFilterStateAttribute = "filter_state['" + ActorIdentityFilterStateKey + "']"

	// EgressDestinationFilterStateKey is the filter-state key holding the
	// egress CONNECT's authority — the address the actor's kernel was dialing,
	// which atunnel takes from SO_ORIGINAL_DST and so is always a literal
	// IP:port. Set on the CONNECT leg and carried across the internal-listener
	// hop, because the MITM leg sees only what is inside the tunnel: a
	// hostname, with the original destination IP nowhere in the request.
	// EgressPolicy's IPBlockRule is written against that IP.
	EgressDestinationFilterStateKey = "dev.ate.egress.destination"
	// EgressDestinationFilterStateAttribute is the CEL expression ext_proc
	// evaluates to read EgressDestinationFilterStateKey back out.
	EgressDestinationFilterStateAttribute = "filter_state['" + EgressDestinationFilterStateKey + "']"

	// directionAttribute carries the Direction outright, for dataplanes that
	// have no Envoy filter chain to name. It is set from a dataplane expression,
	// never from a client header. No dataplane in this repository sets it today:
	// Envoy names its filter chain, and agentgateway routes both directions
	// through its own substrateIngress/substrateEgress policies rather than
	// ext_proc.
	directionAttribute = "dev.ate.extproc.direction"
)

// FilterChainNameAttribute is the CEL attribute carrying the name of the filter
// chain that accepted the request. Envoy's own, not substrate's, so it is not
// under "dev.ate.". The egress Envoy asks for it via request_attributes on its
// ext_proc filter.
//
// Do not "improve" this to xds.listener_name: Envoy 1.34 cannot parse that one,
// and rather than failing config load it logs "error parsing cel expression" at
// trace level and sends an empty attributes map. An absent attribute means
// ingress here, so every egress CONNECT would silently take the ingress path and
// 404 on the actor DNS name parse.
const FilterChainNameAttribute = "xds.filter_chain_name"
