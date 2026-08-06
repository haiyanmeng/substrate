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
// Its unit tests drive that logic directly against a synthesized
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
// not deletion -- the behaviors they pin are properties of the enforcement
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
// Whether a destination is dialed depends on which route its policy selects,
// and the two answers differ. A deferred CONNECT (the hostname policies) goes
// to the internal MITM listener, which answers without reaching the network --
// dynamic_forward_proxy only resolves a name on the inner request, which these
// tests do not send. A passthrough CONNECT goes to an ORIGINAL_DST cluster that
// really dials the authority, and Envoy withholds the 200 until that upstream
// connection is up. So an allowed destination that does not answer comes back
// as a 503 rather than as an open tunnel, which is why the passthrough cases
// assert through requireAuthorized rather than requireConnected.
//
// 1.2.3.4 is inside the allowlist and does not answer, so the in-block case
// always takes the 503 path. 9.9.9.9 is a real resolver: where the cluster has
// outbound internet the ALLOW_ALL case is a genuine end-to-end passthrough, and
// where it does not the same assertion still holds on authorization alone.
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
		// means the CONNECT is expected to be authorized.
		wantReason string
		// dialsOut marks the cases whose policy resolves to passthrough. Those
		// CONNECTs really dial the destination (see the destination block
		// above), so an authorized one still fails when nothing answers, and
		// the assertion has to be on the authorization outcome alone. The
		// MITM-deferred cases never leave the pod and must connect outright.
		dialsOut bool
	}{{
		name:        "ALLOW_ALL reaches anything",
		actor:       actorWideOpen,
		destination: outOfBlockDestination,
		dialsOut:    true,
	}, {
		name:        "ALLOW_BY_IP_BLOCK reaches an address in an allowed block",
		actor:       actorMetricsShipper,
		destination: inBlockDestination,
		dialsOut:    true,
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
		// authority is an IP literal and the actor resolves DNS itself. Now that
		// the gateway runs the inner checkpoint, the CONNECT is a DEFERRAL and
		// so it succeeds -- the hostname check happens later, on the tunneled
		// request.
		//
		// A pass here therefore proves less than the other cases do, and the
		// gap is deliberate rather than overlooked: the probe stops at the inner
		// TLS handshake and never issues an inner HTTP request, which is what
		// the second ext_proc filter fires on. So nothing below reaches
		// DecideInner, and neither the hostname allowlist nor credential
		// injection has end-to-end coverage. Closing that needs a probe that
		// speaks HTTP inside the tunnel and reports what the destination
		// received; poc/extproc asserts it against a local Envoy in the
		// meantime.
		//
		// What a pass DOES prove is the failure this file previously guarded:
		// if --inner-listen were dropped from the Deployment while these routes
		// stayed, extprocd would have nobody to defer to and would deny here.
		name:        "ALLOW_BY_HOSTNAME is deferred to the inner checkpoint",
		actor:       actorRepoReader,
		destination: anyDestination,
	}, {
		name:        "BASIC_CREDENTIAL_INJECT is deferred to the inner checkpoint",
		actor:       actorInvoiceAgent,
		destination: anyDestination,
	}, {
		// No policy is not an empty policy. An actor the table has never heard
		// of must be refused rather than defaulted to anything.
		//
		// It is refused with DENY_ALL's own reason, not a distinct one:
		// Snapshot.Lookup maps an actor it has never seen to DenyAll
		// (internal/extproc/policy.go), so by the time DecideConnect runs there
		// is nothing left to tell the two apart. That is the fail-closed default
		// working, and the reason string is deliberately not more specific --
		// confirming to a caller that its actor is absent from the table says
		// more than a denial needs to.
		name:        "an actor with no policy is refused",
		actor:       actorUnknown,
		destination: anyDestination,
		wantReason:  "policy DENY_ALL",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credential := e2e.MintActorCredential(t, ctx, extproc.DemoAtespace, tc.actor, "uid-"+tc.actor)
			result := probe.Probe(t, ctx, probeapi.Request{
				Destination:         tc.destination,
				ClientCredentialPEM: credential,
			})
			who := extproc.DemoAtespace + "/" + tc.actor
			switch {
			case tc.wantReason != "":
				requireDenied(t, who, result, tc.wantReason)
			case tc.dialsOut:
				requireAuthorized(t, who, result)
			default:
				requireConnected(t, who, result)
			}
		})
	}
}

// TestEgressAuthzRefusesUnusableIdentities covers the certificates that carry
// no actor the gateway can act on. Each is a distinct way of arriving without
// an identity, and each must be refused.
//
// Two of them are refused for their own reason; the third is not, and the
// difference is the gateway's, not this test's. A certificate the gateway
// cannot read an identity out of fails in extprocd's identity layer and says
// so. A certificate it reads perfectly well, which simply names no actor, is
// not an error at all -- it produces the zero actor key, and every path from
// there is the ordinary unknown-actor denial.
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
	//
	// The refusal is DENY_ALL's, for the reason above: a certificate with no
	// ActorIdentity is not malformed, so it yields the zero actor key rather
	// than an identity error, and Snapshot.Lookup denies the zero key exactly
	// as it denies an unknown actor. What matters is that the default is a
	// denial and not an allow; that it is indistinguishable from
	// acme-prod/quarantined's is the fail-closed path arriving at the same
	// place from a different direction.
	t.Run("a pod certificate with no actor identity", func(t *testing.T) {
		result := probe.Probe(t, ctx, probeapi.Request{Destination: anyDestination})
		if result.Identity != probeapi.IdentityPod {
			t.Fatalf("probe presented %q, want %q", result.Identity, probeapi.IdentityPod)
		}
		requireDenied(t, "pod identity", result, "policy DENY_ALL")
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

// requireAuthorized asserts the gateway did not refuse the CONNECT on policy
// grounds, for the passthrough cases where it then really dials.
//
// Reaching the destination is not this suite's business and cannot be made
// this suite's business: the demo table's allowed blocks correspond to nothing
// a cluster can reach, so an allowed CONNECT legitimately ends in Envoy's
// upstream connect timeout. What is asserted instead is the shape of that
// failure. A 403 is a policy denial and fails here. So is anything that is
// neither a 403 nor an upstream-connect failure, which is what keeps this from
// silently passing when the tunnel breaks before the dial -- an ext_proc
// outage under failure_mode_allow false, say, which would otherwise look like
// "not a 403" and slip through.
//
// The gap this leaves: where a cluster has no outbound internet, no case in
// this file proves a passthrough tunnel carries bytes end to end. Closing it
// needs a reachable in-cluster destination inside an allowed block, which the
// hardcoded table cannot express without widening it.
func requireAuthorized(t *testing.T, who string, result probeapi.Result) {
	t.Helper()
	if result.Connected {
		t.Logf("%s: CONNECT to %s was allowed and the destination answered", who, result.Destination)
		return
	}
	if strings.Contains(result.ConnectError, "403") {
		t.Fatalf("%s was refused for %s by policy, but should have been allowed: %s", who, result.Destination, result.ConnectError)
	}
	if !strings.Contains(result.ConnectError, "upstream connect error") {
		t.Fatalf("%s failed to reach %s, and not because the destination was unreachable: %s",
			who, result.Destination, result.ConnectError)
	}
	t.Logf("%s: CONNECT to %s was allowed; the destination did not answer, which is expected: %s",
		who, result.Destination, strings.TrimSpace(result.ConnectError))
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
