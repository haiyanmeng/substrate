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
const DefaultDestination = "192.0.2.1:443"

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
	// Destination is the CONNECT authority, as ip:port. Empty means
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
type Result struct {
	Destination string `json:"destination"`
	SNI         string `json:"sni"`
	// Identity is IdentityPod or IdentitySupplied.
	Identity string `json:"identity"`
	// Connected reports that the gateway answered the CONNECT with a 2xx and a
	// tunnel was established.
	Connected bool `json:"connected"`
	// ConnectError carries the refusal, including the gateway's status line and
	// response body, so a denial can be attributed to a specific policy.
	ConnectError string `json:"connect_error,omitempty"`
	// HandshakeOK reports the inner TLS handshake. Always false when no SNI was
	// requested.
	HandshakeOK    bool   `json:"handshake_ok"`
	HandshakeError string `json:"handshake_error,omitempty"`
	// ChainPEM is the chain the gateway presented on the inner handshake, leaf
	// first. The probe does not verify it; see the probe's package comment.
	ChainPEM string `json:"chain_pem,omitempty"`
}
