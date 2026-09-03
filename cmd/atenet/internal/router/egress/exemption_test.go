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
	"crypto/x509"
	"errors"
	"slices"
	"strings"
	"testing"

	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestNewExemptionSetNormalizes(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{name: "nil", patterns: nil},
		{name: "all blank", patterns: []string{"", "   "}},
		{
			name:     "sorted and deduplicated",
			patterns: []string{"b.example.com", "a.example.com", "b.example.com"},
			want:     []string{"a.example.com", "b.example.com"},
		},
		{
			// The SNI arrives however the client chose to write it, so the
			// pattern has to be stored in one canonical form.
			name:     "case and trailing dot folded together",
			patterns: []string{"API.Example.COM.", "api.example.com"},
			want:     []string{"api.example.com"},
		},
		{
			name:     "wildcards kept as written",
			patterns: []string{"*.Example.com"},
			want:     []string{"*.example.com"},
		},
		{
			name:     "blanks dropped from a real list",
			patterns: []string{"api.example.com", "  ", "cdn.example.com"},
			want:     []string{"api.example.com", "cdn.example.com"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := NewExemptionSet(tc.patterns)
			if got := set.Patterns(); !slices.Equal(got, tc.want) {
				t.Errorf("Patterns() = %v, want %v", got, tc.want)
			}
			if got, want := set.IsEmpty(), len(tc.want) == 0; got != want {
				t.Errorf("IsEmpty() = %v, want %v", got, want)
			}
			if set.IsEmpty() != (set.ID() == "") {
				t.Errorf("ID() = %q for IsEmpty() = %v; the two must agree", set.ID(), set.IsEmpty())
			}
		})
	}
}

// The ID is what ties a connection to a rendered filter chain, so sets that
// mean the same thing must share one and sets that do not must not.
func TestExemptionSetIDDependsOnlyOnContents(t *testing.T) {
	base := NewExemptionSet([]string{"api.example.com", "*.cdn.example.com"})

	same := []struct {
		name     string
		patterns []string
	}{
		{name: "reordered", patterns: []string{"*.cdn.example.com", "api.example.com"}},
		{name: "duplicated", patterns: []string{"api.example.com", "*.cdn.example.com", "api.example.com"}},
		{name: "differently cased", patterns: []string{"API.example.com", "*.CDN.example.com"}},
	}
	for _, tc := range same {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewExemptionSet(tc.patterns).ID(); got != base.ID() {
				t.Errorf("ID() = %q, want %q", got, base.ID())
			}
		})
	}

	different := []struct {
		name     string
		patterns []string
	}{
		{name: "one pattern removed", patterns: []string{"api.example.com"}},
		{name: "one pattern added", patterns: []string{"api.example.com", "*.cdn.example.com", "www.example.com"}},
		// A wildcard and its exact form authorize different traffic, so they
		// must not collapse into one chain.
		{name: "wildcard instead of exact", patterns: []string{"*.example.com", "*.cdn.example.com"}},
	}
	for _, tc := range different {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewExemptionSet(tc.patterns).ID(); got == base.ID() {
				t.Errorf("ID() = %q, want something other than %q", got, base.ID())
			}
		})
	}
}

// The ID lands in an Envoy filter chain name and in filter state, both of which
// are compared literally.
func TestExemptionSetIDIsPlainHex(t *testing.T) {
	id := NewExemptionSet([]string{"api.example.com"}).ID()
	if len(id) != 32 {
		t.Errorf("len(ID()) = %d, want 32", len(id))
	}
	if strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("ID() = %q, want lowercase hex only", id)
	}
}

// Patterns must not hand out the set's own storage: a caller mutating what it
// gets back would change the set the ID was computed from.
func TestExemptionSetPatternsIsACopy(t *testing.T) {
	set := NewExemptionSet([]string{"api.example.com", "cdn.example.com"})
	set.Patterns()[0] = "evil.example.com"
	if got := set.Patterns()[0]; got != "api.example.com" {
		t.Errorf("Patterns()[0] = %q after mutating an earlier result, want %q", got, "api.example.com")
	}
}

// fakeRegistry records what the handler asks it to publish.
type fakeRegistry struct {
	registered []ExemptionSet
	err        error
}

func (r *fakeRegistry) Register(_ context.Context, set ExemptionSet) error {
	r.registered = append(r.registered, set)
	return r.err
}

// exemptionHandler builds a Handler whose actor is running and whose egress
// policy lookup returns policy/err.
func exemptionHandler(roots *x509.CertPool, registry ExemptionRegistry, policy *ateapipb.EgressPolicy, err error) *Handler {
	client := &egressMockClient{actor: runningActor(), policy: policy, policyErr: err}
	return New(client, roots, registry)
}

// exemptionSetID reads the ID the handler published back out of the dynamic
// metadata, which is the only channel the dataplane sees it on. An empty string
// means the connection will be intercepted.
func publishedExemptionSetID(t *testing.T, res extproc.Result) string {
	t.Helper()
	if res.DynamicMetadata == nil {
		return ""
	}
	namespace := res.DynamicMetadata.GetFields()[ExemptionMetadataNamespace].GetStructValue()
	if namespace == nil {
		t.Fatalf("dynamic metadata has no %q namespace: %v", ExemptionMetadataNamespace, res.DynamicMetadata)
	}
	return namespace.GetFields()[ExemptionMetadataKey].GetStringValue()
}

func TestHandleRequestHeadersPublishesTheExemptionSet(t *testing.T) {
	ca := newTestCA(t, "actor-identity-ca")
	leaf := ca.issueActorCert(t, actorCertOptions{})
	registry := &fakeRegistry{}
	policy := &ateapipb.EgressPolicy{TlsInterceptionExemptions: []string{"api.example.com", "*.cdn.example.com"}}
	h := exemptionHandler(ca.roots(), registry, policy, nil)

	res, err := h.HandleRequestHeaders(context.Background(), egressMetadata(xfccHeader(leaf)))
	if err != nil {
		t.Fatalf("HandleRequestHeaders() error = %v, want nil", err)
	}

	want := NewExemptionSet(policy.GetTlsInterceptionExemptions())
	if got := publishedExemptionSetID(t, res); got != want.ID() {
		t.Errorf("published exemption set = %q, want %q", got, want.ID())
	}
	// The gateway has to be configured for the set before it is named, or the
	// connection dispatches to a chain that does not exist.
	if len(registry.registered) != 1 {
		t.Fatalf("registered %d sets, want 1", len(registry.registered))
	}
	if got := registry.registered[0].ID(); got != want.ID() {
		t.Errorf("registered set = %q, want %q", got, want.ID())
	}
}

// Everything short of a healthy policy with a non-empty exemption list has to
// end in interception, and without failing the CONNECT.
func TestHandleRequestHeadersInterceptsWhenNoSetApplies(t *testing.T) {
	tests := []struct {
		name     string
		registry ExemptionRegistry
		policy   *ateapipb.EgressPolicy
		err      error
		// wantRegistered is whether the handler should have tried to publish.
		wantRegistered bool
	}{
		{
			// The common case: exemptions cost nothing when unused.
			name:     "policy exempts nothing",
			registry: &fakeRegistry{},
			policy:   &ateapipb.EgressPolicy{},
		},
		{
			name:     "actor has no policy",
			registry: &fakeRegistry{},
			err:      status.Error(codes.NotFound, "no egress policy"),
		},
		{
			// The set exists but the gateway never confirmed it, so naming it
			// would point at a filter chain that is not there.
			name:           "gateway did not acknowledge the set",
			registry:       &fakeRegistry{err: errors.New("no gateway subscribed")},
			policy:         &ateapipb.EgressPolicy{TlsInterceptionExemptions: []string{"api.example.com"}},
			wantRegistered: true,
		},
		{
			// Exemptions are off entirely.
			name:   "no registry configured",
			policy: &ateapipb.EgressPolicy{TlsInterceptionExemptions: []string{"api.example.com"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ca := newTestCA(t, "actor-identity-ca")
			leaf := ca.issueActorCert(t, actorCertOptions{})
			h := exemptionHandler(ca.roots(), tc.registry, tc.policy, tc.err)

			res, err := h.HandleRequestHeaders(context.Background(), egressMetadata(xfccHeader(leaf)))
			if err != nil {
				t.Fatalf("HandleRequestHeaders() error = %v, want nil", err)
			}
			if got := publishedExemptionSetID(t, res); got != "" {
				t.Errorf("published exemption set = %q, want none", got)
			}
			if fake, ok := tc.registry.(*fakeRegistry); ok {
				if got := len(fake.registered) > 0; got != tc.wantRegistered {
					t.Errorf("registered a set = %v, want %v", got, tc.wantRegistered)
				}
			}
		})
	}
}

// A policy we cannot read is not a policy that exempts nothing: it is a control
// plane we cannot reach, and the gateway already fails closed on those.
func TestHandleRequestHeadersFailsClosedOnPolicyLookupErrors(t *testing.T) {
	ca := newTestCA(t, "actor-identity-ca")
	leaf := ca.issueActorCert(t, actorCertOptions{})
	h := exemptionHandler(ca.roots(), &fakeRegistry{}, nil, status.Error(codes.Unavailable, "control plane down"))

	_, err := h.HandleRequestHeaders(context.Background(), egressMetadata(xfccHeader(leaf)))
	wantStatus(t, err, envoy_type.StatusCode_ServiceUnavailable)
}
