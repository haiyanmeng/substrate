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

package egress

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// testActorSPIFFEURI is the URI SAN the actor-identity CA mints for
// testEgressActor, and so the value the CONNECT leg publishes as filter state.
const testActorSPIFFEURI = "spiffe://" + actorSPIFFETrustDomain + "/atespace/" + testEgressAtespace + "/actor/" + testEgressActor

// egressPolicyMockClient answers the one RPC the MITM handler makes. Every
// other method is nil and would panic, which is the point: a handler that
// reaches for anything else is asking a question it has no business asking on
// this leg.
type egressPolicyMockClient struct {
	ateapipb.ControlClient
	policy *ateapipb.EgressPolicy
	err    error

	// requested records the actor the handler asked about, so a test can check
	// it looked up the certificate's actor rather than one the request named.
	requested *ateapipb.ObjectRef
}

func (m *egressPolicyMockClient) GetActorEgressPolicy(_ context.Context, req *ateapipb.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	m.requested = req.GetActor()
	if m.err != nil {
		return nil, m.err
	}
	return m.policy, nil
}

// mitmMetadata is a request on the decrypted leg: an authority from inside the
// tunnel, plus the two filter-state values the CONNECT leg published.
func mitmMetadata(authority, identity, connectDestination string) *extproc.RequestMetadata {
	fields := map[string]*structpb.Value{}
	if identity != "" {
		fields[extproc.ActorIdentityFilterStateAttribute] = structpb.NewStringValue(identity)
	}
	if connectDestination != "" {
		fields[extproc.EgressDestinationFilterStateAttribute] = structpb.NewStringValue(connectDestination)
	}
	return extproc.NewRequestMetadata([]*corev3.HeaderValue{
		{Key: ":method", RawValue: []byte("GET")},
		{Key: ":path", RawValue: []byte("/v1/models")},
		{Key: ":authority", RawValue: []byte(authority)},
	}, map[string]*structpb.Struct{
		"envoy.filters.http.ext_proc": {Fields: fields},
	})
}

// allowPolicy is a policy permitting exactly the named hostname patterns.
func allowPolicy(patterns ...string) *ateapipb.EgressPolicy {
	return &ateapipb.EgressPolicy{
		Rules: []*ateapipb.EgressRule{
			{Hostnames: &ateapipb.HostnameRule{Patterns: patterns}},
		},
	}
}

func TestMITMAllowsAPermittedDestination(t *testing.T) {
	client := &egressPolicyMockClient{policy: allowPolicy("api.example.com")}
	h := NewMITM(client)

	res, err := h.HandleRequestHeaders(context.Background(),
		mitmMetadata("api.example.com:443", testActorSPIFFEURI, "93.184.216.34:443"))
	if err != nil {
		t.Fatalf("HandleRequestHeaders: %v", err)
	}
	if res.Response == nil {
		t.Error("allowed request produced no headers response, so Envoy has nothing to continue on")
	}

	// The policy fetched must be the certificate's actor. Any other answer
	// means the handler trusted something from inside the tunnel.
	if got := client.requested.GetName(); got != testEgressActor {
		t.Errorf("looked up the policy of actor %q, want %q", got, testEgressActor)
	}
	if got := client.requested.GetAtespace(); got != testEgressAtespace {
		t.Errorf("looked up the policy in atespace %q, want %q", got, testEgressAtespace)
	}
}

// TestMITMPoliciesEveryRequest is the difference between this filter and the
// CONNECT-leg check: the tunnel is already open and already authenticated, and
// the policy still decides each request that comes out of it.
func TestMITMPoliciesEveryRequest(t *testing.T) {
	client := &egressPolicyMockClient{policy: allowPolicy("api.example.com")}
	h := NewMITM(client)

	if _, err := h.HandleRequestHeaders(context.Background(),
		mitmMetadata("api.example.com:443", testActorSPIFFEURI, "93.184.216.34:443")); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// The policy changes underneath an open tunnel; the next request out of it
	// is decided by the new one.
	client.policy = allowPolicy("other.example.com")
	_, err := h.HandleRequestHeaders(context.Background(),
		mitmMetadata("api.example.com:443", testActorSPIFFEURI, "93.184.216.34:443"))
	wantStatus(t, err, envoy_type.StatusCode_Forbidden)
}

func TestMITMDenies(t *testing.T) {
	tests := []struct {
		name               string
		authority          string
		identity           string
		connectDestination string
		policy             *ateapipb.EgressPolicy
		err                error
		want               envoy_type.StatusCode
	}{
		{
			name:               "a hostname no rule matches",
			authority:          "evil.test:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			policy:             allowPolicy("api.example.com"),
			want:               envoy_type.StatusCode_Forbidden,
		},
		{
			// A policy resource that exists but grants nothing is the same
			// answer as no policy at all.
			name:               "a policy with no rules",
			authority:          "api.example.com:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			policy:             &ateapipb.EgressPolicy{},
			want:               envoy_type.StatusCode_Forbidden,
		},
		{
			// Fail closed: an actor nobody has granted anything has not been
			// granted everything.
			name:               "an actor with no egress policy at all",
			authority:          "api.example.com:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			err:                status.Error(codes.NotFound, "no egress policy"),
			want:               envoy_type.StatusCode_Forbidden,
		},
		{
			// The gateway cannot know whether this was permitted, and an answer
			// it cannot get is not "yes". 503 rather than 403 because a retry
			// may well succeed.
			name:               "an unreachable control plane",
			authority:          "api.example.com:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			err:                status.Error(codes.Unavailable, "connection refused"),
			want:               envoy_type.StatusCode_ServiceUnavailable,
		},
		{
			name:               "a control plane that timed out",
			authority:          "api.example.com:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			err:                status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			want:               envoy_type.StatusCode_ServiceUnavailable,
		},
		{
			name:               "a policy lookup that failed for any other reason",
			authority:          "api.example.com:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			err:                status.Error(codes.PermissionDenied, "nope"),
			want:               envoy_type.StatusCode_Forbidden,
		},
		{
			// A misconfigured gateway, not a misbehaving actor — but the
			// traffic is unauthorizable either way.
			name:               "an identity that did not survive the hop",
			authority:          "api.example.com:443",
			connectDestination: "93.184.216.34:443",
			policy:             allowPolicy("api.example.com"),
			want:               envoy_type.StatusCode_Forbidden,
		},
		{
			name:               "an identity from another trust domain",
			authority:          "api.example.com:443",
			identity:           "spiffe://cluster.local/ns/default/sa/actor",
			connectDestination: "93.184.216.34:443",
			policy:             allowPolicy("api.example.com"),
			want:               envoy_type.StatusCode_Forbidden,
		},
		{
			// Nothing to police: no name inside the tunnel and no address
			// carried in from the CONNECT.
			name:      "a request with neither a hostname nor a CONNECT address",
			authority: "93.184.216.34:443",
			identity:  testActorSPIFFEURI,
			policy:    &ateapipb.EgressPolicy{Rules: []*ateapipb.EgressRule{{All: &emptypb.Empty{}}}},
			want:      envoy_type.StatusCode_Forbidden,
		},
		{
			name:               "a malformed authority",
			authority:          "not a host:443",
			identity:           testActorSPIFFEURI,
			connectDestination: "93.184.216.34:443",
			policy:             &ateapipb.EgressPolicy{Rules: []*ateapipb.EgressRule{{All: &emptypb.Empty{}}}},
			want:               envoy_type.StatusCode_Forbidden,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewMITM(&egressPolicyMockClient{policy: tc.policy, err: tc.err})
			_, err := h.HandleRequestHeaders(context.Background(),
				mitmMetadata(tc.authority, tc.identity, tc.connectDestination))
			wantStatus(t, err, tc.want)
		})
	}
}

// TestMITMDenialsDoNotDiscloseThePolicy checks that a refusal tells the actor
// what it asked for and nothing about what anyone was granted. Which rules
// exist is the operator's business, and an actor that can enumerate them by
// probing has been handed a map of the network.
func TestMITMDenialsDoNotDiscloseThePolicy(t *testing.T) {
	client := &egressPolicyMockClient{policy: allowPolicy("secret-internal.example.com", "*.corp.example.com")}
	h := NewMITM(client)

	_, err := h.HandleRequestHeaders(context.Background(),
		mitmMetadata("evil.test:443", testActorSPIFFEURI, "93.184.216.34:443"))
	wantStatus(t, err, envoy_type.StatusCode_Forbidden)

	var re *extproc.ReqError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not a *extproc.ReqError", err)
	}
	for _, secret := range []string{"secret-internal.example.com", "corp.example.com"} {
		if got := re.Error(); strings.Contains(got, secret) {
			t.Errorf("denial message %q names %q from the policy", got, secret)
		}
	}
}

// TestMITMAllowsWhenTheMatchedRuleHasUnappliedEffects pins the decision that a
// rule this gateway cannot fully carry out still authorizes. The rule is what
// grants access; inject_static_headers is an add-on, and no credential provider
// exists to resolve its substrate-secret:// URIs yet. Denying instead would
// break egress the operator did grant.
func TestMITMAllowsWhenTheMatchedRuleHasUnappliedEffects(t *testing.T) {
	client := &egressPolicyMockClient{policy: &ateapipb.EgressPolicy{
		Rules: []*ateapipb.EgressRule{{
			Hostnames: &ateapipb.HostnameRule{
				Patterns: []string{"api.example.com"},
				Effects: &ateapipb.EgressRuleEffects{
					InjectStaticHeaders: []*ateapipb.CredentialHeaderInjection{
						{Header: "authorization", Prefix: "Bearer ", CredentialUri: "substrate-secret://kubernetes/openai/key"},
					},
				},
			},
		}},
	}}
	h := NewMITM(client)

	if _, err := h.HandleRequestHeaders(context.Background(),
		mitmMetadata("api.example.com:443", testActorSPIFFEURI, "93.184.216.34:443")); err != nil {
		t.Fatalf("HandleRequestHeaders: %v", err)
	}
}

// TestMITMMatchesTheConnectAddress covers the reason the CONNECT leg publishes
// its authority at all: an IPBlockRule is written against the address the
// actor's kernel dialed, which is nowhere in the request that comes out of the
// tunnel.
func TestMITMMatchesTheConnectAddress(t *testing.T) {
	policy := &ateapipb.EgressPolicy{
		Rules: []*ateapipb.EgressRule{
			{IpBlocks: &ateapipb.IPBlockRule{Cidrs: []string{"93.184.216.0/24"}}},
		},
	}

	t.Run("inside the block", func(t *testing.T) {
		h := NewMITM(&egressPolicyMockClient{policy: policy})
		if _, err := h.HandleRequestHeaders(context.Background(),
			mitmMetadata("api.example.com:443", testActorSPIFFEURI, "93.184.216.34:443")); err != nil {
			t.Fatalf("HandleRequestHeaders: %v", err)
		}
	})

	t.Run("outside the block", func(t *testing.T) {
		h := NewMITM(&egressPolicyMockClient{policy: policy})
		_, err := h.HandleRequestHeaders(context.Background(),
			mitmMetadata("api.example.com:443", testActorSPIFFEURI, "203.0.113.9:443"))
		wantStatus(t, err, envoy_type.StatusCode_Forbidden)
	})

	// Without the address, an IP rule has nothing to match — so the request is
	// denied rather than let through on the hostname the actor supplied.
	t.Run("no address carried", func(t *testing.T) {
		h := NewMITM(&egressPolicyMockClient{policy: policy})
		_, err := h.HandleRequestHeaders(context.Background(),
			mitmMetadata("api.example.com:443", testActorSPIFFEURI, ""))
		wantStatus(t, err, envoy_type.StatusCode_Forbidden)
	})
}

func TestParseActorSPIFFEURI(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		atespace string
		actor    string
		wantErr  bool
	}{
		{name: "the SAN the CA mints", raw: testActorSPIFFEURI, atespace: testEgressAtespace, actor: testEgressActor},
		{
			name:     "another atespace",
			raw:      "spiffe://substrate-actor.local/atespace/team-a/actor/agent-7",
			atespace: "team-a",
			actor:    "agent-7",
		},

		{name: "empty", raw: "", wantErr: true},
		// Envoy joins multiple SANs with a comma; one actor certificate has
		// exactly one, so this is a certificate naming no single actor.
		{name: "two SANs", raw: testActorSPIFFEURI + "," + testActorSPIFFEURI, wantErr: true},
		{name: "not a SPIFFE ID", raw: "https://substrate-actor.local/atespace/default/actor/my-actor", wantErr: true},
		{name: "another trust domain", raw: "spiffe://cluster.local/atespace/default/actor/my-actor", wantErr: true},
		{name: "a workload ID of another shape", raw: "spiffe://substrate-actor.local/ns/default/sa/my-actor", wantErr: true},
		{name: "a truncated path", raw: "spiffe://substrate-actor.local/atespace/default", wantErr: true},
		{name: "a path with extra segments", raw: "spiffe://substrate-actor.local/atespace/default/actor/my-actor/x", wantErr: true},
		{name: "no path at all", raw: "spiffe://substrate-actor.local", wantErr: true},
		// A CA that minted this is compromised or fed from something other than
		// control-plane state, and either way the name is not one to look up.
		{name: "an atespace that is not a resource name", raw: "spiffe://substrate-actor.local/atespace/..%2f/actor/my-actor", wantErr: true},
		{name: "an actor that is not a resource name", raw: "spiffe://substrate-actor.local/atespace/default/actor/UPPER", wantErr: true},
		{name: "an empty actor name", raw: "spiffe://substrate-actor.local/atespace/default/actor/", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := parseActorSPIFFEURI(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseActorSPIFFEURI(%q) = %v, want an error", tc.raw, ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseActorSPIFFEURI(%q): %v", tc.raw, err)
			}
			if ref.Atespace != tc.atespace || ref.Name != tc.actor {
				t.Errorf("parseActorSPIFFEURI(%q) = %s/%s, want %s/%s", tc.raw, ref.Atespace, ref.Name, tc.atespace, tc.actor)
			}
		})
	}
}
