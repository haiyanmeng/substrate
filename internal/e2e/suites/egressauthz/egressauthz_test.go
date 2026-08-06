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

// Package egressauthz e2e-tests the egress gateway's policy enforcement point.
//
// extprocd answers an ext_proc call on every CONNECT and decides, from the
// actor named by the client certificate, whether that destination is allowed.
// Its unit tests drive that logic directly against a synthesised
// ProcessingRequest. What they cannot cover is everything between the actor and
// the decision: that Envoy calls extprocd at all, that the identity survives the
// x-forwarded-client-cert round trip, that the actor CA half of the gateway's
// two-CA trust bundle is really trusted at the front door, and that a denial
// arrives as a 403 the client can see rather than as a tunnel that quietly
// opens anyway.
//
// Every assertion here is made on the outcome of a real CONNECT through the
// deployed gateway.
//
// The policy table is internal/extproc/hardcoded.go: five actors in one
// atespace, one per policy kind. The names below are that table. When it is
// replaced by a real policy source these tests need the equivalent fixtures,
// not deletion -- the behaviours they pin are properties of the enforcement
// point, not of the table.
package egressauthz

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/e2e/fixtures/egressprobe/probeapi"
	"github.com/agent-substrate/substrate/internal/extproc"
)

// Destinations chosen against metrics-shipper's CIDR allowlist, which is the
// only policy that reads the CONNECT authority.
//
// Neither is ever dialled. An allowed CONNECT routes to the gateway's internal
// MITM listener -- the vhost has a single connect_matcher route -- which answers
// without reaching the network, and dynamic_forward_proxy only resolves a name
// on the inner request, which these tests do not send. 9.9.9.9 is a real
// resolver address rather than documentation space on purpose: if a future
// change did start dialling, the failure should be a timeout that is obviously
// wrong, not a connection to a host the test appeared to intend.
const (
	inBlockDestination    = "1.2.3.4:443" // inside 1.2.3.0/24
	outOfBlockDestination = "9.9.9.9:443" // outside every allowed block
	anyDestination        = "1.2.3.4:443" // for policies that ignore the authority
)

// The five actors of internal/extproc/hardcoded.go, plus one that is not in it.
// The atespace is imported rather than restated so a rename breaks the build
// here instead of turning every case into a silent unknown-actor denial.
const (
	actorQuarantined    = "quarantined"     // DENY_ALL
	actorWideOpen       = "wide-open"       // ALLOW_ALL
	actorRepoReader     = "repo-reader"     // ALLOW_BY_HOSTNAME
	actorMetricsShipper = "metrics-shipper" // ALLOW_BY_IP_BLOCK
	actorInvoiceAgent   = "invoice-agent"   // BASIC_CREDENTIAL_INJECT
	actorUnknown        = "not-in-the-table"
)

// TestEgressAuthzEnforcesEachPolicy walks the whole policy table against the
// deployed gateway: what each actor is allowed to reach, and what it is not.
//
// One probe for all the cases. They are independent, and standing up a pod per
// case would cost minutes to prove nothing extra.
func TestEgressAuthzEnforcesEachPolicy(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)
	probe := e2e.StartEgressProbe(t, ctx, ns.Name)

	cases := []struct {
		name        string
		actor       string
		destination string
		// wantReason is the substring the gateway's refusal must contain. Empty
		// means the CONNECT is expected to succeed.
		wantReason string
	}{{
		name:        "ALLOW_ALL reaches anything",
		actor:       actorWideOpen,
		destination: outOfBlockDestination,
	}, {
		name:        "ALLOW_BY_IP_BLOCK reaches an address in an allowed block",
		actor:       actorMetricsShipper,
		destination: inBlockDestination,
	}, {
		name:        "ALLOW_BY_IP_BLOCK refuses an address outside every block",
		actor:       actorMetricsShipper,
		destination: outOfBlockDestination,
		wantReason:  "destination not in any allowed block",
	}, {
		name:        "DENY_ALL refuses everything",
		actor:       actorQuarantined,
		destination: inBlockDestination,
		// Denied before any TLS work, and before the destination is even
		// considered -- an in-block address is used precisely so a pass cannot
		// be the IP check firing by accident.
		wantReason: "policy DENY_ALL",
	}, {
		// The two hostname policies cannot be decided at the CONNECT: the
		// authority is an IP literal and the actor resolves DNS itself. They are
		// deferrals to an inner checkpoint the gateway does not run, so they
		// must deny. Allowing them would leave these actors constrained only by
		// the gateway's global sdsmintd allowlist -- a per-actor bypass that
		// looks like working authorization in every log line.
		name:        "ALLOW_BY_HOSTNAME fails closed without the inner checkpoint",
		actor:       actorRepoReader,
		destination: anyDestination,
		wantReason:  "requires the inner checkpoint",
	}, {
		name:        "BASIC_CREDENTIAL_INJECT fails closed without the inner checkpoint",
		actor:       actorInvoiceAgent,
		destination: anyDestination,
		wantReason:  "requires the inner checkpoint",
	}, {
		// No policy is not an empty policy. An actor the table has never heard
		// of must be refused rather than defaulted to anything.
		name:        "an actor with no policy is refused",
		actor:       actorUnknown,
		destination: anyDestination,
		wantReason:  "unknown policy kind",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credential := e2e.MintActorCredential(t, ctx, extproc.DemoAtespace, tc.actor, "uid-"+tc.actor)
			result := probe.Probe(t, ctx, probeapi.Request{
				Destination:         tc.destination,
				ClientCredentialPEM: credential,
			})
			if tc.wantReason == "" {
				requireConnected(t, extproc.DemoAtespace+"/"+tc.actor, result)
				return
			}
			requireDenied(t, extproc.DemoAtespace+"/"+tc.actor, result, tc.wantReason)
		})
	}
}

// TestEgressAuthzRefusesUnusableIdentities covers the certificates that carry
// no actor the gateway can act on. Each is a distinct way of arriving without
// an identity, and each must be refused for its own reason rather than
// collapsing into a generic failure.
func TestEgressAuthzRefusesUnusableIdentities(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)
	probe := e2e.StartEgressProbe(t, ctx, ns.Name)

	// A certificate scoped to something other than atunnel. This is the check
	// cmd/atelet/credentialbroker.go records as the reason its mint declares a
	// purpose at all: a certificate issued for some future use must not be
	// replayable to open a tunnel. ate-apiserver never mints one, so the only
	// way to test it is to forge it.
	t.Run("a certificate whose purpose is not atunnel", func(t *testing.T) {
		credential := e2e.MintForgedActorCredential(t, ctx, extproc.DemoAtespace, actorWideOpen,
			`{"Atespace":"`+extproc.DemoAtespace+`","ActorName":"`+actorWideOpen+`","ActorUid":"uid-forged","Purpose":"session"}`)
		result := probe.Probe(t, ctx, probeapi.Request{
			Destination:         anyDestination,
			ClientCredentialPEM: credential,
		})
		// wide-open is ALLOW_ALL, so a gateway that ignored the purpose would
		// let this through. That is what makes this case worth running.
		requireDenied(t, "forged purpose", result, "no usable actor identity")
	})

	// An identity that parses but is not complete. Rejected by validation rather
	// than treated as an actor named by whatever fields did arrive.
	t.Run("a certificate with an incomplete identity", func(t *testing.T) {
		credential := e2e.MintForgedActorCredential(t, ctx, extproc.DemoAtespace, actorWideOpen,
			`{"Atespace":"`+extproc.DemoAtespace+`","ActorName":"","ActorUid":"uid-forged","Purpose":"atunnel"}`)
		result := probe.Probe(t, ctx, probeapi.Request{
			Destination:         anyDestination,
			ClientCredentialPEM: credential,
		})
		requireDenied(t, "incomplete identity", result, "no usable actor identity")
	})

	// The probe's own workload certificate: valid, trusted at the front door,
	// and carrying no ActorIdentity at all. It authenticates a substrate
	// workload, which is not the same as authorizing an actor's egress. This is
	// the fail-closed proof -- every pod in the cluster holds one of these.
	t.Run("a pod certificate with no actor identity", func(t *testing.T) {
		result := probe.Probe(t, ctx, probeapi.Request{Destination: anyDestination})
		if result.Identity != probeapi.IdentityPod {
			t.Fatalf("probe presented %q, want %q", result.Identity, probeapi.IdentityPod)
		}
		requireDenied(t, "pod identity", result, "unknown policy kind")
	})
}

// requireConnected asserts the gateway established the tunnel.
func requireConnected(t *testing.T, who string, result probeapi.Result) {
	t.Helper()
	if !result.Connected {
		t.Fatalf("%s was refused for %s but should have been allowed: %s", who, result.Destination, result.ConnectError)
	}
	t.Logf("%s: CONNECT to %s was allowed", who, result.Destination)
}

// requireDenied asserts the gateway refused the CONNECT, and refused it for the
// expected reason.
//
// Checking the reason and not just the refusal is the point. Several unrelated
// faults -- an untrusted client CA, extprocd being down with failure_mode_allow
// false, a listener that never got the filter -- also produce a failed CONNECT,
// and a test that accepted any of them would keep passing while the policy it
// claims to check had stopped being consulted. The 403 is what says the refusal
// came from a policy decision rather than from the transport.
func requireDenied(t *testing.T, who string, result probeapi.Result, wantReason string) {
	t.Helper()
	if result.Connected {
		t.Fatalf("%s was allowed to reach %s but should have been refused (%s)", who, result.Destination, wantReason)
	}
	if !strings.Contains(result.ConnectError, "403") {
		t.Fatalf("%s failed to reach %s, but not with a 403 from the policy layer: %s", who, result.Destination, result.ConnectError)
	}
	if !strings.Contains(result.ConnectError, wantReason) {
		t.Fatalf("%s was refused for %s with the wrong reason: got %q, want it to mention %q",
			who, result.Destination, result.ConnectError, wantReason)
	}
	t.Logf("%s: CONNECT to %s was refused as expected: %s", who, result.Destination, strings.TrimSpace(result.ConnectError))
}
