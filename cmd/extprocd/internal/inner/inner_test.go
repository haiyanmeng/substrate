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

package inner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeStream plays Envoy: it hands the server a fixed list of messages, then
// EOF, and collects what comes back.
type fakeStream struct {
	grpc.ServerStream
	incoming []*extprocv3.ProcessingRequest
	sent     []*extprocv3.ProcessingResponse
	sendErr  error
}

func (s *fakeStream) Context() context.Context { return context.Background() }

func (s *fakeStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if len(s.incoming) == 0 {
		return nil, io.EOF
	}
	req := s.incoming[0]
	s.incoming = s.incoming[1:]
	return req, nil
}

func (s *fakeStream) Send(resp *extprocv3.ProcessingResponse) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, resp)
	return nil
}

// rawHeaders builds a RequestHeaders message the way recent Envoy sends one:
// values in raw_value, with value left empty.
func rawHeaders(pairs map[string]string) *extprocv3.ProcessingRequest {
	headers := &corev3.HeaderMap{}
	for key, value := range pairs {
		headers.Headers = append(headers.Headers, &corev3.HeaderValue{
			Key:      key,
			RawValue: []byte(value),
		})
	}
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: headers},
		},
	}
}

// process runs one request through a server and returns the single response.
func process(t *testing.T, req *extprocv3.ProcessingRequest) *extprocv3.ProcessingResponse {
	t.Helper()
	stream := &fakeStream{incoming: []*extprocv3.ProcessingRequest{req}}
	if err := NewServer().Process(stream); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("Process sent %d responses, want exactly 1", len(stream.sent))
	}
	return stream.sent[0]
}

func TestProcessAllows(t *testing.T) {
	resp := process(t, rawHeaders(map[string]string{
		":method":    "GET",
		":authority": "example.com",
		":path":      "/",
	}))

	headers, ok := resp.GetResponse().(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("allow returned %T, want a RequestHeaders response", resp.GetResponse())
	}
	// An allow with no credential must carry no mutation. The filter's rules
	// permit ordinary header mutations so that credential injection can work,
	// which means a mutation returned by accident is one Envoy applies.
	if mutation := headers.RequestHeaders.GetResponse().GetHeaderMutation(); mutation != nil {
		t.Errorf("allow carried a header mutation: %v", mutation)
	}
}

// mutationOf returns the single header mutation on an allow response.
func mutationOf(t *testing.T, resp *extprocv3.ProcessingResponse) *extprocv3.HeaderMutation {
	t.Helper()
	headers, ok := resp.GetResponse().(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("allow returned %T, want a RequestHeaders response", resp.GetResponse())
	}
	return headers.RequestHeaders.GetResponse().GetHeaderMutation()
}

func TestAllowResponseInjectsTheCredential(t *testing.T) {
	resp := NewServer().allowResponse(t.Context(),
		AllowWithCredential("authorization", "Bearer s3cret"))

	mutation := mutationOf(t, resp)
	if len(mutation.GetSetHeaders()) != 1 {
		t.Fatalf("allow set %d headers, want exactly 1", len(mutation.GetSetHeaders()))
	}
	set := mutation.GetSetHeaders()[0]

	if got := set.GetHeader().GetKey(); got != "authorization" {
		t.Errorf("injected header = %q, want authorization", got)
	}
	// Recent Envoy reads raw_value and ignores value, so a credential written
	// to value alone is an empty header rather than a visible failure.
	if got := string(set.GetHeader().GetRawValue()); got != "Bearer s3cret" {
		t.Errorf("injected raw_value = %q, want the credential", got)
	}
	// The default is APPEND_IF_EXISTS_OR_ADD. The actor writes its own headers
	// inside its own tunnel, so appending would leave a forged authorization
	// beside the real one and let the upstream choose between them.
	if got := set.GetAppendAction(); got != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
		t.Errorf("append action = %v, want OVERWRITE_IF_EXISTS_OR_ADD", got)
	}
}

// Rewriting :authority would send the credential to a name the SNI was never
// policed for, which is the one mutation that turns injection into
// exfiltration. Envoy's own rules refuse it, but by default they refuse it
// silently, so a policy that tried would look like it worked.
func TestAllowResponseRefusesPseudoHeaderInjection(t *testing.T) {
	resp := NewServer().allowResponse(t.Context(),
		AllowWithCredential(":authority", "attacker.example.com"))

	if mutation := mutationOf(t, resp); mutation != nil {
		t.Errorf("allow carried a pseudo-header mutation: %v", mutation)
	}
}

// The shipped Evaluate injects nothing, and a nil Credential must read as "no
// injection" everywhere rather than panicking on the way.
func TestAllowResponseWithoutACredential(t *testing.T) {
	resp := NewServer().allowResponse(t.Context(), Allow())

	if mutation := mutationOf(t, resp); mutation != nil {
		t.Errorf("allow without a credential carried a mutation: %v", mutation)
	}
}

// Evaluate allows everything today, so there is no request that reaches the
// deny path through Process. Pin the shape of a denial anyway: it is what the
// first real policy will emit, and every assertion here is about how the
// denial reads to Envoy and to the actor rather than about who triggered it.
func TestDenyResponse(t *testing.T) {
	resp := denyResponse()

	immediate, ok := resp.GetResponse().(*extprocv3.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("deny returned %T, want an ImmediateResponse", resp.GetResponse())
	}
	if got := immediate.ImmediateResponse.GetStatus().GetCode(); got != envoy_type.StatusCode_Forbidden {
		t.Errorf("deny status = %v, want Forbidden", got)
	}
	// Without details the denial is indistinguishable in the access log from
	// the CONNECT checkpoint's 403 and the cleartext chain's direct_response.
	if got := immediate.ImmediateResponse.GetDetails(); got != DenyDetails {
		t.Errorf("deny details = %q, want %q", got, DenyDetails)
	}
	// The reason is for the operator's logs. Leaking it to the actor turns the
	// deny path into a way to enumerate the policy.
	if body := string(immediate.ImmediateResponse.GetBody()); body != denyBody {
		t.Errorf("deny body = %q, want the fixed body %q", body, denyBody)
	}
}

// Host is the form a hostname policy is meant to match on, so its
// normalization is the thing standing between a policy and its cheapest
// bypasses. Each case below is one of them.
func TestRequestHostNormalizes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		authority string
		want      string
	}{
		{"already normal", "blocked.example.com", "blocked.example.com"},
		// DNS names are case-insensitive, so a policy comparing the authority
		// verbatim is bypassed by shifting the case of a single letter.
		{"uppercase", "BLOCKED.Example.COM", "blocked.example.com"},
		// The authority may carry a port, and a policy keyed on the full
		// authority would miss every non-default one.
		{"with port", "blocked.example.com:8443", "blocked.example.com"},
		// A trailing dot is a legal absolute form of the same name.
		{"trailing dot", "blocked.example.com.", "blocked.example.com"},
		{"port and case and dot", "Blocked.Example.COM.:8443", "blocked.example.com"},
		// Subdomains and parents are different names and must stay distinct: a
		// Host that collapsed either one would silently widen every policy
		// written against it.
		{"subdomain stays distinct", "sub.blocked.example.com", "sub.blocked.example.com"},
		{"parent stays distinct", "example.com", "example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Request{Authority: tc.authority}).Host(); got != tc.want {
				t.Errorf("Request{Authority: %q}.Host() = %q, want %q", tc.authority, got, tc.want)
			}
		})
	}
}

// Recent Envoy sends header values in raw_value and leaves value empty. A
// server reading only .Value sees an empty authority on every request, which a
// hostname policy reads as "no destination" rather than as a bug.
func TestRequestFromHeadersReadsRawValueAndValue(t *testing.T) {
	headers := &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: ":method", RawValue: []byte("POST")},
		{Key: ":authority", Value: "legacy.example.com"},
		{Key: ":path", RawValue: []byte("/v1/things")},
		{Key: ":scheme", RawValue: []byte("https")},
		{Key: "X-Custom", RawValue: []byte("kept")},
	}}

	req := requestFromHeaders(&extprocv3.HttpHeaders{Headers: headers})

	if req.Method != "POST" {
		t.Errorf("Method = %q, want POST", req.Method)
	}
	if req.Authority != "legacy.example.com" {
		t.Errorf("Authority = %q, want the value-form header to be read", req.Authority)
	}
	if req.Path != "/v1/things" {
		t.Errorf("Path = %q, want /v1/things", req.Path)
	}
	if req.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", req.Scheme)
	}
	// Keys are lower-cased so a policy does not have to guess the casing.
	if got := req.Headers["x-custom"]; got != "kept" {
		t.Errorf("Headers[x-custom] = %q, want kept", got)
	}
}

// The host fallback. Envoy folds Host into :authority before ext_proc runs, so
// the gateway does not produce this shape today; the test pins the fallback so
// a headers-only message still yields a destination rather than an empty
// string, which a hostname policy would match on and allow.
func TestRequestFromHeadersFallsBackToHost(t *testing.T) {
	req := requestFromHeaders(&extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{
		Headers: []*corev3.HeaderValue{
			{Key: ":method", RawValue: []byte("GET")},
			{Key: "host", RawValue: []byte("cleartext.example.com")},
		},
	}})

	if req.Authority != "cleartext.example.com" {
		t.Errorf("Authority = %q, want the Host header", req.Authority)
	}
	if req.Host() != "cleartext.example.com" {
		t.Errorf("Host() = %q, want cleartext.example.com", req.Host())
	}
}

// Evaluate sees the same destination over cleartext as it does over TLS,
// whichever header carries it. An asymmetry between the two mitm_listener
// chains would be an actor's cheapest bypass: speak plaintext.
func TestEvaluateSeesTheSameHostFromEitherHeader(t *testing.T) {
	viaAuthority := requestFromHeaders(&extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{
		Headers: []*corev3.HeaderValue{{Key: ":authority", RawValue: []byte("blocked.example.com")}},
	}})
	viaHost := requestFromHeaders(&extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{
		Headers: []*corev3.HeaderValue{{Key: "host", RawValue: []byte("blocked.example.com")}},
	}})

	if viaAuthority.Host() != viaHost.Host() {
		t.Errorf("Host() = %q via :authority but %q via host, want the two chains to look identical to a policy",
			viaAuthority.Host(), viaHost.Host())
	}
}

// Envoy fails a request whose response does not match the message it sent, and
// under failure_mode_allow: false that failure is a denial. These kinds are all
// SKIPped by the filter config, so reaching one means the processing_mode
// drifted -- which should not also break the request.
func TestProcessAnswersEachMessageKindInKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *extprocv3.ProcessingRequest
		want any
	}{
		{
			"response headers",
			&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{
				ResponseHeaders: &extprocv3.HttpHeaders{},
			}},
			&extprocv3.ProcessingResponse_ResponseHeaders{},
		},
		{
			"request body",
			&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{},
			}},
			&extprocv3.ProcessingResponse_RequestBody{},
		},
		{
			"response body",
			&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseBody{
				ResponseBody: &extprocv3.HttpBody{},
			}},
			&extprocv3.ProcessingResponse_ResponseBody{},
		},
		{
			"request trailers",
			&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestTrailers{
				RequestTrailers: &extprocv3.HttpTrailers{},
			}},
			&extprocv3.ProcessingResponse_RequestTrailers{},
		},
		{
			"response trailers",
			&extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseTrailers{
				ResponseTrailers: &extprocv3.HttpTrailers{},
			}},
			&extprocv3.ProcessingResponse_ResponseTrailers{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := process(t, tc.req)
			if got, want := typeName(resp.GetResponse()), typeName(tc.want); got != want {
				t.Errorf("response kind = %s, want %s", got, want)
			}
		})
	}
}

// An unknown message kind must not be answered with a guess. Ending the stream
// without a response is the fail-closed outcome: Envoy reports it as a 500.
func TestProcessRefusesAnUnknownMessageKind(t *testing.T) {
	stream := &fakeStream{incoming: []*extprocv3.ProcessingRequest{{}}}
	if err := NewServer().Process(stream); err == nil {
		t.Fatal("Process accepted a ProcessingRequest with no message set, want an error")
	}
	if len(stream.sent) != 0 {
		t.Errorf("Process answered an unknown message with %v, want no response at all", stream.sent)
	}
}

func TestProcessHandlesMultipleMessagesAndEOF(t *testing.T) {
	stream := &fakeStream{incoming: []*extprocv3.ProcessingRequest{
		rawHeaders(map[string]string{":authority": "a.example.com"}),
		rawHeaders(map[string]string{":authority": "b.example.com"}),
	}}
	if err := NewServer().Process(stream); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(stream.sent) != 2 {
		t.Errorf("Process sent %d responses for 2 messages, want 2", len(stream.sent))
	}
}

func typeName(v any) string {
	return fmt.Sprintf("%T", v)
}

func TestParseActorSplitsTheSpiffeID(t *testing.T) {
	for _, tc := range []struct {
		name              string
		uri               string
		atespace, actorNm string
	}{
		{"a real one", "spiffe://substrate-actor.local/atespace/demo/actor/egress-demo", "demo", "egress-demo"},
		{"names with hyphens", "spiffe://substrate-actor.local/atespace/acme-prod/actor/invoice-agent", "acme-prod", "invoice-agent"},

		// Everything below must yield "", "": Evaluate keys policy on the
		// result, so a shape this does not fully understand has to read as
		// "no identity" rather than as a partially-understood one.
		{"unasserted", "", "", ""},
		{"wrong trust domain", "spiffe://substrate-pod.local/atespace/demo/actor/egress-demo", "", ""},
		{"not spiffe", "https://substrate-actor.local/atespace/demo/actor/egress-demo", "", ""},
		{"no actor segment", "spiffe://substrate-actor.local/atespace/demo", "", ""},
		{"empty atespace", "spiffe://substrate-actor.local/atespace//actor/egress-demo", "", ""},
		{"empty actor name", "spiffe://substrate-actor.local/atespace/demo/actor/", "", ""},
		{"trailing path segment", "spiffe://substrate-actor.local/atespace/demo/actor/egress-demo/extra", "", ""},
		{"prefix only", "spiffe://substrate-actor.local/atespace/", "", ""},
		// A name that merely contains an actor URI is not one. This is the
		// case a prefix or substring match on Actor would get wrong.
		{"embedded, not prefixed", "spiffe://evil.example/x/spiffe://substrate-actor.local/atespace/demo/actor/root", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			atespace, actorNm := parseActor(tc.uri)
			if atespace != tc.atespace || actorNm != tc.actorNm {
				t.Errorf("parseActor(%q) = %q, %q; want %q, %q", tc.uri, atespace, actorNm, tc.atespace, tc.actorNm)
			}
		})
	}
}

// The split parts and the header dump have to reach the log, which is the
// only place they are observable while Evaluate ignores them.
func TestAuthorizeLogsTheSplitActorAndHeaders(t *testing.T) {
	const uri = "spiffe://substrate-actor.local/atespace/demo/actor/egress-demo"

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(restore)

	attrs := map[string]*structpb.Struct{
		"envoy.filters.http.ext_proc": {Fields: map[string]*structpb.Value{
			actorAttribute: structpb.NewStringValue(uri),
		}},
	}
	headers := rawHeaders(map[string]string{
		":authority":    "example.com",
		"user-agent":    "curl/8",
		"authorization": "Bearer super-secret-token",
	}).GetRequestHeaders()

	NewServer().authorize(context.Background(), attrs, headers)

	logged := buf.String()
	for _, want := range []string{
		`actor=spiffe://substrate-actor.local/atespace/demo/actor/egress-demo`,
		`atespace=demo`,
		`actor_name=egress-demo`,
		`headers.user-agent=curl/8`,
		`headers.:authority=example.com`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log is missing %s\ngot: %s", want, logged)
		}
	}
	if strings.Contains(logged, "super-secret-token") {
		t.Errorf("the authorization value reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "headers.authorization=") {
		t.Errorf("the authorization header name was dropped, want presence still visible:\n%s", logged)
	}
}

func TestHeadersLogValueSortsAndRedacts(t *testing.T) {
	h := Headers{
		"user-agent":    "curl/8",
		":authority":    "example.com",
		"authorization": "Bearer super-secret-token",
		"cookie":        "session=abc",
	}

	var keys []string
	var byKey = map[string]string{}
	for _, a := range h.LogValue().Group() {
		keys = append(keys, a.Key)
		byKey[a.Key] = a.Value.String()
	}

	want := []string{":authority", "authorization", "cookie", "user-agent"}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v (sorted)", keys, want)
	}
	if byKey["user-agent"] != "curl/8" {
		t.Errorf("user-agent = %q, want it logged in full", byKey["user-agent"])
	}
	for _, secret := range []string{"authorization", "cookie"} {
		if strings.Contains(byKey[secret], "secret") || strings.Contains(byKey[secret], "session=") {
			t.Errorf("%s = %q, want the value withheld", secret, byKey[secret])
		}
		if byKey[secret] == "" {
			t.Errorf("%s was dropped entirely, want the name kept so presence is still visible", secret)
		}
	}
}
