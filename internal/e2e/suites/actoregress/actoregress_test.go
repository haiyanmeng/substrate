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

// Package actoregress e2e-tests the path an actor's egress actually takes.
//
// ate-api-server sets --egress-gateway-address, so every actor's TCP is
// REDIRECTed by ateom into atunnel and tunneled to the egress gateway
// (internal/ateomnet/net.go InstallActorNftablesRules). Three things on that
// path are covered by nothing else:
//
//   - The redirect. No other suite makes an actor open a socket.
//   - The certificate ateom mints through the credential broker. egressauthz
//     and sdsmint sign their own with the actor CA, so ate-api-server could
//     start issuing a certificate the gateway rejects and both stay green.
//   - --egress-gateway-address surviving the trip through ateapi, atelet and
//     the EgressGateway message into prepareActorEgress.
//
// The probe here therefore dials as if no gateway existed -- probeapi.ViaDirect
// -- and the tunnel is built underneath it out of a certificate it never sees.
// That is also the limitation of the suite: an actor cannot choose its own
// identity, so the interesting per-policy refusals stay in
// internal/e2e/suites/egressauthz, and the choice of SNI stays in
// internal/e2e/suites/sdsmint. What is left here is the one thing only an actor
// can show.
//
// Requires the fixture: hack/install-ate.sh --deploy-demo-egress.
package actoregress

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/e2e/fixtures/egressprobe/probeapi"
	"github.com/agent-substrate/substrate/internal/extproc"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

const (
	// The fixture deployed by hack/install-demo-egress.sh.
	probeTemplateNamespace = "ate-demo-egress"
	probeTemplateName      = "egressprobe"

	// ACTOR NAMES ARE NOT OURS TO CHOOSE. The egress policy table is keyed on
	// atespace plus actor name (internal/extproc/hardcoded.go), and it is
	// hardcoded, so selecting a policy means being called one of these. The
	// alternative -- e2e-only entries in the table under test -- was rejected
	// when the demo table was introduced.
	//
	// The consequence is that this suite cannot run twice at once against one
	// cluster: two runs would fight over one actor. It is also why the actors
	// are not suffixed with a timestamp the way every other suite's are, and
	// why createProbeActor tolerates one left behind by a run that crashed
	// before its cleanup.

	// allowedActor is ALLOW_BY_HOSTNAME, which is the only policy kind that
	// still routes to the MITM leg. ALLOW_ALL and ALLOW_BY_IP_BLOCK are decided
	// at the CONNECT and take the passthrough route, which really dials the
	// destination -- against probeDestination that is a hang, not a handshake.
	allowedActor = "repo-reader"
	// deniedActor is DENY_ALL: refused at the CONNECT, before any tunnel.
	deniedActor = "quarantined"

	// probeDestination is TEST-NET-1 (RFC 5737), reachable from nowhere. That
	// is the point: if a certificate comes back from here, it was minted by the
	// gateway, because there is nothing at the other end to serve one.
	//
	// atunnel takes the CONNECT authority from SO_ORIGINAL_DST and rejects
	// hostnames, so this has to be an IP literal either way.
	probeDestination = probeapi.DefaultDestination
)

// TestActorEgressIsTunneled is the anchor: an actor makes what it believes is a
// direct TLS connection to an address that does not exist, and gets back a
// certificate chaining to the cluster's MITM CA.
//
// Nothing in the cluster but the egress gateway can produce that chain, so one
// assertion covers the whole path -- the redirect is installed, the broker's
// certificate was accepted at the front door, the policy allowed the CONNECT,
// the route selected the MITM leg, and sdsmintd minted. Any of those broken and
// there is no chain to verify.
func TestActorEgressIsTunneled(t *testing.T) {
	ctx := context.Background()

	root := e2e.MITMRootCertificate(t, ctx)
	actor := createProbeActor(t, ctx, allowedActor)

	// Unique per run so the leaf is minted rather than served out of Envoy's
	// live secret set, which holds a name for --idle (30m). A cached leaf would
	// still prove the traffic crossed the gateway, but it would stop proving
	// that sdsmintd is answering.
	//
	// *.example.com is in sdsmintd's --allow (atenet-egress.yaml). The actor's
	// own hostname allowlist -- github.com and friends -- does not cover this
	// and does not have to: that list is checked at the inner checkpoint, which
	// fires on the tunneled HTTP request, and the probe stops at the handshake.
	sni := fmt.Sprintf("actoregress-%d.example.com", time.Now().UnixNano())

	result := probe(t, ctx, actor, probeapi.Request{
		Via:         probeapi.ViaDirect,
		Destination: probeDestination,
		SNI:         sni,
	})
	if !result.Connected {
		t.Fatalf("the actor's dial to %s never connected (%s). Under the redirect this connection is local to the worker "+
			"and should come up immediately; a failure here means nftables did not redirect it and the actor really tried "+
			"to reach TEST-NET-1", probeDestination, result.ConnectError)
	}
	if !result.HandshakeOK {
		t.Fatalf("the inner TLS handshake to %s (SNI %q) failed: %s. The tunnel opened, so this is the gateway refusing "+
			"after the CONNECT: either the policy for %s/%s no longer routes to MITM, or sdsmintd declined to mint",
			probeDestination, sni, result.HandshakeError, actor.Atespace, actor.Name)
	}

	chain := e2e.ParseCertChain(t, sni, result.ChainPEM)
	leaf := chain[0]
	opts := x509.VerifyOptions{
		DNSName:       sni,
		Roots:         e2e.CertPool(root),
		Intermediates: e2e.CertPool(chain[1:]...),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := leaf.Verify(opts); err != nil {
		t.Fatalf("the certificate the actor was served for %q does not chain to the %s/%s MITM root: %v. "+
			"The handshake succeeded, so something answered -- but it was not this cluster's egress gateway",
			sni, e2e.EgressNamespace, e2e.MITMCASecret, err)
	}
	t.Logf("actor %s/%s dialed %s and was served a %d-cert chain rooted at the MITM CA (leaf CN %q, serial %s): "+
		"its egress went through the gateway", actor.Atespace, actor.Name, probeDestination, len(chain),
		leaf.Subject.CommonName, leaf.SerialNumber.Text(16))
}

// TestActorEgressIsDeniedByPolicy is the other half: the same fixture, the same
// destination, a different actor, and no connection to the far side.
//
// It asserts less than its sibling, and cannot assert more. Egress.handle logs
// the 403 and closes the socket (internal/atunnel/egress.go), so the reason
// never reaches the actor -- a denial is indistinguishable from any other EOF
// from where the probe stands. Its value is entirely in the pairing: the only
// difference from the test above is which policy the certificate selects.
//
// The one thing it does assert on its own is that the connection came up. Under
// the redirect the socket is local to the worker and connects before atunnel
// has spoken to the gateway at all, so Connected distinguishes "the gateway
// refused" from "there was no redirect and the actor really dialed TEST-NET-1"
// -- the failure mode that would otherwise let this test pass for the wrong
// reason.
func TestActorEgressIsDeniedByPolicy(t *testing.T) {
	ctx := context.Background()

	actor := createProbeActor(t, ctx, deniedActor)
	sni := fmt.Sprintf("actoregress-%d.example.com", time.Now().UnixNano())

	result := probe(t, ctx, actor, probeapi.Request{
		Via:         probeapi.ViaDirect,
		Destination: probeDestination,
		SNI:         sni,
	})
	if !result.Connected {
		t.Fatalf("the actor's dial to %s never connected (%s). That is not a policy denial: a denied actor's socket still "+
			"comes up, because the redirect terminates it locally. This looks like the redirect being absent",
			probeDestination, result.ConnectError)
	}
	if result.HandshakeOK {
		chain := e2e.ParseCertChain(t, sni, result.ChainPEM)
		t.Fatalf("actor %s/%s completed a TLS handshake to %s (leaf CN %q) despite its DENY_ALL policy -- egress "+
			"authorization is not being enforced on the actor path",
			actor.Atespace, actor.Name, probeDestination, chain[0].Subject.CommonName)
	}
	if result.ChainPEM != "" {
		t.Errorf("the handshake failed but a chain came back for %q; expected nothing to have been served", sni)
	}
	t.Logf("actor %s/%s was cut off as expected: %s. The gateway's reason is not visible from here -- check the worker's "+
		"ateom container for \"atunnel failed to open egress tunnel\" carrying the 403",
		actor.Atespace, actor.Name, result.HandshakeError)
}

// probe asks the actor for one trip out, through the router the same way real
// traffic arrives.
//
// The router is retried, not the probe: after ResumeActor returns, the xDS
// update carrying the actor's route may not have reached the router yet and the
// first requests come back 503. A Result, once one arrives, is returned as-is --
// a refusal is one of the outcomes under test, not a failure to reach the probe.
func probe(t *testing.T, ctx context.Context, actor resources.ActorRef, req probeapi.Request) probeapi.Result {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encoding probe request: %v", err)
	}
	t.Logf("probe: actor=%s/%s via=%s destination=%q sni=%q", actor.Atespace, actor.Name, req.Via, req.Destination, req.SNI)

	router, err := e2e.NewRouterClient(ctx)
	if err != nil {
		t.Fatalf("NewRouterClient: %v", err)
	}
	defer router.Close()

	const timeout = 60 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		resp, err := router.PostJSON(ctx, actor, "/probe", body)
		if err != nil {
			t.Fatalf("POST /probe to %s through the router: %v", actor.DNSName(), err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("reading probe response (HTTP %d): %v", resp.StatusCode, readErr)
		}

		if resp.StatusCode == http.StatusOK {
			var out probeapi.Result
			if err := json.Unmarshal(payload, &out); err != nil {
				t.Fatalf("decoding probe response: %v; body: %s", err, payload)
			}
			return out
		}
		// 4xx is the probe rejecting the request itself -- a malformed Via, an
		// unparseable credential -- and retrying will not fix it.
		if resp.StatusCode < 500 {
			t.Fatalf("probe rejected the request with HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe unreachable through the router after %v: HTTP %d: %s", timeout, resp.StatusCode, strings.TrimSpace(string(payload)))
		}
		t.Logf("router returned HTTP %d; retrying...", resp.StatusCode)
		time.Sleep(1 * time.Second)
	}
}

// createProbeActor creates the named actor from the probe fixture and resumes
// it, returning a reference for routing. Cleanup suspends and deletes it.
//
// The name is fixed by the caller rather than generated, for the reason in the
// const block above. AlreadyExists is therefore tolerated: a previous run that
// died between CreateActor and its cleanup would otherwise leave the suite
// permanently broken, and the actor it left is the same actor from the same
// template. Nothing here is stateful enough for reuse to matter -- ResumeActor
// below puts it on a worker either way.
func createProbeActor(t *testing.T, ctx context.Context, name string) resources.ActorRef {
	t.Helper()
	clients := e2e.GetClients()
	actorRef := resources.ActorRef{Atespace: extproc.DemoAtespace, Name: name}
	objectRef := actorRef.ToObjectRef()

	// The atespace only has to exist; it carries no policy of its own. The
	// policy is keyed on its name, which is why it is the demo one.
	_, _ = clients.SubstrateAPI.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: extproc.DemoAtespace}},
	})

	t.Logf("creating actor %s/%s from %s/%s", actorRef.Atespace, actorRef.Name, probeTemplateNamespace, probeTemplateName)
	_, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: actorRef.Atespace, Name: actorRef.Name},
		ActorTemplateNamespace: probeTemplateNamespace,
		ActorTemplateName:      probeTemplateName,
	}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateActor %s/%s: %v (deploy the fixture with --deploy-demo-egress)", actorRef.Atespace, actorRef.Name, err)
	}
	t.Cleanup(func() {
		_, _ = clients.SubstrateAPI.SuspendActor(context.Background(), &ateapipb.SuspendActorRequest{Actor: objectRef})
		_, _ = clients.SubstrateAPI.DeleteActor(context.Background(), &ateapipb.DeleteActorRequest{Actor: objectRef})
	})

	if _, err := clients.SubstrateAPI.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: objectRef}); err != nil {
		t.Fatalf("ResumeActor %s/%s: %v", actorRef.Atespace, actorRef.Name, err)
	}
	t.Logf("resumed actor %s/%s", actorRef.Atespace, actorRef.Name)
	return actorRef
}
