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
	"context"
	"fmt"
	"io"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
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
func process(t *testing.T, policy Policy, req *extprocv3.ProcessingRequest) *extprocv3.ProcessingResponse {
	t.Helper()
	stream := &fakeStream{incoming: []*extprocv3.ProcessingRequest{req}}
	if err := NewServer(policy, nil).Process(stream); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("Process sent %d responses, want exactly 1", len(stream.sent))
	}
	return stream.sent[0]
}

func TestProcessAllows(t *testing.T) {
	resp := process(t, AllowAll(), rawHeaders(map[string]string{
		":method":    "GET",
		":authority": "example.com",
		":path":      "/",
	}))

	headers, ok := resp.GetResponse().(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("allow returned %T, want a RequestHeaders response", resp.GetResponse())
	}
	// An allow must carry no mutation. The filter's rules permit ordinary
	// header mutations so that credential injection can land here later, which
	// means a mutation returned by accident would actually be applied rather
	// than rejected.
	if mutation := headers.RequestHeaders.GetResponse().GetHeaderMutation(); mutation != nil {
		t.Errorf("allow carried a header mutation: %v", mutation)
	}
}

func TestProcessDenies(t *testing.T) {
	policy, err := DenyHosts([]string{"blocked.example.com"})
	if err != nil {
		t.Fatalf("DenyHosts: %v", err)
	}
	resp := process(t, policy, rawHeaders(map[string]string{
		":method":    "GET",
		":authority": "blocked.example.com",
		":path":      "/",
	}))

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

func TestDenyHostsMatching(t *testing.T) {
	policy, err := DenyHosts([]string{"Blocked.Example.COM", "other.invalid"})
	if err != nil {
		t.Fatalf("DenyHosts: %v", err)
	}

	for _, tc := range []struct {
		name      string
		authority string
		wantAllow bool
	}{
		{"exact", "blocked.example.com", false},
		// DNS names are case-insensitive, so a policy comparing the authority
		// verbatim is bypassed by shifting the case of a single letter.
		{"uppercase authority", "BLOCKED.example.com", false},
		// The authority may carry a port, and a denylist keyed on the full
		// authority would miss every non-default one.
		{"with port", "blocked.example.com:8443", false},
		// A trailing dot is a legal absolute form of the same name.
		{"trailing dot", "blocked.example.com.", false},
		{"listed second", "other.invalid", false},
		{"unlisted", "allowed.example.com", true},
		// A denylist entry must not leak into its subdomains or its parent:
		// both are different names that were not listed.
		{"subdomain of a denied name", "sub.blocked.example.com", true},
		{"parent of a denied name", "example.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := policy.Evaluate(t.Context(), &Request{Authority: tc.authority})
			if decision.Allow != tc.wantAllow {
				t.Errorf("Evaluate(%q).Allow = %v, want %v", tc.authority, decision.Allow, tc.wantAllow)
			}
		})
	}
}

func TestDenyHostsRejectsMalformedEntries(t *testing.T) {
	for _, entry := range []string{"", "   ", "https://example.com", "example.com:443", "example.com/path", "two names"} {
		if _, err := DenyHosts([]string{entry}); err == nil {
			t.Errorf("DenyHosts(%q) succeeded, want an error: an entry that never matches is a policy that silently does nothing", entry)
		}
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

// The policy sees a denied host over cleartext exactly as it does over TLS,
// whichever header carries it. An asymmetry between the two mitm_listener
// chains would be an actor's cheapest bypass: speak plaintext.
func TestProcessDeniesCleartextHostHeader(t *testing.T) {
	policy, err := DenyHosts([]string{"blocked.example.com"})
	if err != nil {
		t.Fatalf("DenyHosts: %v", err)
	}
	resp := process(t, policy, rawHeaders(map[string]string{
		":method": "GET",
		"host":    "blocked.example.com",
		":path":   "/",
	}))

	if _, ok := resp.GetResponse().(*extprocv3.ProcessingResponse_ImmediateResponse); !ok {
		t.Fatalf("cleartext request to a denied host returned %T, want an ImmediateResponse", resp.GetResponse())
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
			resp := process(t, AllowAll(), tc.req)
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
	if err := NewServer(AllowAll(), nil).Process(stream); err == nil {
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
	if err := NewServer(AllowAll(), nil).Process(stream); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(stream.sent) != 2 {
		t.Errorf("Process sent %d responses for 2 messages, want 2", len(stream.sent))
	}
}

func typeName(v any) string {
	return fmt.Sprintf("%T", v)
}
