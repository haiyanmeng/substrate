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
	"testing/synctest"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestMetrics returns instruments backed by a reader of this test's own,
// plus a collect function that reads them back. It deliberately does not touch
// otel.SetMeterProvider: the global is process-wide, and tests that install
// into it cannot run in parallel or read only their own recordings.
func newTestMetrics(t *testing.T) (*metrics, func() map[string]float64) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down the meter provider: %v", err)
		}
	})

	m, err := newMetrics(mp)
	if err != nil {
		t.Fatalf("newMetrics: %v", err)
	}

	return m, func() map[string]float64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collecting metrics: %v", err)
		}
		return flattenMetrics(&rm)
	}
}

// flattenMetrics turns collected metrics into "name|attrs" keys, with
// histograms contributing "|count" and "|sum" separately -- the two halves the
// instrument choice leans on, since a histogram's count is the event and its
// sum is the size of it.
func flattenMetrics(rm *metricdata.ResourceMetrics) map[string]float64 {
	out := make(map[string]float64)
	key := func(name string, attrs attribute.Set) string {
		if attrs.Len() == 0 {
			return name
		}
		return name + "|" + attrs.Encoded(attribute.DefaultEncoder())
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					out[key(m.Name, dp.Attributes)] = float64(dp.Value)
				}
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					base := key(m.Name, dp.Attributes)
					out[base+"|count"] = float64(dp.Count)
					out[base+"|sum"] = float64(dp.Sum)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					base := key(m.Name, dp.Attributes)
					out[base+"|count"] = float64(dp.Count)
					out[base+"|sum"] = dp.Sum
				}
			}
		}
	}
	return out
}

// metricValue reads one series, failing if nothing recorded it. Asserting that
// a gauge came back to zero must not be satisfiable by the series being absent.
func metricValue(t *testing.T, got map[string]float64, key string) float64 {
	t.Helper()
	v, ok := got[key]
	if !ok {
		t.Fatalf("no series %q was recorded; collected %v", key, got)
	}
	return v
}

// Counters are optional, and every call site passes whatever it was given
// without checking. A nil metrics that panicked would take the server down in
// exactly the configuration that is not being measured.
func TestMetricsIsNilSafe(t *testing.T) {
	ctx := context.Background()
	var m *metrics
	m.recordMint(ctx, time.Second)
	m.recordCacheHit(ctx)
	m.recordDenial(ctx)
	m.recordStreamOpen(ctx)
	m.recordStreamClose(ctx, 3)
	m.recordSubscribe(ctx, 2)
	m.recordUnsubscribe(ctx, 1)
	m.addLiveNames(ctx, 4)
	m.recordNACK(ctx)
	m.recordResync(ctx, 5)
	m.recordResponse(ctx, 100, 2, 1)
	m.recordIdleWithdrawal(ctx, 2)
	m.recordRotation(ctx, time.Second, 7)
	if m.enabled() {
		t.Error("a nil metrics reported itself as enabled")
	}
}

func TestMetricsCountsMintsAndCacheHits(t *testing.T) {
	counters, collect := newTestMetrics(t)
	m, err := newMinter(testSigner(t), minterOptions{
		TTL:     time.Minute,
		Logger:  quietLogger(),
		Metrics: counters,
	})
	if err != nil {
		t.Fatalf("newMinter: %v", err)
	}
	ctx := context.Background()

	if _, err := m.certificate(ctx, "a.example"); err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	for range 3 {
		if _, err := m.certificate(ctx, "a.example"); err != nil {
			t.Fatalf("GetCertificate: %v", err)
		}
	}
	if _, err := m.certificate(ctx, "*.evil.test"); err == nil {
		t.Fatal("a wildcard SNI reached the signer")
	}

	got := collect()
	for key, want := range map[string]float64{
		certRequestsMetric + "|outcome=minted":    1,
		certRequestsMetric + "|outcome=cache_hit": 3,
		certRequestsMetric + "|outcome=denied":    1,
		signDurationMetric + "|count":             1,
	} {
		if v := metricValue(t, got, key); v != want {
			t.Errorf("%s = %v, want %v", key, v, want)
		}
	}
	// Only the one mint is timed; a cache hit or a denial must not reach the
	// signing histogram.
	if v := metricValue(t, got, signDurationMetric+"|sum"); v <= 0 {
		t.Errorf("%s|sum = %v, want a positive signing time", signDurationMetric, v)
	}
}

// names.active is the gauge the scalability phases read to confirm Envoy's live
// secret set is what the harness thinks it is. It has to survive the whole
// subscribe / withdraw / stream-teardown cycle without drifting.
func TestMetricsTracksLiveNamesAcrossAStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counters, collect := newTestMetrics(t)
		srv := testServer(t, serverOptions{Metrics: counters})
		stream, stop := startServer(t, srv)

		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"a.example", "b.example"},
		}
		stream.nextResponse(t)
		if got := metricValue(t, collect(), namesActiveMetric); got != 2 {
			t.Fatalf("%s = %v after two subscriptions, want 2", namesActiveMetric, got)
		}

		// A refused name must not count: it was never issued.
		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                secretTypeURL,
			ResourceNamesSubscribe: []string{"*.evil.test"},
		}
		stream.nextResponse(t)
		if got := metricValue(t, collect(), namesActiveMetric); got != 2 {
			t.Fatalf("%s = %v after a refusal, want it unchanged at 2", namesActiveMetric, got)
		}

		// An unsubscribe draws no response, so there is nothing to read as the
		// signal that it landed. Wait for the server to go quiet instead: once it
		// is blocked again, it is done with the request.
		stream.requests <- &discovery.DeltaDiscoveryRequest{
			TypeUrl:                  secretTypeURL,
			ResourceNamesUnsubscribe: []string{"a.example"},
		}
		synctest.Wait()
		if got := metricValue(t, collect(), namesActiveMetric); got != 1 {
			t.Fatalf("%s = %v after an unsubscribe, want 1", namesActiveMetric, got)
		}

		// Whatever a stream was still holding dies with it, so the gauge has to
		// come all the way back down or a reconnect churn leaks into the total.
		if err := stop(); err != nil {
			t.Fatalf("DeltaSecrets returned %v", err)
		}
		got := collect()
		for key, want := range map[string]float64{
			streamsActiveMetric:                         0,
			namesActiveMetric:                           0,
			subscriptionsMetric + "|action=subscribe":   3,
			subscriptionsMetric + "|action=unsubscribe": 1,
		} {
			if v := metricValue(t, got, key); v != want {
				t.Errorf("%s = %v, want %v", key, v, want)
			}
		}
		if v := metricValue(t, got, responseSizeMetric+"|sum"); v <= 0 {
			t.Errorf("%s|sum = %v, want the responses to have had a size", responseSizeMetric, v)
		}
	})
}
