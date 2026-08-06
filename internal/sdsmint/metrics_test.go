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
	"testing"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

// Counters are optional, and every call site passes whatever it was given
// without checking. A nil Metrics that panicked would take the server down in
// exactly the configuration that is not being measured.
func TestMetricsIsNilSafe(t *testing.T) {
	var m *Metrics
	m.recordMint(time.Second)
	m.recordCacheHit()
	m.recordDenial()
	m.recordStreamOpen()
	m.recordStreamClose(3)
	m.recordSubscribe(2)
	m.recordUnsubscribe(1)
	m.addLiveNames(4)
	m.recordNACK()
	m.recordResync(5)
	m.recordResponse(100, 2, 1)
	m.recordRotation(time.Second, 7)
	if m.enabled() {
		t.Error("a nil Metrics reported itself as enabled")
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot on a nil Metrics = %v; want empty", got)
	}
}

func TestMetricsCountsMintsAndCacheHits(t *testing.T) {
	metrics := &Metrics{}
	m, err := NewMinter(testCA(t), MinterOptions{
		Validate: AllowGlobs([]string{"*.example"}),
		TTL:      time.Minute,
		Logger:   quietLogger(),
		Metrics:  metrics,
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	ctx := context.Background()

	if _, err := m.GetCertificate(ctx, "a.example"); err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	for range 3 {
		if _, err := m.GetCertificate(ctx, "a.example"); err != nil {
			t.Fatalf("GetCertificate: %v", err)
		}
	}
	if _, err := m.GetCertificate(ctx, "evil.test"); err == nil {
		t.Fatal("the allowlist let a bad host through")
	}

	s := metrics.Snapshot()
	if s["mints_issued"] != 1 {
		t.Errorf("mints_issued = %d, want 1", s["mints_issued"])
	}
	if s["cache_hits"] != 3 {
		t.Errorf("cache_hits = %d, want 3", s["cache_hits"])
	}
	if s["mints_denied"] != 1 {
		t.Errorf("mints_denied = %d, want 1", s["mints_denied"])
	}
	if s["sign_nanos_avg"] <= 0 {
		t.Errorf("sign_nanos_avg = %d, want a positive signing time", s["sign_nanos_avg"])
	}
}

// names_live is the gauge the scalability phases read to confirm Envoy's live
// secret set is what the harness thinks it is. It has to survive the whole
// subscribe / withdraw / stream-teardown cycle without drifting.
func TestMetricsTracksLiveNamesAcrossAStream(t *testing.T) {
	metrics := &Metrics{}
	srv := testServer(t, ServerOptions{Metrics: metrics}, []string{"*.example"})
	stream, stop := startServer(t, srv)

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example", "b.example"},
	}
	stream.nextResponse(t)
	if got := metrics.Snapshot()["names_live"]; got != 2 {
		t.Fatalf("names_live = %d after two subscriptions, want 2", got)
	}

	// A refused name must not count: it was never issued.
	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"evil.test"},
	}
	stream.nextResponse(t)
	if got := metrics.Snapshot()["names_live"]; got != 2 {
		t.Fatalf("names_live = %d after a refusal, want it unchanged at 2", got)
	}

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                  SecretTypeURL,
		ResourceNamesUnsubscribe: []string{"a.example"},
	}
	if err := waitFor(func() bool { return metrics.Snapshot()["names_live"] == 1 }); err != nil {
		t.Fatalf("names_live after an unsubscribe: %v (snapshot %v)", err, metrics.Snapshot())
	}

	// Whatever a stream was still holding dies with it, so the gauge has to
	// come all the way back down or a reconnect churn leaks into the total.
	if err := stop(); err != nil {
		t.Fatalf("DeltaSecrets returned %v", err)
	}
	s := metrics.Snapshot()
	if s["streams_live"] != 0 || s["names_live"] != 0 {
		t.Errorf("after the stream ended: streams_live=%d names_live=%d, want 0 and 0", s["streams_live"], s["names_live"])
	}
	if s["response_bytes_max"] <= 0 {
		t.Error("no response bytes were counted")
	}
	if s["subscribes"] != 3 || s["unsubscribes"] != 1 {
		t.Errorf("subscribes=%d unsubscribes=%d, want 3 and 1", s["subscribes"], s["unsubscribes"])
	}
}

func waitFor(cond func() bool) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return context.DeadlineExceeded
}
