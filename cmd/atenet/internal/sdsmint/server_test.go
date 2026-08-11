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

package sdsmint

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"testing"
	"time"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/metadata"
)

// fakeDeltaStream stands in for the gRPC stream Envoy would be on the other
// end of. Requests are fed in from a slice; responses are collected.
type fakeDeltaStream struct {
	ctx      context.Context
	requests chan *discovery.DeltaDiscoveryRequest
	sent     chan *discovery.DeltaDiscoveryResponse
}

func newFakeDeltaStream(ctx context.Context) *fakeDeltaStream {
	return &fakeDeltaStream{
		ctx:      ctx,
		requests: make(chan *discovery.DeltaDiscoveryRequest, 8),
		sent:     make(chan *discovery.DeltaDiscoveryResponse, 8),
	}
}

func (f *fakeDeltaStream) Send(resp *discovery.DeltaDiscoveryResponse) error {
	select {
	case f.sent <- resp:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *fakeDeltaStream) Recv() (*discovery.DeltaDiscoveryRequest, error) {
	select {
	case req, ok := <-f.requests:
		if !ok {
			return nil, io.EOF
		}
		return req, nil
	case <-f.ctx.Done():
		return nil, io.EOF
	}
}

func (f *fakeDeltaStream) Context() context.Context     { return f.ctx }
func (f *fakeDeltaStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeDeltaStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeDeltaStream) SetTrailer(metadata.MD)       {}
func (f *fakeDeltaStream) SendMsg(any) error            { return nil }
func (f *fakeDeltaStream) RecvMsg(any) error            { return nil }

// nextResponse waits for one response, failing the test if none arrives.
func (f *fakeDeltaStream) nextResponse(t *testing.T) *discovery.DeltaDiscoveryResponse {
	t.Helper()
	select {
	case resp := <-f.sent:
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a DeltaDiscoveryResponse")
		return nil
	}
}

// startServer runs DeltaSecrets against a fake stream and returns the stream
// plus a func that stops it and reports the server's error.
func startServer(t *testing.T, srv *Server) (*fakeDeltaStream, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeDeltaStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.DeltaSecrets(stream) }()

	return stream, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("DeltaSecrets did not return after the stream was cancelled")
			return nil
		}
	}
}

func testServer(t *testing.T, opts ServerOptions, patterns []string) *Server {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.TTL <= 0 {
		opts.TTL = time.Minute
	}
	m := testMinter(t, MinterOptions{Validate: AllowGlobs(patterns), TTL: opts.TTL})
	return NewServer(m, opts)
}

// unpackSecret pulls the Secret proto out of a delta Resource.
func unpackSecret(t *testing.T, res *discovery.Resource) *tlsv3.Secret {
	t.Helper()
	msg, err := res.GetResource().UnmarshalNew()
	if err != nil {
		t.Fatalf("unmarshalling resource %q: %v", res.GetName(), err)
	}
	secret, ok := msg.(*tlsv3.Secret)
	if !ok {
		t.Fatalf("resource %q is a %T, want *tlsv3.Secret", res.GetName(), msg)
	}
	return secret
}

func TestDeltaSecretsMintsSubscribedName(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}

	resp := stream.nextResponse(t)
	if resp.GetTypeUrl() != SecretTypeURL {
		t.Errorf("type_url = %q, want %q", resp.GetTypeUrl(), SecretTypeURL)
	}
	if resp.GetNonce() == "" {
		t.Error("response has no nonce; Envoy needs one to ACK")
	}
	if len(resp.GetResources()) != 1 {
		t.Fatalf("got %d resources, want 1", len(resp.GetResources()))
	}

	res := resp.GetResources()[0]
	if res.GetName() != "a.example" {
		t.Errorf("resource name = %q, want a.example", res.GetName())
	}
	if res.GetVersion() == "" {
		t.Error("resource has no version; delta xDS needs one per resource")
	}

	secret := unpackSecret(t, res)
	// This is the invariant the whole design rests on: Envoy matches the
	// response to its on-demand subscription by secret name, which is the SNI.
	if secret.GetName() != "a.example" {
		t.Errorf("secret name = %q, want it to equal the requested resource name", secret.GetName())
	}

	chain := secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes()
	key := secret.GetTlsCertificate().GetPrivateKey().GetInlineBytes()
	if _, err := tls.X509KeyPair(chain, key); err != nil {
		t.Errorf("secret does not contain a usable TLS keypair: %v", err)
	}
}

func TestDeltaSecretsWithdrawsDisallowedName(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.allowed"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"ok.allowed", "evil.test"},
	}

	resp := stream.nextResponse(t)

	if len(resp.GetResources()) != 1 || resp.GetResources()[0].GetName() != "ok.allowed" {
		t.Errorf("resources = %v, want just ok.allowed", resourceNames(resp))
	}
	// A server cannot NACK in xDS. Withdrawing the name is how it says "this
	// will not be issued", and per the Envoy docs it also cancels the
	// data-plane subscription for that name.
	if got := resp.GetRemovedResources(); len(got) != 1 || got[0] != "evil.test" {
		t.Errorf("removed_resources = %v, want [evil.test]", got)
	}
}

func TestDeltaSecretsBareAckSendsNothing(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}
	first := stream.nextResponse(t)

	// Envoy's ACK carries the nonce and no subscriptions. Replying to it
	// would start an infinite ACK loop.
	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:       SecretTypeURL,
		ResponseNonce: first.GetNonce(),
	}

	select {
	case resp := <-stream.sent:
		t.Fatalf("server replied to a bare ACK with %v", resourceNames(resp))
	case <-time.After(300 * time.Millisecond):
	}
}

func TestDeltaSecretsRotationPushesNewVersion(t *testing.T) {
	// Envoy has no TTL of its own for an on-demand secret, so the server has
	// to push a replacement or the leaf silently expires under a live
	// subscription.
	srv := testServer(t, ServerOptions{Rotate: true, TTL: 300 * time.Millisecond}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}
	first := stream.nextResponse(t)
	firstVersion := first.GetResources()[0].GetVersion()

	// The rotation interval is floored at 1s, so with a 300ms TTL the first
	// tick lands well after the cache stops reusing the leaf and re-mints.
	// This test only cares that the version moves; the timing relationship
	// itself is TestRotationNeverServesAnExpiredLeaf's job.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case resp := <-stream.sent:
			if len(resp.GetResources()) == 0 {
				continue
			}
			if resp.GetResources()[0].GetVersion() != firstVersion {
				if resp.GetResources()[0].GetName() != "a.example" {
					t.Fatalf("rotated resource name = %q, want a.example", resp.GetResources()[0].GetName())
				}
				return // rotation observed
			}
		case <-deadline:
			t.Fatal("no rotation push with a new version arrived")
		}
	}
}

func TestDeltaSecretsUnsubscribeStopsRotation(t *testing.T) {
	srv := testServer(t, ServerOptions{Rotate: true, TTL: 200 * time.Millisecond}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}
	stream.nextResponse(t)

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                  SecretTypeURL,
		ResourceNamesUnsubscribe: []string{"a.example"},
	}

	// Drain anything already queued, then confirm the stream goes quiet.
	time.Sleep(300 * time.Millisecond)
	for len(stream.sent) > 0 {
		<-stream.sent
	}

	select {
	case resp := <-stream.sent:
		t.Fatalf("server kept rotating an unsubscribed name: %v", resourceNames(resp))
	case <-time.After(600 * time.Millisecond):
	}
}

func TestDeltaSecretsSeedsFromInitialResourceVersions(t *testing.T) {
	// On reconnect Envoy replays what it already holds. The server must adopt
	// that state so rotation covers those names without re-pushing them.
	srv := testServer(t, ServerOptions{Rotate: true, TTL: 200 * time.Millisecond}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                 SecretTypeURL,
		InitialResourceVersions: map[string]string{"resumed.example": "old-version"},
	}

	// No immediate response: nothing was subscribed in this request.
	select {
	case resp := <-stream.sent:
		t.Fatalf("server responded to a replay-only request with %v", resourceNames(resp))
	case <-time.After(100 * time.Millisecond):
	}

	// But the resumed name must still get rotated.
	select {
	case resp := <-stream.sent:
		names := resourceNames(resp)
		if len(names) != 1 || names[0] != "resumed.example" {
			t.Fatalf("rotation covered %v, want [resumed.example]", names)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed subscription was never rotated")
	}
}

func TestDeltaSecretsRejectsWrongTypeURL(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.example"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeDeltaStream(ctx)

	done := make(chan error, 1)
	go func() { done <- srv.DeltaSecrets(stream) }()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                "type.googleapis.com/envoy.config.cluster.v3.Cluster",
		ResourceNamesSubscribe: []string{"a.example"},
	}

	// Wait for the server to fail on its own rather than canceling, which
	// would race the context-done branch of the stream loop.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("DeltaSecrets accepted a non-SDS type_url")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DeltaSecrets did not reject a non-SDS type_url")
	}
}

func TestDeltaSecretsSurvivesNack(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}
	first := stream.nextResponse(t)

	// A NACK must not tear down the stream; Envoy would just reconnect and
	// we would lose every live subscription.
	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:       SecretTypeURL,
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   &rpcstatus.Status{Code: 3, Message: "bad certificate"},
	}
	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"b.example"},
	}

	resp := stream.nextResponse(t)
	if names := resourceNames(resp); len(names) != 1 || names[0] != "b.example" {
		t.Errorf("after a NACK the server served %v, want [b.example]", names)
	}
}

func TestFetchSecrets(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.example"})

	resp, err := srv.FetchSecrets(context.Background(), &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeURL,
		ResourceNames: []string{"a.example", "denied.test"},
	})
	if err != nil {
		t.Fatalf("FetchSecrets: %v", err)
	}
	// SotW has no removal channel, so the denied name can only be omitted.
	if len(resp.GetResources()) != 1 {
		t.Fatalf("got %d resources, want 1", len(resp.GetResources()))
	}
	if resp.GetVersionInfo() == "" {
		t.Error("SotW response has no version_info")
	}
}

func TestStreamSecrets(t *testing.T) {
	srv := testServer(t, ServerOptions{}, []string{"*.example"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := newFakeSotWStream(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.StreamSecrets(stream) }()

	stream.requests <- &discovery.DiscoveryRequest{
		TypeUrl:       SecretTypeURL,
		ResourceNames: []string{"a.example"},
	}

	select {
	case resp := <-stream.sent:
		if len(resp.GetResources()) != 1 {
			t.Errorf("got %d resources, want 1", len(resp.GetResources()))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a SotW response")
	}

	close(stream.requests)
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("StreamSecrets returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StreamSecrets did not return after EOF")
	}
}

func resourceNames(resp *discovery.DeltaDiscoveryResponse) []string {
	names := make([]string, 0, len(resp.GetResources()))
	for _, r := range resp.GetResources() {
		names = append(names, r.GetName())
	}
	return names
}

// fakeSotWStream is the state-of-the-world equivalent of fakeDeltaStream.
type fakeSotWStream struct {
	ctx      context.Context
	requests chan *discovery.DiscoveryRequest
	sent     chan *discovery.DiscoveryResponse
}

func newFakeSotWStream(ctx context.Context) *fakeSotWStream {
	return &fakeSotWStream{
		ctx:      ctx,
		requests: make(chan *discovery.DiscoveryRequest, 8),
		sent:     make(chan *discovery.DiscoveryResponse, 8),
	}
}

func (f *fakeSotWStream) Send(resp *discovery.DiscoveryResponse) error {
	select {
	case f.sent <- resp:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *fakeSotWStream) Recv() (*discovery.DiscoveryRequest, error) {
	select {
	case req, ok := <-f.requests:
		if !ok {
			return nil, io.EOF
		}
		return req, nil
	case <-f.ctx.Done():
		return nil, io.EOF
	}
}

func (f *fakeSotWStream) Context() context.Context     { return f.ctx }
func (f *fakeSotWStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeSotWStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeSotWStream) SetTrailer(metadata.MD)       {}
func (f *fakeSotWStream) SendMsg(any) error            { return nil }
func (f *fakeSotWStream) RecvMsg(any) error            { return nil }
