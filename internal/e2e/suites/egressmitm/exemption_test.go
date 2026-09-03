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

package egressmitm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// exemptOrigin is a second documentation origin, so this case cannot be
// satisfied by anything the interception assertions left behind for
// example.com. Both phases below hit it, and the probe builds a fresh client
// per /fetch, so neither reuses the other's tunnel.
const exemptOrigin = "https://www.example.com/"

// testTLSInterceptionExemption is the inverse of the trust assertions in
// TestActorEgressMITMTrust, and reads the same two fetches backwards:
//
//   - roots=system succeeds. Under interception it cannot: the gateway's
//     minted leaf chains to no public CA. A 200 means the actor completed TLS
//     with the origin's own certificate, which is only possible if the gateway
//     passed the connection through.
//   - roots=bundle fails with a certificate error, for the same reason in the
//     other direction: the projected bundle holds the gateway CA and no public
//     roots, so it cannot validate a real origin.
//
// Together they rule out the reading that would otherwise fit a lone success —
// that interception is simply off cluster-wide — because the assertions before
// this one proved it was on for the same actor moments earlier.
func testTLSInterceptionExemption(t *testing.T, ctx context.Context, clients *e2e.Clients, rc *e2e.RouterClient, id string) {
	ref := resources.ActorRef{Atespace: probeNamespace, Name: id}

	// Baseline first: without the policy this origin is intercepted like any
	// other. Skipping it would leave a passing test that proves nothing on a
	// cluster where the passthrough is the default.
	before := probeFetch(t, ctx, rc, id, exemptOrigin, "system")
	if before.Error == "" {
		t.Fatalf("fetch %s with system roots succeeded (status %s) before any exemption; the gateway is not intercepting, so this case cannot show that an exemption changed anything", exemptOrigin, before.Status)
	}

	setExemptions(t, ctx, clients, ref, "www.example.com")

	// The exemption reaches the gateway on the next CONNECT, but only after
	// atenet has published the set and Envoy has acknowledged it. Until then
	// ext_proc deliberately leaves the connection intercepted, so retry across
	// one push rather than reading the first fetch as the answer.
	deadline := time.Now().Add(60 * time.Second)
	var exempt fetchResponse
	for {
		exempt = probeFetch(t, ctx, rc, id, exemptOrigin, "system")
		if exempt.Error == "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if exempt.Error != "" {
		t.Fatalf("fetch %s with system roots still failed after exempting www.example.com: %s — the connection is still being intercepted", exemptOrigin, exempt.Error)
	}
	if exempt.Status != "200" {
		t.Errorf("fetch %s with system roots: status %s, want 200", exemptOrigin, exempt.Status)
	}

	if bundle := probeFetch(t, ctx, rc, id, exemptOrigin, "bundle"); bundle.Error == "" {
		t.Errorf("fetch %s with the projected bundle unexpectedly succeeded (status %s): an exempted connection presents the origin's own certificate, which the gateway CA cannot validate", exemptOrigin, bundle.Status)
	} else if !strings.Contains(bundle.Error, "certificate") && !strings.Contains(bundle.Error, "x509") {
		t.Errorf("fetch %s with the projected bundle failed, but not with a certificate-verification error: %s", exemptOrigin, bundle.Error)
	}
}

// setExemptions gives the actor an egress policy exempting patterns, creating
// it or replacing whatever is already there. The policy carries no rules: the
// gateway does not evaluate them yet, and an exemption is not an authorization
// in any case.
func setExemptions(t *testing.T, ctx context.Context, clients *e2e.Clients, ref resources.ActorRef, patterns ...string) {
	t.Helper()
	actor := &ateapipb.ObjectRef{Atespace: ref.Atespace, Name: ref.Name}
	policy := &ateapipb.EgressPolicy{
		Metadata:                  &ateapipb.ResourceMetadata{Atespace: ref.Atespace, Name: "default"},
		TlsInterceptionExemptions: patterns,
	}

	existing, err := clients.SubstrateAPI.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: actor})
	if err != nil {
		if _, err := clients.SubstrateAPI.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
			Actor:        actor,
			EgressPolicy: policy,
		}); err != nil {
			t.Fatalf("CreateActorEgressPolicy for %q: %v", ref.Name, err)
		}
		return
	}

	// An update is a full replacement and needs the current UID and version as
	// preconditions.
	policy.Metadata.Uid = existing.GetMetadata().GetUid()
	policy.Metadata.Version = existing.GetMetadata().GetVersion()
	if _, err := clients.SubstrateAPI.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{
		Actor:        actor,
		EgressPolicy: policy,
	}); err != nil {
		t.Fatalf("UpdateActorEgressPolicy for %q: %v", ref.Name, err)
	}
}
