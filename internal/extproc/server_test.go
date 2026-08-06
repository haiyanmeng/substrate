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
func run(t *testing.T, cp Checkpoint, store *Store, req *extprocv3.ProcessingRequest, opts ...ServerOption) *extprocv3.ProcessingResponse {
	t.Helper()
	stream := &fakeStream{in: []*extprocv3.ProcessingRequest{req}}
	if err := NewServer(cp, store, NewStats(), opts...).Process(stream); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(stream.out) != 1 {
		t.Fatalf("got %d responses, want 1", len(stream.out))
	}
	return stream.out[0]
}

// connectRequest builds the CONNECT the gateway sees: a destination, and an
// identity carried only by the peer certificate Envoy authenticated.
func connectRequest(t *testing.T, actor, authority string, extra ...string) *extprocv3.ProcessingRequest {
	t.Helper()
	headers := append([]string{
		":method: CONNECT",
		":authority: " + authority,
		XFCCHeader + ": " + xfccFor(t, DemoAtespace, actor),
	}, extra...)
	return headersRequest(headers...)
}

// withFilterStateActor attaches the identity the CONNECT checkpoint established,
// which is the only source the inner checkpoint honours.
func withFilterStateActor(req *extprocv3.ProcessingRequest, actor string) *extprocv3.ProcessingRequest {
	req.Attributes = map[string]*structpb.Struct{
		"envoy.filters.http.ext_proc": {
			Fields: map[string]*structpb.Value{
				filterStateActorKey: structpb.NewStringValue(DemoAtespace + "/" + actor),
			},
		},
	}
	return req
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
			// WithInnerCheckpoint because this table is about what the policy
			// decides, not about what this deployment can carry out. The gateway
			// ships without the inner checkpoint, and
			// TestConnectDeniesMITMWithoutTheInnerCheckpoint covers that.
			resp := run(t, CheckpointConnect, store,
				connectRequest(t, tc.actor, tc.authority), WithInnerCheckpoint())

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

// A CONNECT the gateway cannot attribute to an actor has nothing to look up. It
// must deny rather than fall back to any default.
func TestConnectCheckpointMissingIdentityDenies(t *testing.T) {
	store := hardcodedStore()

	for _, tc := range []struct {
		name string
		xfcc string
	}{
		// The listener is misconfigured -- set_current_client_cert_details is not
		// forwarding the certificate. Denying is the only safe reading.
		{"no XFCC header at all", ""},
		{"XFCC without a Cert field", `By=spiffe://x;Hash=abc;Subject="CN=worker"`},
		{"Cert is not a certificate", `Cert="not-a-certificate"`},
		// A valid podidentity certificate with no ActorIdentity: the e2e
		// egressprobe, and any other pod tooling. No identity, so no policy.
		{"peer certificate carries no ActorIdentity", xfccWithCert(actorCertPEM(t, nil))},
		// The purpose check the atelet credential broker was waiting for.
		{"identity is not for atunnel", xfccWithCert(forgedIdentityCertPEM(t,
			`{"Atespace":"acme-prod","ActorName":"wide-open","ActorUid":"u1","Purpose":"session"}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := []string{":method: CONNECT", ":authority: 9.9.9.9:443"}
			if tc.xfcc != "" {
				headers = append(headers, XFCCHeader+": "+tc.xfcc)
			}
			resp := run(t, CheckpointConnect, store, headersRequest(headers...))
			if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
				t.Errorf("status = %v, want 403", got)
			}
		})
	}
}

// An actor whose identity headers are forged inside the tunnel gets nothing:
// the X-Ate-* headers PR 708 deleted are no longer an identity source, and the
// certificate is the only one.
func TestConnectCheckpointIgnoresForgedIdentityHeaders(t *testing.T) {
	resp := run(t, CheckpointConnect, hardcodedStore(), headersRequest(
		":method: CONNECT",
		":authority: 9.9.9.9:443",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: wide-open",
		ActorKeyHeader+": "+DemoAtespace+"/wide-open",
	))
	if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
		t.Errorf("status = %v, want 403; header-asserted identity was honoured", got)
	}
}

// ALLOW_BY_HOSTNAME and BASIC_CREDENTIAL_INJECT can only be decided after MITM,
// at the inner checkpoint. A gateway that does not run one cannot enforce them,
// and must deny rather than let those actors through constrained only by the
// gateway-wide sdsmintd allowlist -- which would be a silent per-actor bypass.
func TestConnectDeniesMITMWithoutTheInnerCheckpoint(t *testing.T) {
	for _, actor := range []string{"repo-reader", "invoice-agent"} {
		t.Run(actor, func(t *testing.T) {
			resp := run(t, CheckpointConnect, hardcodedStore(),
				connectRequest(t, actor, "140.82.121.4:443"))
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
	resp := run(t, CheckpointConnect, hardcodedStore(),
		connectRequest(t, "wide-open", "9.9.9.9:443"))
	rh := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	if !rh.RequestHeaders.GetResponse().GetClearRouteCache() {
		t.Error("allow response does not set clear_route_cache; the mode header would be inert")
	}
}

// An actor that sets x-ate-egress-mode on its own CONNECT must not be able to
// pick its route. The mutation has to overwrite, not append.
func TestConnectOverwritesAForgedModeHeader(t *testing.T) {
	resp := run(t, CheckpointConnect, hardcodedStore(),
		// repo-reader must go to mitm; it is asking for passthrough to skip the
		// hostname check entirely.
		connectRequest(t, "repo-reader", "1.1.1.1:443", EgressModeHeader+": passthrough"),
		WithInnerCheckpoint())

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
		XFCCHeader+": "+xfccFor(t, DemoAtespace, "wide-open"),
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
			resp := run(t, CheckpointInner, store, withFilterStateActor(headersRequest(
				":method: GET",
				":authority: "+tc.host,
			), "repo-reader"))
			denied := immediateStatus(resp) == typev3.StatusCode_Forbidden
			if denied == tc.wantAllow {
				t.Errorf("host %q: denied=%v, wantAllow=%v", tc.host, denied, tc.wantAllow)
			}
		})
	}
}

func TestInnerCheckpointInjectsCredentials(t *testing.T) {
	resp := run(t, CheckpointInner, hardcodedStore(), withFilterStateActor(headersRequest(
		":method: GET",
		":authority: api.stripe.com",
		// Whatever the actor sent must not survive.
		"authorization: Bearer actor-supplied-value",
	), "invoice-agent"))

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

// The gateway's own routing headers, the X-Ate-* metadata an actor may still be
// sending, and the certificate Envoy forwarded are all internal. None may reach
// the real destination -- x-forwarded-client-cert least of all, since it would
// hand the actor's certificate to whatever it is talking to.
func TestInnerCheckpointStripsInternalHeaders(t *testing.T) {
	resp := run(t, CheckpointInner, hardcodedStore(), withFilterStateActor(headersRequest(
		":method: GET",
		":authority: github.com",
		"x-ate-atespace: "+DemoAtespace,
		"x-ate-actor-name: repo-reader",
		"x-ate-actor-version: 7",
		EgressModeHeader+": mitm",
		ActorKeyHeader+": "+DemoAtespace+"/repo-reader",
		XFCCHeader+": "+xfccFor(t, DemoAtespace, "repo-reader"),
	), "repo-reader"))

	removed := removedHeaders(t, resp)
	for _, want := range tunnelInternalHeaders {
		if !slices.Contains(removed, want) {
			t.Errorf("remove_headers = %v, missing %q", removed, want)
		}
	}
}

// Filter state is the only source the inner checkpoint reads. Everything inside
// the tunnel is a channel the actor controls end to end, so a conflicting claim
// there must not merely lose -- it must not be consulted at all. An actor that
// reached api.stripe.com by setting x-ate-actor-key would collect
// invoice-agent's injected credential.
func TestInnerCheckpointReadsIdentityOnlyFromFilterState(t *testing.T) {
	// The actor claims to be invoice-agent inside its own tunnel.
	forged := []string{
		ActorKeyHeader + ": " + DemoAtespace + "/invoice-agent",
		"x-ate-actor-name: invoice-agent",
		"x-ate-atespace: " + DemoAtespace,
	}

	t.Run("a conflicting claim loses to filter state", func(t *testing.T) {
		req := withFilterStateActor(headersRequest(append([]string{
			":method: GET",
			":authority: api.stripe.com",
		}, forged...)...), "repo-reader")

		resp := run(t, CheckpointInner, hardcodedStore(), req)
		if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
			t.Fatalf("status = %v, want 403: filter state says repo-reader, which cannot reach api.stripe.com", got)
		}
	})

	t.Run("a claim with no filter state is not an identity", func(t *testing.T) {
		resp := run(t, CheckpointInner, hardcodedStore(), headersRequest(append([]string{
			":method: GET",
			":authority: api.stripe.com",
		}, forged...)...))
		if got := immediateStatus(resp); got != typev3.StatusCode_Forbidden {
			t.Fatalf("status = %v, want 403: a header-asserted identity was honoured", got)
		}
	})
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
			resp := run(t, cp, &Store{},
				withFilterStateActor(connectRequest(t, "wide-open", "9.9.9.9:443"), "wide-open"))
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
	req := connectRequest(t, "swapper", "9.9.9.9:443")

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
// every actor looks anonymous and every request is denied. The XFCC header is
// deliberately the one in Value: it is the identity, so it is where reading the
// wrong field costs the most.
func TestHeadersAreReadFromEitherValueField(t *testing.T) {
	req := &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: ":method", Value: "CONNECT"},
					{Key: ":authority", RawValue: []byte("9.9.9.9:443")},
					// Mixed case too: Envoy does not normalise this one for us.
					{Key: "X-Forwarded-Client-Cert", Value: xfccFor(t, DemoAtespace, "wide-open")},
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
	resp := run(t, CheckpointConnect, hardcodedStore(),
		connectRequest(t, "quarantined", "9.9.9.9:443"))
	ir := resp.Response.(*extprocv3.ProcessingResponse_ImmediateResponse)
	details := ir.ImmediateResponse.GetDetails()
	if details == "" {
		t.Fatal("deny response carries no details")
	}
	if strings.ContainsAny(details, " \t\r\n") {
		t.Errorf("details %q contains whitespace", details)
	}
}
