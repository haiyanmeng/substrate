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
	"sync"
	"testing"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

// The sweep interval is floored at 250ms, so an idle window shorter than that
// buys nothing: reclamation lag is then set by the floor. 600ms gives the tests
// a name that goes idle in under a second while still exercising the
// divide-by-four path.
const testIdle = 600 * time.Millisecond

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
//
// This is what makes the live set bounded. With IdleTimeout at zero -- the
// default, and what phase 6 measured before this existed -- a secret Envoy
// fetched once is held for the life of the stream at ~60KB of proxy RSS
// apiece, so the footprint tracks every distinct host ever contacted rather
// than the ones in use.
func TestIdleNameIsWithdrawn(t *testing.T) {
	metrics := &Metrics{}
	srv := testServer(t, ServerOptions{IdleTimeout: testIdle, Metrics: metrics}, []string{"*.example"})
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
	if len(resp.GetResources()) != 1 {
		t.Fatalf("initial response carried %d resources, want 1", len(resp.GetResources()))
	}
	if got := metrics.Snapshot()["names_live"]; got != 1 {
		t.Fatalf("names_live = %d after the subscribe, want 1", got)
	}

	took := awaitRemoval(t, stream, "a.example", 5*time.Second)
	t.Logf("withdrawn %v after the subscribe (idle timeout %v)", took.Round(time.Millisecond), testIdle)

	// Early withdrawal would mean the name was reclaimed while the client was
	// arguably still interested, which is the failure mode that turns this
	// from reclamation into churn.
	if took < testIdle {
		t.Errorf("withdrawn after %v, before the %v idle timeout elapsed", took, testIdle)
	}

	snap := metrics.Snapshot()
	if snap["names_live"] != 0 {
		t.Errorf("names_live = %d after the withdrawal, want 0", snap["names_live"])
	}
	if snap["idle_withdrawals"] != 1 {
		t.Errorf("idle_withdrawals = %d, want 1", snap["idle_withdrawals"])
	}
	if snap["removals_sent"] != 1 {
		t.Errorf("removals_sent = %d, want 1", snap["removals_sent"])
	}
}

// TestIdleWithdrawalIsOffByDefault pins the default down, because it is the
// difference between this PoC's measured behavior and the new one. Phase 6's
// control arm depends on it.
func TestIdleWithdrawalIsOffByDefault(t *testing.T) {
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
	stream.nextResponse(t)
	expectNoRemoval(t, stream, 4*testIdle)
}

// TestSubscribeKeepsANameAlive checks the one signal a client has. Envoy
// re-subscribes when it needs a name it does not hold; if that did not reset
// the clock, a name could be withdrawn moments after being asked for.
func TestSubscribeKeepsANameAlive(t *testing.T) {
	srv := testServer(t, ServerOptions{IdleTimeout: testIdle}, []string{"*.example"})
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

	// Re-subscribe comfortably inside the window, for longer than the window
	// itself, then assert nothing was withdrawn along the way.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 8; i++ {
			time.Sleep(testIdle / 3)
			select {
			case stream.requests <- &discovery.DeltaDiscoveryRequest{
				TypeUrl:                SecretTypeURL,
				ResourceNamesSubscribe: []string{"a.example"},
			}:
			case <-time.After(time.Second):
				return
			}
		}
	}()
	expectNoRemoval(t, stream, 3*testIdle)
	<-done
}

// TestResyncKeepsANameAlive covers the reconnect path. Envoy replays its live
// set in initial_resource_versions on a new stream; the names in that replay
// are names it is still holding, so the fresh stream must not immediately
// withdraw them.
func TestResyncKeepsANameAlive(t *testing.T) {
	srv := testServer(t, ServerOptions{IdleTimeout: testIdle}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                  SecretTypeURL,
		InitialResourceVersions:  map[string]string{"a.example": "1"},
		ResourceNamesSubscribe:   nil,
		ResourceNamesUnsubscribe: nil,
	}

	// It should survive the first sweep after the replay, then age out like
	// anything else: the replay is a touch, not a lease.
	expectNoRemoval(t, stream, testIdle/2)
	awaitRemoval(t, stream, "a.example", 5*time.Second)
}

// TestRotationDoesNotKeepAnIdleNameAlive is the interaction that would quietly
// defeat the whole thing. Rotation walks every live name and re-mints it; if
// that counted as activity, a server with --rotate on would never reclaim
// anything and the idle timeout would look like it worked while doing nothing.
func TestRotationDoesNotKeepAnIdleNameAlive(t *testing.T) {
	// Rotation fires at 2/3 TTL, floored at 1s, so a 2s TTL means a tick lands
	// inside the window this test watches.
	srv := testServer(t, ServerOptions{
		Rotate:      true,
		TTL:         2 * time.Second,
		IdleTimeout: testIdle,
	}, []string{"*.example"})
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
	awaitRemoval(t, stream, "a.example", 5*time.Second)
}

// TestWithdrawnNameIsServedAgainOnRequest is the safety property. Withdrawal
// must cost a re-fetch and nothing more: a host that gets busy again after a
// quiet hour has to work, or reclamation is an outage with a schedule.
func TestWithdrawnNameIsServedAgainOnRequest(t *testing.T) {
	srv := testServer(t, ServerOptions{IdleTimeout: testIdle}, []string{"*.example"})
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
	if len(first.GetResources()) != 1 {
		t.Fatalf("initial response carried %d resources, want 1", len(first.GetResources()))
	}
	awaitRemoval(t, stream, "a.example", 5*time.Second)

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
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
}

// forgetRecorder wraps a Minter to observe the Forget calls the server makes.
type forgetRecorder struct {
	Minter
	mu        sync.Mutex
	forgotten []string
}

func (f *forgetRecorder) Forget(host string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, host)
	return true
}

func (f *forgetRecorder) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgotten...)
}

// TestWithdrawalReleasesTheMinterCache checks that reclamation is not
// one-sided. Withdrawing from Envoy while the signer keeps the leaf cached
// moves the memory rather than releasing it.
func TestWithdrawalReleasesTheMinterCache(t *testing.T) {
	rec := &forgetRecorder{Minter: testMinter(t, MinterOptions{TTL: time.Minute})}
	srv := NewServer(rec, ServerOptions{
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
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}
	stream.nextResponse(t)
	awaitRemoval(t, stream, "a.example", 5*time.Second)

	got := rec.calls()
	if len(got) != 1 || got[0] != "a.example" {
		t.Errorf("Forget calls = %v, want [a.example]", got)
	}
}

// TestMinterForgetDropsTheCachedLeaf is the other half: that Forget on the
// real minter actually releases, rather than merely being called.
func TestMinterForgetDropsTheCachedLeaf(t *testing.T) {
	m := testMinter(t, MinterOptions{TTL: time.Minute})
	f, ok := m.(Forgetter)
	if !ok {
		t.Fatalf("%T does not implement Forgetter; the server's reclamation path is a no-op", m)
	}
	ctx := context.Background()

	first, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	again, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if again.Serial != first.Serial {
		t.Fatalf("second call minted a new leaf (%s -> %s); the cache is not working, so this test cannot show anything",
			first.Serial, again.Serial)
	}

	if !f.Forget("a.example") {
		t.Error("Forget reported nothing held for a name that was just cached")
	}
	if f.Forget("a.example") {
		t.Error("Forget reported a hit on a name it had already released")
	}

	after, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("GetCertificate after Forget: %v", err)
	}
	if after.Serial == first.Serial {
		t.Errorf("still serving serial %s after Forget; the leaf was not released", after.Serial)
	}
}
