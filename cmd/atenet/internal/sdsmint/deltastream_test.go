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
	"crypto/x509"
	"encoding/pem"
	"io"
	"testing"
	"testing/synctest"
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
	case <-time.After(respondWait):
		t.Fatal("timed out waiting for a DeltaDiscoveryResponse")
		return nil
	}
}

// quiet blocks until the server has nothing left to do, then fails if it sent
// anything. This is the negative assertion the whole file used to spell as a
// sleep: synctest.Wait returns once every goroutine is durably blocked, so
// anything the server meant to send is already in the channel by then.
func (f *fakeDeltaStream) quiet(t *testing.T, whileDoing string) {
	t.Helper()
	synctest.Wait()
	select {
	case resp := <-f.sent:
		t.Fatalf("server sent %v %s", resourceNames(resp), whileDoing)
	default:
	}
}

// drain empties whatever is already queued. Pair it with a Wait, or it races
// the sender it is trying to get ahead of.
func (f *fakeDeltaStream) drain() {
	synctest.Wait()
	for len(f.sent) > 0 {
		<-f.sent
	}
}

// startServer runs DeltaSecrets against a fake stream and returns the stream
// plus a func that stops it and reports the server's error.
func startServer(t *testing.T, srv *server) (*fakeDeltaStream, func() error) {
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
		case <-time.After(respondWait):
			t.Fatal("DeltaSecrets did not return after the stream was cancelled")
			return nil
		}
	}
}

func resourceNames(resp *discovery.DeltaDiscoveryResponse) []string {
	names := make([]string, 0, len(resp.GetResources()))
	for _, r := range resp.GetResources() {
		names = append(names, r.GetName())
	}
	return names
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

// leafFromResource pulls the leaf x509 out of a delta Resource carrying a
// Secret, so a test can reason about the certificate Envoy would actually
// serve rather than just the xDS version string.
func leafFromResource(t *testing.T, res *discovery.Resource) *x509.Certificate {
	t.Helper()
	secret := unpackSecret(t, res)
	chain := secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes()
	block, _ := pem.Decode(chain)
	if block == nil {
		t.Fatalf("resource %q: certificate chain is not PEM", res.GetName())
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("resource %q: parsing leaf: %v", res.GetName(), err)
	}
	return leaf
}

func TestDeltaSecretsMintsSubscribedName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}

		resp := stream.nextResponse(t)
		if resp.GetTypeUrl() != secretTypeURL {
			t.Errorf("type_url = %q, want %q", resp.GetTypeUrl(), secretTypeURL)
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
	})
}

func TestDeltaSecretsWithdrawsRefusedName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		// The minter refuses names, not destinations: "*.evil.test" is turned away
		// for being a wildcard rather than for being anyone in particular. What
		// this pins is the partial response -- one name refused must not cost the
		// other name in the same subscription its certificate.
		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"ok.allowed", "*.evil.test"},
		}

		resp := stream.nextResponse(t)

		if len(resp.GetResources()) != 1 || resp.GetResources()[0].GetName() != "ok.allowed" {
			t.Errorf("resources = %v, want just ok.allowed", resourceNames(resp))
		}
		// A server cannot NACK in xDS. Withdrawing the name is how it says "this
		// will not be issued", and per the Envoy docs it also cancels the
		// data-plane subscription for that name.
		if got := resp.GetRemovedResources(); len(got) != 1 || got[0] != "*.evil.test" {
			t.Errorf("removed_resources = %v, want [*.evil.test]", got)
		}
	})
}

func TestDeltaSecretsBareAckSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		first := stream.nextResponse(t)

		// Envoy's ACK carries the nonce and no subscriptions. Replying to it
		// would start an infinite ACK loop.
		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:       secretTypeURL,
			ResponseNonce: first.GetNonce(),
		}
		stream.quiet(t, "in reply to a bare ACK")
	})
}

// TestDeltaSecretsNeverPushesUnprompted pins the shape of the server after
// rotation was removed: with no idle timeout, a subscribe is answered once and
// then the stream is silent for the whole life of the leaf and beyond. Nothing
// re-mints a name in place, so a leaf expires under a live subscription --
// that is a known consequence of the removal, not an accident, and this is
// where it is written down.
func TestDeltaSecretsNeverPushesUnprompted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The TTL testServer's minter is built with.
		const ttl = defaultTTL
		srv := testServer(t, serverOptions{})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		stream.nextResponse(t)

		// Well past the point where the leaf has expired. Free on a fake clock.
		time.Sleep(2 * ttl)
		stream.quiet(t, "after the only subscribed leaf had expired")
	})
}

// TestDeltaSecretsUnsubscribeDropsTheName checks that an unsubscribe actually
// removes the name from the stream's set rather than merely being accepted.
// The idle sweep is what makes that observable: a name the server still held
// would be withdrawn once it aged out, and a name it has forgotten cannot be.
func TestDeltaSecretsUnsubscribeDropsTheName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{IdleTimeout: testIdle})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		stream.nextResponse(t)

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                  secretTypeURL,
			ResourceNamesUnsubscribe: []string{"a.example"},
		}

		stream.drain()
		expectNoRemoval(t, stream, withdrawWait)
	})
}

// TestDeltaSecretsSeedsFromInitialResourceVersions covers the reconnect path.
// A replayed name has to land in the stream's set, or the server would treat
// it as unknown: an unsubscribe would match nothing and the idle sweep could
// never reach it. The sweep is what makes adoption observable -- an unadopted
// name is never withdrawn, because the server does not know it exists.
func TestDeltaSecretsSeedsFromInitialResourceVersions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{IdleTimeout: testIdle})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                 secretTypeURL,
			InitialResourceVersions: map[string]string{"resumed.example": "old-version"},
		}

		// No immediate response: nothing was subscribed in this request.
		stream.quiet(t, "in reply to a replay-only request")

		awaitRemoval(t, stream, "resumed.example", withdrawWait)
	})
}

func TestDeltaSecretsRejectsWrongTypeURL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{})

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
		case <-time.After(respondWait):
			t.Fatal("DeltaSecrets did not reject a non-SDS type_url")
		}
	})
}

func TestDeltaSecretsSurvivesNack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		first := stream.nextResponse(t)

		// A NACK must not tear down the stream; Envoy would just reconnect and
		// we would lose every live subscription.
		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:       secretTypeURL,
			ResponseNonce: first.GetNonce(),
			ErrorDetail:   &rpcstatus.Status{Code: 3, Message: "bad certificate"},
		}
		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"b.example"},
		}

		resp := stream.nextResponse(t)
		if names := resourceNames(resp); len(names) != 1 || names[0] != "b.example" {
			t.Errorf("after a NACK the server served %v, want [b.example]", names)
		}
	})
}

// The idle sweep, withdrawIdle's half of the file.
//
// The sweep interval is idleSweepDivisor per window, floored at 250ms, so any
// window under a second lands on the floor instead of the divisor. These tests
// run on a fake clock, so a window long enough to keep the divisor in play
// costs nothing: 4s means sweeps every 1s and a withdrawal at 5s.
const testIdle = 4 * time.Second

// withdrawWait bounds a wait for the sweep. Generous, because fake time makes
// it free, and the only run that spends it is one that is already failing.
const withdrawWait = 10 * testIdle

// awaitRemoval waits for a response withdrawing name, returning how long it
// took. Resources arriving in the meantime are ignored: a sweep can land
// between a subscribe and the mints answering it.
func awaitRemoval(t *testing.T, stream *fakeDeltaStream, name string, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	deadline := time.After(within)
	for {
		select {
		case resp := <-stream.sent:
			for _, got := range resp.GetRemovedResources() {
				if got == name {
					return time.Since(start)
				}
			}
		case <-deadline:
			t.Fatalf("%q was never withdrawn within %v", name, within)
			return 0
		}
	}
}

// expectNoRemoval fails if anything is withdrawn during the window.
func expectNoRemoval(t *testing.T, stream *fakeDeltaStream, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case resp := <-stream.sent:
			if r := resp.GetRemovedResources(); len(r) > 0 {
				t.Fatalf("unexpected withdrawal of %v", r)
			}
		case <-deadline:
			return
		}
	}
}

// TestIdleNameIsWithdrawn is the core of it: a name nobody asks about again
// goes away on its own.
func TestIdleNameIsWithdrawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{IdleTimeout: testIdle})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		resp := stream.nextResponse(t)
		if len(resp.GetResources()) != 1 {
			t.Fatalf("initial response carried %d resources, want 1", len(resp.GetResources()))
		}

		took := awaitRemoval(t, stream, "a.example", withdrawWait)
		t.Logf("withdrawn %v after the subscribe (idle timeout %v)", took.Round(time.Millisecond), testIdle)

		// Early withdrawal would mean the name was reclaimed while the client
		// was arguably still interested, which is the failure mode that turns
		// this from reclamation into churn.
		if took < testIdle {
			t.Errorf("withdrawn after %v, before the %v idle timeout elapsed", took, testIdle)
		}
	})
}

// TestIdleWithdrawalIsOffByDefault pins the default down, because it is the
// difference between this PoC's measured behavior and the new one.
func TestIdleWithdrawalIsOffByDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		stream.nextResponse(t)
		expectNoRemoval(t, stream, 4*testIdle)
	})
}

// TestSubscribeKeepsANameAlive checks the one signal a client has. Envoy
// re-subscribes when it needs a name it does not hold; if that did not reset
// the clock, a name could be withdrawn moments after being asked for.
func TestSubscribeKeepsANameAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{IdleTimeout: testIdle})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		stream.nextResponse(t)

		// Re-subscribe comfortably inside the window, for longer than the
		// window itself, then assert nothing was withdrawn along the way.
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 8; i++ {
				time.Sleep(testIdle / 3)
				select {
				case stream.requests <- &discovery.DeltaDiscoveryRequest{
					TypeUrl:                secretTypeURL,
					ResourceNamesSubscribe: []string{"a.example"},
				}:
				case <-time.After(time.Second):
					return
				}
			}
		}()
		expectNoRemoval(t, stream, 3*testIdle)
		<-done
	})
}

// TestResyncKeepsANameAlive covers the reconnect path. Envoy replays its live
// set in initial_resource_versions on a new stream; the names in that replay
// are names it is still holding, so the fresh stream must not immediately
// withdraw them.
func TestResyncKeepsANameAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{IdleTimeout: testIdle})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                  secretTypeURL,
			InitialResourceVersions:  map[string]string{"a.example": "1"},
			ResourceNamesSubscribe:   nil,
			ResourceNamesUnsubscribe: nil,
		}

		// It should survive the first sweep after the replay, then age out like
		// anything else: the replay is a touch, not a lease.
		expectNoRemoval(t, stream, testIdle/2)
		awaitRemoval(t, stream, "a.example", withdrawWait)
	})
}

// TestWithdrawnNameIsServedAgainOnRequest is the safety property, and with
// rotation gone it is also the refresh path: withdrawal must cost a re-fetch
// and nothing more, or reclamation is an outage with a schedule. A host that
// gets busy again after a quiet hour has to work, and a host in continuous use
// only ever gets a new certificate this way.
func TestWithdrawnNameIsServedAgainOnRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := testServer(t, serverOptions{IdleTimeout: testIdle})
		stream, stop := startServer(t, srv)
		defer func() {
			if err := stop(); err != nil {
				t.Errorf("DeltaSecrets returned %v", err)
			}
		}()

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		first := stream.nextResponse(t)
		if len(first.GetResources()) != 1 {
			t.Fatalf("initial response carried %d resources, want 1", len(first.GetResources()))
		}
		awaitRemoval(t, stream, "a.example", withdrawWait)

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example"},
		}
		for {
			resp := stream.nextResponse(t)
			if len(resp.GetResources()) == 0 {
				continue
			}
			if got := resp.GetResources()[0].GetName(); got != "a.example" {
				t.Fatalf("re-fetch returned %q, want a.example", got)
			}
			leaf := leafFromResource(t, resp.GetResources()[0])
			if err := leaf.VerifyHostname("a.example"); err != nil {
				t.Fatalf("re-minted leaf does not cover a.example: %v", err)
			}
			return
		}
	})
}
