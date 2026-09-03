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

import (
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

// Direction is the gateway a request arrived through. It selects the handler,
// so it must come from something the dataplane asserts rather than from the
// request.
type Direction string

const (
	// DirectionIngress is inbound traffic addressed to an actor.
	DirectionIngress Direction = "ingress"
	// DirectionEgress is an actor's outbound CONNECT, before the gateway
	// tunnels it out.
	DirectionEgress Direction = "egress"
	// DirectionEgressMITM is one request from inside an already-authenticated
	// tunnel, seen on the MITM gateway's decrypted leg. It is a separate
	// direction from DirectionEgress because it is a separate question: the
	// CONNECT asks who the actor is, and this asks whether that actor may reach
	// the destination the request names.
	DirectionEgressMITM Direction = "egress-mitm"
)

const (
	// EgressFilterChainName is the Envoy filter chain that terminates actor
	// egress CONNECTs, and so the one that selects the egress handler. It must
	// stay in sync with the filter chain name in
	// manifests/ate-install/atenet-egress.yaml and
	// manifests/ate-install/atenet-egress-with-sdsmint.yaml.
	EgressFilterChainName = "egress"
	// EgressMITMFilterChainName and EgressMITMCleartextFilterChainName are the
	// MITM listener's two HTTP filter chains — tunneled TLS that the gateway
	// terminates with a minted leaf, and tunneled cleartext. They select the
	// same handler; they are two chains only because one has a transport socket
	// to terminate and the other does not. Both must stay in sync with
	// manifests/ate-install/atenet-egress-with-sdsmint.yaml.
	//
	// The listener's third chain, the raw TCP passthrough, is deliberately
	// absent: it is a tcp_proxy with no HTTP filter chain to run ext_proc in,
	// and an opaque byte stream names no destination to authorize.
	EgressMITMFilterChainName          = "egress_mitm"
	EgressMITMCleartextFilterChainName = "egress_mitm_cleartext"
)

// egressFilterChains maps each filter chain the egress gateway names to the
// direction whose handler serves it. A chain missing from this map is treated
// as ingress, the fail-safe direction — see directionOf.
var egressFilterChains = map[string]Direction{
	EgressFilterChainName:              DirectionEgress,
	EgressMITMFilterChainName:          DirectionEgressMITM,
	EgressMITMCleartextFilterChainName: DirectionEgressMITM,
}

// directionOf reports which direction's handler an ext_proc RequestHeaders
// callback belongs to.
//
// Dispatch is by filter chain, not by :method, because the two directions apply
// opposite trust models: on egress the actor identity comes from a client
// certificate Envoy validated against the actor-identity CA, while on ingress
// every request header is unauthenticated client input. Keying on :method would
// let any external client sending CONNECT select the egress handler and use its
// denial messages as an actor-existence and status oracle. Envoy asserts the
// filter chain name; the request cannot influence it.
//
// An unrecognized or absent attribute means ingress, the fail-safe direction:
// an egress request misrouted to the ingress handler fails to parse as an actor
// DNS name and 404s, whereas the reverse leaks control-plane state.
func directionOf(req *extprocv3.ProcessingRequest) Direction {
	switch dir := Direction(requestAttribute(req, directionAttribute)); dir {
	case DirectionEgress, DirectionEgressMITM:
		return dir
	}
	if dir, ok := egressFilterChains[filterChainName(req)]; ok {
		return dir
	}
	return DirectionIngress
}

// filterChainName returns the xds.filter_chain_name attribute Envoy attached to
// the request, or "" when the listener did not request the attribute. The
// attributes map is keyed by the ext_proc filter's name within the HCM chain,
// which we do not want to hardcode here, so scan every entry.
func filterChainName(req *extprocv3.ProcessingRequest) string {
	return requestAttribute(req, FilterChainNameAttribute)
}

func requestAttribute(req *extprocv3.ProcessingRequest, name string) string {
	for _, attrs := range req.GetAttributes() {
		if v, ok := attrs.GetFields()[name]; ok {
			return v.GetStringValue()
		}
	}
	return ""
}
