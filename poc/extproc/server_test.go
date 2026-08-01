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
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeStream drives Process without a network, replaying a fixed script of
// ProcessingRequests and capturing every response.
type fakeStream struct {
	grpc.ServerStream
	in   []*extprocv3.ProcessingRequest
	out  []*extprocv3.ProcessingResponse
	next int
}

func (f *fakeStream) Context() context.Context { return context.Background() }
func (f *fakeStream) SetHeader(metadata.MD) error {
	return nil
}
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}

func (f *fakeStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if f.next >= len(f.in) {
		return nil, io.EOF
	}
	req := f.in[f.next]
	f.next++
	return req, nil
}

func (f *fakeStream) Send(resp *extprocv3.ProcessingResponse) error {
	f.out = append(f.out, resp)
	return nil
}

// headersRequest builds a RequestHeaders message from "name: value" strings.
//
// The split skips a leading colon so pseudo-headers work: cutting ":authority:
// 9.9.9.9:443" on its first colon yields an empty name and swallows the value.
func headersRequest(headers ...string) *extprocv3.ProcessingRequest {
	hs := make([]*corev3.HeaderValue, 0, len(headers))
	for _, h := range headers {
		lead := strings.HasPrefix(h, ":")
		name, value, _ := strings.Cut(strings.TrimPrefix(h, ":"), ":")
		if lead {
			name = ":" + name
		}
		hs = append(hs, &corev3.HeaderValue{
			Key: strings.TrimSpace(name),
			// RawValue rather than Value: this is what Envoy 1.37 sends, and a
			// filter that only reads Value sees empty strings against it.
			RawValue: []byte(strings.TrimSpace(value)),
		})
	}
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: hs},
			},
		},
	}
}

// run replays one request through a server and returns the single response.
func run(t *testing.T, cp Checkpoint, store *Store, req *extprocv3.ProcessingRequest) *extprocv3.ProcessingResponse {
	t.Helper()
	stream := &fakeStream{in: []*extprocv3.ProcessingRequest{req}}
	if err := NewServer(cp, store, NewStats()).Process(stream); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(stream.out) != 1 {
		t.Fatalf("got %d responses, want 1", len(stream.out))
	}
	return stream.out[0]
}

func hardcodedStore() *Store { return NewStore(HardcodedSnapshot()) }

// immediateStatus returns the status of an ImmediateResponse, or 0 if the
// response was a continue.
func immediateStatus(resp *extprocv3.ProcessingResponse) typev3.StatusCode {
	ir, ok := resp.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
	if !ok {
		return 0
	}
	return ir.ImmediateResponse.GetStatus().GetCode()
}

// setHeaders flattens a continue response's SetHeaders into a map.
func setHeaders(t *testing.T, resp *extprocv3.ProcessingResponse) map[string]string {
	t.Helper()
	rh, ok := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("response is %T, want RequestHeaders", resp.Response)
	}
	out := map[string]string{}
	for _, h := range rh.RequestHeaders.GetResponse().GetHeaderMutation().GetSetHeaders() {
		out[strings.ToLower(h.GetHeader().GetKey())] = string(h.GetHeader().GetRawValue())
	}
	return out
}

func removedHeaders(t *testing.T, resp *extprocv3.ProcessingResponse) []string {
	t.Helper()
	rh, ok := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("response is %T, want RequestHeaders", resp.Response)
	}
	return rh.RequestHeaders.GetResponse().GetHeaderMutation().GetRemoveHeaders()
}

func TestConnectCheckpointModes(t *testing.T) {
	store := hardcodedStore()

	for _, tc := range []struct {
		name      string
		actor     string
		authority string
		wantDeny  bool
		wantMode  Mode
	}{
		{name: "DENY_ALL is denied", actor: "quarantined", authority: "127.0.0.1:19602", wantDeny: true},
		{name: "ALLOW_ALL passes through", actor: "wide-open", authority: "9.9.9.9:443", wantMode: ModePassthrough},
		{name: "in-block destination passes through", actor: "metrics-shipper", authority: "1.2.3.9:443", wantMode: ModePassthrough},
		{name: "out-of-block destination is denied", actor: "metrics-shipper", authority: "9.9.9.9:443", wantDeny: true},
		{name: "hostname policy goes to mitm", actor: "repo-reader", authority: "140.82.121.4:443", wantMode: ModeMITM},
		{name: "inject policy goes to mitm", actor: "invoice-agent", authority: "1.1.1.1:443", wantMode: ModeMITM},
		{name: "unknown actor is denied", actor: "stranger", authority: "9.9.9.9:443", wantDeny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, CheckpointConnect, store, headersRequest(
				":method: CONNECT",
				":authority: "+tc.authority,
				"x-ate-atespace: "+DemoAtespace,
				"x-ate-actor-name: "+tc.actor,
				"x-ate-actor-version: 7",
			))

			if tc.wantDeny {
				if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
					t.Fatalf("status = %v, want 403", got)
				}
				return
			}
			if got := immediateStatus(resp); got != 0 {
				t.Fatalf("request was denied with %v, want mode %q", got, tc.wantMode)
			}
			if got := setHeaders(t, resp)[EgressModeHeader]; got != string(tc.wantMode) {
				t.Errorf("%s = %q, want %q", EgressModeHeader, got, tc.wantMode)
			}
		})
	}
}

// A CONNECT with no actor name has nothing to look up. It must deny rather than
// fall back to any default.
func TestConnectCheckpointMissingIdentityDenies(t *testing.T) {
	store := hardcodedStore()

	for _, tc := range []struct {
		name    string
		headers []string
	}{
		{"no actor name", []string{":method: CONNECT", ":authority: 9.9.9.9:443", "x-ate-atespace: " + DemoAtespace}},
		{"no atespace", []string{":method: CONNECT", ":authority: 9.9.9.9:443", "x-ate-actor-name: wide-open"}},
		{"no metadata at all", []string{":method: CONNECT", ":authority: 9.9.9.9:443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, CheckpointConnect, store, headersRequest(tc.headers...))
			if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
				t.Errorf("status = %v, want 403", got)
			}
		})
	}
}

// clear_route_cache is what makes the mode header affect routing at all. Without
// it Envoy has already picked a route, and the request silently takes the
// fallback with a 200. Measured against Envoy 1.37; see egress-authn.md.
func TestConnectAllowClearsTheRouteCache(t *testing.T) {
	resp := run(t, CheckpointConnect, hardcodedStore(), headersRequest(
		":method: CONNECT",
		":authority: 9.9.9.9:443",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: wide-open",
	))
	rh := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	if !rh.RequestHeaders.GetResponse().GetClearRouteCache() {
		t.Error("allow response does not set clear_route_cache; the mode header would be inert")
	}
}

// An actor that sets x-ate-egress-mode on its own CONNECT must not be able to
// pick its route. The mutation has to overwrite, not append.
func TestConnectOverwritesAForgedModeHeader(t *testing.T) {
	resp := run(t, CheckpointConnect, hardcodedStore(), headersRequest(
		":method: CONNECT",
		":authority: 1.1.1.1:443",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: repo-reader",
		// repo-reader must go to mitm; it is asking for passthrough to skip the
		// hostname check entirely.
		EgressModeHeader+": passthrough",
	))

	rh := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	var found bool
	for _, h := range rh.RequestHeaders.GetResponse().GetHeaderMutation().GetSetHeaders() {
		if !strings.EqualFold(h.GetHeader().GetKey(), EgressModeHeader) {
			continue
		}
		found = true
		if got := string(h.GetHeader().GetRawValue()); got != string(ModeMITM) {
			t.Errorf("%s = %q, want %q", EgressModeHeader, got, ModeMITM)
		}
		if h.GetAppendAction() != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
			t.Errorf("append action = %v, want OVERWRITE_IF_EXISTS_OR_ADD; a forged value would survive alongside ours",
				h.GetAppendAction())
		}
	}
	if !found {
		t.Fatalf("response did not set %s", EgressModeHeader)
	}
}

// The connect checkpoint mounted on a non-CONNECT chain would apply
// CONNECT-shaped policy to ordinary requests, whose :authority is a hostname
// rather than the destination IP.
func TestConnectCheckpointRejectsNonConnect(t *testing.T) {
	resp := run(t, CheckpointConnect, hardcodedStore(), headersRequest(
		":method: GET",
		":authority: github.com",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: wide-open",
	))
	if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
		t.Errorf("status = %v, want 403", got)
	}
}

func TestInnerCheckpointHostnameAllowlist(t *testing.T) {
	store := hardcodedStore()

	for _, tc := range []struct {
		host      string
		wantAllow bool
	}{
		{"github.com", true},
		{"microsoft.com", true},
		{"my-app.my-company.com", true},
		{"evil.example", false},
		{"evil-github.com", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			resp := run(t, CheckpointInner, store, headersRequest(
				":method: GET",
				":authority: "+tc.host,
				ActorKeyHeader+": "+DemoAtespace+"/repo-reader",
			))
			denied := immediateStatus(resp) == typev3.StatusCode_Forbidden
			if denied == tc.wantAllow {
				t.Errorf("host %q: denied=%v, wantAllow=%v", tc.host, denied, tc.wantAllow)
			}
		})
	}
}

func TestInnerCheckpointInjectsCredentials(t *testing.T) {
	resp := run(t, CheckpointInner, hardcodedStore(), headersRequest(
		":method: GET",
		":authority: api.stripe.com",
		ActorKeyHeader+": "+DemoAtespace+"/invoice-agent",
		// Whatever the actor sent must not survive.
		"authorization: Bearer actor-supplied-value",
	))

	if got := immediateStatus(resp); got != 0 {
		t.Fatalf("api.stripe.com was denied with %v", got)
	}

	set := setHeaders(t, resp)
	if got := set["token"]; got != "X" {
		t.Errorf("token = %q, want %q", got, "X")
	}

	removed := removedHeaders(t, resp)
	if !slices.Contains(removed, "authorization") {
		t.Errorf("remove_headers = %v, want it to drop the actor's authorization header", removed)
	}
	// Removing the target too, before setting it, is what stops an actor from
	// supplying its own "token" header alongside ours.
	if !slices.Contains(removed, "token") {
		t.Errorf("remove_headers = %v, want it to drop any actor-supplied token header", removed)
	}
}

// The X-Ate-* metadata and the gateway's own routing headers are internal. They
// must not reach the real destination.
func TestInnerCheckpointStripsInternalHeaders(t *testing.T) {
	resp := run(t, CheckpointInner, hardcodedStore(), headersRequest(
		":method: GET",
		":authority: github.com",
		ActorKeyHeader+": "+DemoAtespace+"/repo-reader",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: repo-reader",
		"x-ate-actor-version: 7",
		EgressModeHeader+": mitm",
	))

	removed := removedHeaders(t, resp)
	for _, want := range []string{
		"x-ate-atespace", "x-ate-actor-name", "x-ate-actor-version",
		EgressModeHeader, ActorKeyHeader,
	} {
		if !slices.Contains(removed, want) {
			t.Errorf("remove_headers = %v, missing %q", removed, want)
		}
	}
}

// Filter state is the only sound source, so it must win even when the tunnel
// body carries a conflicting claim. An actor that reaches api.stripe.com by
// setting x-ate-actor-key would collect invoice-agent's injected credential.
func TestInnerCheckpointPrefersFilterState(t *testing.T) {
	req := headersRequest(
		":method: GET",
		":authority: api.stripe.com",
		// The actor claims to be invoice-agent inside its own tunnel.
		ActorKeyHeader+": "+DemoAtespace+"/invoice-agent",
		"x-ate-actor-name: invoice-agent",
		"x-ate-atespace: "+DemoAtespace,
	)
	req.Attributes = map[string]*structpb.Struct{
		"envoy.filters.http.ext_proc": {
			Fields: map[string]*structpb.Value{
				// What the gateway actually established at the CONNECT.
				filterStateActorKey: structpb.NewStringValue(DemoAtespace + "/repo-reader"),
			},
		},
	}

	resp := run(t, CheckpointInner, hardcodedStore(), req)
	if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
		t.Fatalf("status = %v, want 403: filter state says repo-reader, which cannot reach api.stripe.com", got)
	}
}

// An inner request with no identity from any source has nothing to authorize.
func TestInnerCheckpointNoIdentityDenies(t *testing.T) {
	resp := run(t, CheckpointInner, hardcodedStore(), headersRequest(
		":method: GET",
		":authority: github.com",
	))
	if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
		t.Errorf("status = %v, want 403", got)
	}
}

// A replica that has not loaded a policy table must deny at both checkpoints,
// not fall open. Its readiness probe is already failing, but Envoy may still
// have an open stream to it.
func TestNoSnapshotDeniesEverywhere(t *testing.T) {
	for _, cp := range []Checkpoint{CheckpointConnect, CheckpointInner} {
		t.Run(string(cp), func(t *testing.T) {
			resp := run(t, cp, &Store{}, headersRequest(
				":method: CONNECT",
				":authority: 9.9.9.9:443",
				"x-ate-atespace: "+DemoAtespace,
				"x-ate-actor-name: wide-open",
			))
			if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
				t.Errorf("status = %v, want 403", got)
			}
		})
	}
}

// A swap must take effect on the next request with no restart and no stream
// churn: the same server object, still serving, changes its answer.
func TestPolicySwapTakesEffectWithoutRestart(t *testing.T) {
	key := ActorKey{Atespace: DemoAtespace, Name: "swapper"}
	allow, err := NewSnapshot(1, map[ActorKey]Policy{key: {Kind: KindAllowAll}})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	deny, err := NewSnapshot(2, map[ActorKey]Policy{key: {Kind: KindDenyAll}})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	store := NewStore(allow)
	srv := NewServer(CheckpointConnect, store, NewStats())
	req := headersRequest(
		":method: CONNECT",
		":authority: 9.9.9.9:443",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: swapper",
	)

	first := &fakeStream{in: []*extprocv3.ProcessingRequest{req}}
	if err := srv.Process(first); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := immediateStatus(first.out[0]); got != 0 {
		t.Fatalf("before the swap: denied with %v, want allow", got)
	}

	store.Swap(deny)

	second := &fakeStream{in: []*extprocv3.ProcessingRequest{req}}
	if err := srv.Process(second); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := immediateStatus(second.out[0]); got != typev3.StatusCode_Forbidden {
		t.Errorf("after the swap: status = %v, want 403", got)
	}
}

// Envoy writes header values into RawValue on 1.37 and into Value on older
// builds. Reading only one silently sees empty strings, which here would mean
// every actor looks anonymous and every request is denied.
func TestHeadersAreReadFromEitherValueField(t *testing.T) {
	req := &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: ":method", Value: "CONNECT"},
					{Key: ":authority", RawValue: []byte("9.9.9.9:443")},
					{Key: "X-Ate-Atespace", Value: DemoAtespace},
					{Key: "X-Ate-Actor-Name", RawValue: []byte("wide-open")},
				}},
			},
		},
	}

	resp := run(t, CheckpointConnect, hardcodedStore(), req)
	if got := immediateStatus(resp); got != 0 {
		t.Fatalf("denied with %v; a header field was not read", got)
	}
	if got := setHeaders(t, resp)[EgressModeHeader]; got != string(ModePassthrough) {
		t.Errorf("%s = %q, want passthrough", EgressModeHeader, got)
	}
}

// %RESPONSE_CODE_DETAILS% is emitted into access logs as a bare token, so the
// details field must not contain whitespace.
func TestDenyDetailsAreLogSafe(t *testing.T) {
	resp := run(t, CheckpointConnect, hardcodedStore(), headersRequest(
		":method: CONNECT",
		":authority: 9.9.9.9:443",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: quarantined",
	))
	ir := resp.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
	details := ir.ImmediateResponse.GetDetails()
	if details == "" {
		t.Fatal("deny response carries no details")
	}
	if strings.ContainsAny(details, " \t\r\n") {
		t.Errorf("details %q contains whitespace", details)
	}
}
