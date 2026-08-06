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

// Package probeapi is the wire contract between the in-cluster egress probe and
// the suites that drive it.
//
// It exists as its own package because the probe is package main and cannot be
// imported. The alternative -- each suite re-declaring the response struct --
// was tolerable when there was one field to get wrong; it is not now that the
// probe reports two independent outcomes.
//
// Nothing here may import anything outside the standard library: this package is
// linked into the probe image.
package probeapi

// DefaultDestination is the CONNECT authority used when a Request leaves it
// empty.
//
// atunnel takes the destination from SO_ORIGINAL_DST and rejects hostnames
// (internal/atunnel/client.go:153-169), so it must be an IP literal. 192.0.2.0/24
// is TEST-NET-1: it resolves nowhere, which is all a test of the MITM leg needs,
// because the name under test travels in the tunneled ClientHello rather than
// here. A test of egress *policy* wants a destination the policy has an opinion
// about, and passes one explicitly.
//
// "Resolves nowhere" stopped being free once the CONNECT vhost began branching
// on x-ate-egress-mode. It is still right for an actor whose policy routes to
// MITM, because that leg answers inside the pod. An actor whose policy resolves
// to passthrough is dialled for real, and against this address that is a hang
// until the handshake budget expires -- which reads as a gateway fault rather
// than as a test pointed at the wrong actor. Pick the destination and the
// actor together.
const DefaultDestination = "192.0.2.1:443"

// How the probe reaches the destination, selected by Request.Via.
//
// The two modes exercise the gateway from opposite sides, and which one a suite
// wants follows from what it is trying to prove.
const (
	// ViaTunnel issues an explicit CONNECT to the gateway with a client
	// certificate of the caller's choosing. The probe is an ordinary Pod doing
	// deliberate proxy traffic, which is what lets a suite present an arbitrary
	// -- or deliberately malformed -- identity and read the gateway's own
	// refusal back out of the 403.
	//
	// This is the default, so a Request that predates Via keeps working.
	ViaTunnel = "tunnel"
	// ViaDirect dials the destination as if the gateway did not exist: no
	// CONNECT, no client certificate, nothing proxy-aware. Only meaningful when
	// the probe runs as an Actor, where nftables REDIRECTs the connection to
	// atunnel and the tunnel is built by ateom out of a certificate the probe
	// never sees (internal/ateomnet/net.go InstallActorNftablesRules).
	//
	// That indirection is the point: it is the only mode that tests the path
	// production traffic actually takes. It also means the gateway's refusal
	// reason is unavailable here -- atunnel logs the 403 and closes the socket,
	// so a denied actor sees an EOF and nothing more.
	ViaDirect = "direct"
)

// Identity source names reported back in Result.Identity, so a suite can tell
// from the response which credential the probe actually presented rather than
// which one it meant to.
const (
	// IdentityPod is the probe's own workload certificate. It carries a
	// substratex509.PodIdentity and no ActorIdentity, so the egress gateway's
	// policy layer sees no actor at all.
	IdentityPod = "pod"
	// IdentitySupplied is a credential the caller posted, normally an actor
	// certificate the suite minted.
	IdentitySupplied = "supplied"
)

// Request asks the probe for one trip through the egress gateway.
type Request struct {
	// Via is ViaTunnel or ViaDirect. Empty means ViaTunnel.
	Via string `json:"via"`
	// Destination is the address to reach, as ip:port -- the CONNECT authority
	// under ViaTunnel, the dial target under ViaDirect. Empty means
	// DefaultDestination.
	Destination string `json:"destination"`
	// SNI, when set, makes the probe complete an inner TLS handshake inside the
	// tunnel, which is what makes Envoy ask sdsmintd to mint a leaf for that
	// name. Empty stops after the CONNECT, which is what a test of egress
	// authorization wants: the decision has already been made by then.
	SNI string `json:"sni"`
	// ClientCredentialPEM is the certificate chain and private key presented to
	// the gateway's front door. Empty means the probe's own Pod credential.
	//
	// This is how a suite gets to be an actor. The probe has no actor identity
	// of its own and cannot mint one; the suite signs a certificate with the
	// actor CA and posts it here.
	//
	// Ignored under ViaDirect, where the probe presents no certificate at all
	// and ateom supplies the actor's real one out of band.
	ClientCredentialPEM string `json:"client_credential_pem"`
}

// Result is the probe's report on one Request.
//
// Connected and HandshakeOK are separate because the two things that can refuse
// this trip refuse it at different points, and a test needs to know which. The
// egress PEP denies the CONNECT itself with a 403, before any tunnel exists.
// sdsmintd denies by declining to mint, which leaves Envoy with no certificate
// to present and fails the *inner* handshake in a tunnel that opened fine.
// Collapsed into one field, a policy change looks like a minter regression.
//
// Under ViaDirect the split still holds but the halves move. Nothing can refuse
// the connection: the REDIRECT is local, so the socket is up before atunnel has
// spoken to the gateway at all, and Connected is true even for an actor with no
// egress rights. A denial lands on the handshake instead, as an EOF.
type Result struct {
	// Via echoes the mode the probe ran in, so a result read on its own cannot
	// be misread as the other one.
	Via         string `json:"via"`
	Destination string `json:"destination"`
	SNI         string `json:"sni"`
	// Identity is IdentityPod or IdentitySupplied. Empty under ViaDirect, where
	// the probe presents no certificate of its own.
	Identity string `json:"identity"`
	// Connected reports that the gateway answered the CONNECT with a 2xx and a
	// tunnel was established -- or, under ViaDirect, merely that the TCP
	// connection came up. See the note above: it proves much less there.
	Connected bool `json:"connected"`
	// ConnectError carries the refusal, including the gateway's status line and
	// response body, so a denial can be attributed to a specific policy. Under
	// ViaDirect there is no such body to carry.
	ConnectError string `json:"connect_error,omitempty"`
	// HandshakeOK reports the inner TLS handshake. Always false when no SNI was
	// requested.
	HandshakeOK    bool   `json:"handshake_ok"`
	HandshakeError string `json:"handshake_error,omitempty"`
	// ChainPEM is the chain the gateway presented on the inner handshake, leaf
	// first. The probe does not verify it; see the probe's package comment.
	ChainPEM string `json:"chain_pem,omitempty"`
}
