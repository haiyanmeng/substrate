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

func TestDeltaSecretsRotationPushesNewVersion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Envoy has no TTL of its own for an on-demand secret, so the server has
		// to push a replacement or the leaf silently expires under a live
		// subscription.
		const ttl = defaultTTL
		srv := testServer(t, serverOptions{TTL: ttl})
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
		firstVersion := first.GetResources()[0].GetVersion()

		// One tick is enough; the second is slack. This test only cares that the
		// version moves, not when -- the timing relationship itself is
		// TestRotationNeverServesAnExpiredLeaf's job.
		deadline := time.After(2 * rotateInterval(ttl))
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
	})
}

// push is one observed rotation push: when it landed on the stream, which
// certificate it carried, and when that certificate stops being valid.
type push struct {
	at       time.Time
	serial   string
	notAfter time.Time
}

// TestRotationNeverServesAnExpiredLeaf pins down the interaction between two
// clocks that were chosen independently: the rotation ticker (2/3 of TTL) and
// the minter's cache entry lifetime (a full TTL).
//
// Envoy holds whatever leaf we last pushed until we push another one -- it has
// no expiry of its own for an on-demand secret. So the invariant that matters
// is not "we tick often enough", it is "every leaf is replaced before its own
// notAfter". This test collects the push timeline and checks exactly that.
func TestRotationNeverServesAnExpiredLeaf(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The production default. Nothing pushes back on the TTL any more:
		// the server's 1s rotation floor and x509's one-second notAfter
		// granularity are what used to rule out a short TTL, and on a fake
		// clock waiting out a long one is free.
		const ttl = defaultTTL
		// Rotation fires at 10m and 20m, so this covers two of them. Under the
		// pre-fix behavior the tick at 10m was a cache hit that re-sent the old
		// leaf, and the replacement did not arrive until 20m -- five minutes
		// after that leaf had expired. That is the case this window is sized to
		// catch.
		const observe = 25 * time.Minute

		srv := testServer(t, serverOptions{TTL: ttl})
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

		var pushes []push
		deadline := time.After(observe)
	collect:
		for {
			select {
			case resp := <-stream.sent:
				for _, res := range resp.GetResources() {
					if res.GetName() != "a.example" {
						continue
					}
					leaf := leafFromResource(t, res)
					pushes = append(pushes, push{
						at:       time.Now(),
						serial:   res.GetVersion(),
						notAfter: leaf.NotAfter,
					})
				}
			case <-deadline:
				break collect
			}
		}

		if len(pushes) < 2 {
			t.Fatalf("only %d pushes in %v; expected the initial mint plus at least one rotation", len(pushes), observe)
		}

		// Report the timeline unconditionally: when this test fails the shape of
		// the failure is the finding, not the fact of it.
		start := pushes[0].at
		for i, p := range pushes {
			t.Logf("push %d at t+%-8v serial=%s valid until t+%v",
				i, p.at.Sub(start).Round(time.Millisecond), p.serial,
				p.notAfter.Sub(start).Round(time.Millisecond))
		}

		// Each leaf is served from the moment it is pushed until the next push
		// replaces it. If the next push lands after the current leaf's notAfter,
		// Envoy served an expired certificate for the difference.
		var worst time.Duration
		for i := 0; i < len(pushes)-1; i++ {
			if gap := pushes[i+1].at.Sub(pushes[i].notAfter); gap > worst {
				worst = gap
			}
		}
		if worst > 0 {
			t.Errorf("served an expired leaf for %v: a push landed that long after the "+
				"previous leaf's notAfter (TTL %v, rotation interval %v)",
				worst.Round(time.Millisecond), ttl, time.Duration(float64(ttl)*rotateFraction))
		}

		// The last leaf we pushed must still have been valid when we stopped
		// watching, or the run ended inside a staleness window the loop above
		// cannot see (there is no "next push" to measure against).
		if last := pushes[len(pushes)-1]; last.notAfter.Before(time.Now()) {
			t.Errorf("the most recently pushed leaf (serial %s) expired %v ago and nothing has replaced it",
				last.serial, time.Since(last.notAfter).Round(time.Millisecond))
		}
	})
}

func TestDeltaSecretsUnsubscribeStopsRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const ttl = defaultTTL
		srv := testServer(t, serverOptions{TTL: ttl})
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

		// Drain anything already queued, then sit through two rotation ticks
		// that must produce nothing.
		stream.drain()
		time.Sleep(2 * rotateInterval(ttl))
		stream.quiet(t, "for a name that was unsubscribed")
	})
}

func TestDeltaSecretsSeedsFromInitialResourceVersions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// On reconnect Envoy replays what it already holds. The server must adopt
		// that state so rotation covers those names without re-pushing them.
		const ttl = defaultTTL
		srv := testServer(t, serverOptions{TTL: ttl})
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

		// But the resumed name must still get rotated.
		select {
		case resp := <-stream.sent:
			names := resourceNames(resp)
			if len(names) != 1 || names[0] != "resumed.example" {
				t.Fatalf("rotation covered %v, want [resumed.example]", names)
			}
		case <-time.After(2 * rotateInterval(ttl)):
			t.Fatal("resumed subscription was never rotated")
		}
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
// took. Resources arriving in the meantime are ignored -- with rotation on,
// pushes and withdrawals share the stream.
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
		counters, collect := newTestMetrics(t)
		srv := testServer(t, serverOptions{IdleTimeout: testIdle, Metrics: counters})
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
		if got := metricValue(t, collect(), namesActiveMetric); got != 1 {
			t.Fatalf("%s = %v after the subscribe, want 1", namesActiveMetric, got)
		}

		took := awaitRemoval(t, stream, "a.example", withdrawWait)
		t.Logf("withdrawn %v after the subscribe (idle timeout %v)", took.Round(time.Millisecond), testIdle)

		// Early withdrawal would mean the name was reclaimed while the client
		// was arguably still interested, which is the failure mode that turns
		// this from reclamation into churn.
		if took < testIdle {
			t.Errorf("withdrawn after %v, before the %v idle timeout elapsed", took, testIdle)
		}

		got := collect()
		for key, want := range map[string]float64{
			namesActiveMetric:                       0,
			idleWithdrawnMetric + "|count":          1, // one sweep withdrew something
			idleWithdrawnMetric + "|sum":            1, // and it withdrew one name
			responseEntriesMetric + "|kind=removal": 1,
		} {
			if v := metricValue(t, got, key); v != want {
				t.Errorf("%s = %v, want %v", key, v, want)
			}
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

// TestRotationDoesNotKeepAnIdleNameAlive is the interaction that would quietly
// defeat the whole thing. Rotation walks every live name and re-mints it; if
// that counted as activity, no server would ever reclaim anything and the idle
// timeout would look like it worked while doing nothing.
func TestRotationDoesNotKeepAnIdleNameAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Rotation fires at 2/3 TTL, floored at 1s, so a 2s TTL puts ticks at
		// 1.3s, 2.7s and 4s -- three of them ahead of the 5s withdrawal. That
		// ordering is the test: with nothing rotating before the sweep there is
		// no interaction here to observe.
		srv := testServer(t, serverOptions{
			TTL:         2 * time.Second,
			IdleTimeout: testIdle,
		})
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
		awaitRemoval(t, stream, "a.example", withdrawWait)
	})
}

// TestWithdrawnNameIsServedAgainOnRequest is the safety property. Withdrawal
// must cost a re-fetch and nothing more: a host that gets busy again after a
// quiet hour has to work, or reclamation is an outage with a schedule.
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

// TestWithdrawalReleasesTheMinterCache checks that reclamation is not
// one-sided. Withdrawing from Envoy while the signer keeps the leaf cached
// moves the memory rather than releasing it.
//
// It reads the minter's cache rather than counting Forget calls: the sweep
// forgets before it sends, so by the time the client sees the removal the
// release has already happened, and the cache is what the release is for.
func TestWithdrawalReleasesTheMinterCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := testMinter(t, minterOptions{TTL: time.Minute})
		srv := newServer(m, serverOptions{
			Logger:      quietLogger(),
			TTL:         time.Minute,
			IdleTimeout: testIdle,
		})
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
		awaitRemoval(t, stream, "a.example", withdrawWait)

		if got := m.cache.len(); got != 0 {
			t.Errorf("minter still holds %d leaves after the only subscribed name was withdrawn; want 0", got)
		}
	})
}

// TestStreamCloseReleasesTheMinterCache is the other half of that reclamation.
// Rotation and the idle sweep both run per stream, so a name still subscribed
// when the connection drops is reachable by neither: nothing re-fetches it and
// nothing sweeps it, and its leaf would sit in the shared minter cache until
// capacity evicted it.
func TestStreamCloseReleasesTheMinterCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := testMinter(t, minterOptions{TTL: time.Minute})
		// No idle timeout: the sweep must not be what does the releasing here.
		srv := newServer(m, serverOptions{
			Logger: quietLogger(),
			TTL:    time.Minute,
		})
		stream, stop := startServer(t, srv)

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example", "b.example"},
		}
		stream.nextResponse(t)

		if got := m.cache.len(); got == 0 {
			t.Fatal("nothing was cached, so this test cannot show a release")
		}

		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}

		if got := m.cache.len(); got != 0 {
			t.Errorf("minter still holds %d leaves after the stream closed; want 0", got)
		}
	})
}
