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
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Attribute sets, built once. Every one of these is on a hot path -- a
// certificate request happens per SNI, a response per push -- and
// metric.WithAttributes allocates.
var (
	outcomeMinted   = metric.WithAttributes(attribute.String("outcome", "minted"))
	outcomeCacheHit = metric.WithAttributes(attribute.String("outcome", "cache_hit"))
	outcomeDenied   = metric.WithAttributes(attribute.String("outcome", "denied"))

	actionSubscribe   = metric.WithAttributes(attribute.String("action", "subscribe"))
	actionUnsubscribe = metric.WithAttributes(attribute.String("action", "unsubscribe"))

	entryResource = metric.WithAttributes(attribute.String("kind", "resource"))
	entryRemoval  = metric.WithAttributes(attribute.String("kind", "removal"))
)

// metrics is what the scalability harness needs to attribute a bottleneck.
//
// Without it the only server-side signal is the issuance log, and parsing logs
// at rate is both slow and a perturbation of the thing being measured. A nil
// *metrics is safe: every method becomes a no-op, so the instruments stay
// optional.
//
// Several counters are histograms whose count is the event and whose sum is
// the size of it -- resyncNames is both "how many reconnects" and "how many
// names they carried". That is one instrument where the hand-rolled version
// needed two, and it comes with a distribution the pair could not express.
type metrics struct {
	certRequests metric.Int64Counter
	signDuration metric.Float64Histogram

	streamsOpened metric.Int64Counter
	streamsActive metric.Int64UpDownCounter
	subscriptions metric.Int64Counter
	namesActive   metric.Int64UpDownCounter
	nacks         metric.Int64Counter
	// resyncNames counts requests carrying initial_resource_versions, which is
	// how Envoy replays its live set after the stream drops, and sums the names
	// those replays carried -- the cost of a reconnect.
	resyncNames metric.Int64Histogram

	responseSize    metric.Int64Histogram
	responseEntries metric.Int64Counter

	// idleWithdrawn counts sweeps that withdrew something, not sweeps that ran
	// -- a tick that found nothing idle is not an event worth recording.
	idleWithdrawn metric.Int64Histogram

	rotationDuration  metric.Float64Histogram
	rotationResources metric.Int64Histogram
}

// serviceName is the OpenTelemetry service name and instrumentation scope for
// sdsmint. It runs as its own container beside the router rather than inside
// it, so it reports under its own name instead of extproc.ServiceName.
const serviceName = "atenet-sdsmint"

// Instrument names. Dotted, per the OTel convention the rest of the repo
// follows; the Prometheus exporter is what turns them into underscores.
const (
	certRequestsMetric      = "atenet.sdsmint.certificate.requests"
	signDurationMetric      = "atenet.sdsmint.sign.duration"
	streamsOpenedMetric     = "atenet.sdsmint.streams.opened"
	streamsActiveMetric     = "atenet.sdsmint.streams.active"
	subscriptionsMetric     = "atenet.sdsmint.subscriptions"
	namesActiveMetric       = "atenet.sdsmint.names.active"
	nacksMetric             = "atenet.sdsmint.nacks"
	resyncNamesMetric       = "atenet.sdsmint.resync.names"
	responseSizeMetric      = "atenet.sdsmint.response.size"
	responseEntriesMetric   = "atenet.sdsmint.response.entries"
	idleWithdrawnMetric     = "atenet.sdsmint.idle.withdrawn"
	rotationDurationMetric  = "atenet.sdsmint.rotation.duration"
	rotationResourcesMetric = "atenet.sdsmint.rotation.resources"
)

// newMetrics creates the instruments on mp. Taking the provider rather than
// reaching for otel.GetMeterProvider keeps tests off a process-wide singleton
// they would have to install and undo.
func newMetrics(mp metric.MeterProvider) (*metrics, error) {
	meter := mp.Meter(serviceName)

	// Collected rather than returned one at a time: naming every instrument
	// that failed beats reporting the first and hiding the rest.
	var errs []error
	counter := func(name, unit, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s counter: %w", name, err))
		}
		return c
	}
	upDown := func(name, unit, desc string) metric.Int64UpDownCounter {
		c, err := meter.Int64UpDownCounter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s up-down counter: %w", name, err))
		}
		return c
	}
	hist := func(name, unit, desc string, bounds ...float64) metric.Int64Histogram {
		h, err := meter.Int64Histogram(name,
			metric.WithUnit(unit),
			metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(bounds...),
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s histogram: %w", name, err))
		}
		return h
	}
	secondsHist := func(name, desc string, bounds ...float64) metric.Float64Histogram {
		h, err := meter.Float64Histogram(name,
			metric.WithUnit("s"),
			metric.WithDescription(desc),
			metric.WithExplicitBucketBoundaries(bounds...),
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s histogram: %w", name, err))
		}
		return h
	}

	m := &metrics{
		certRequests: counter(certRequestsMetric, "{request}",
			"certificate requests by outcome: minted, served from cache, or denied by hostname syntax"),
		// Signing a P-256 leaf is a few hundred microseconds, so the buckets
		// start well below a millisecond; the top end is there to catch a CA
		// key that turned out to be RSA.
		signDuration: secondsHist(signDurationMetric,
			"time spent signing one leaf certificate",
			0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1),

		streamsOpened: counter(streamsOpenedMetric, "{stream}",
			"delta SDS streams opened since start"),
		streamsActive: upDown(streamsActiveMetric, "{stream}",
			"delta SDS streams currently open"),
		subscriptions: counter(subscriptionsMetric, "{name}",
			"resource names subscribed and unsubscribed by the data plane"),
		namesActive: upDown(namesActiveMetric, "{name}",
			"resource names currently held across all streams"),
		nacks: counter(nacksMetric, "{response}",
			"responses Envoy rejected with error_detail"),
		resyncNames: hist(resyncNamesMetric, "{name}",
			"names replayed in initial_resource_versions per reconnect; the count is reconnects",
			1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500),

		responseSize: hist(responseSizeMetric, "By",
			"serialized size of each DeltaDiscoveryResponse",
			1024, 4096, 16384, 65536, 262144, 1048576, 4194304),
		responseEntries: counter(responseEntriesMetric, "{entry}",
			"resources and removals carried by responses"),

		idleWithdrawn: hist(idleWithdrawnMetric, "{name}",
			"names withdrawn per idle sweep; the count is sweeps that withdrew something",
			1, 2, 5, 10, 25, 50, 100, 250, 500, 1024),

		rotationDuration: secondsHist(rotationDurationMetric,
			"time to re-mint and pack every name on one stream",
			0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30),
		rotationResources: hist(rotationResourcesMetric, "{resource}",
			"resources pushed per rotation tick; the count is ticks",
			1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// enabled reports whether recording will do anything. Callers use it to skip
// work that only exists to feed an instrument, such as sizing a response.
func (m *metrics) enabled() bool { return m != nil }

func (m *metrics) recordMint(ctx context.Context, d time.Duration) {
	if m == nil {
		return
	}
	m.certRequests.Add(ctx, 1, outcomeMinted)
	m.signDuration.Record(ctx, d.Seconds())
}

func (m *metrics) recordCacheHit(ctx context.Context) {
	if m != nil {
		m.certRequests.Add(ctx, 1, outcomeCacheHit)
	}
}

func (m *metrics) recordDenial(ctx context.Context) {
	if m != nil {
		m.certRequests.Add(ctx, 1, outcomeDenied)
	}
}

func (m *metrics) recordStreamOpen(ctx context.Context) {
	if m != nil {
		m.streamsOpened.Add(ctx, 1)
		m.streamsActive.Add(ctx, 1)
	}
}

// recordStreamClose also drops the names the stream was holding, since they
// die with it. namesActive is a gauge across all streams, so a stream that
// goes away without this would leak its whole subscription set into the total.
func (m *metrics) recordStreamClose(ctx context.Context, names int) {
	if m != nil {
		m.streamsActive.Add(ctx, -1)
		m.namesActive.Add(ctx, int64(-names))
	}
}

func (m *metrics) recordSubscribe(ctx context.Context, n int) {
	if m != nil {
		m.subscriptions.Add(ctx, int64(n), actionSubscribe)
	}
}

func (m *metrics) recordUnsubscribe(ctx context.Context, n int) {
	if m != nil {
		m.subscriptions.Add(ctx, int64(n), actionUnsubscribe)
	}
}

func (m *metrics) addLiveNames(ctx context.Context, delta int) {
	if m != nil {
		m.namesActive.Add(ctx, int64(delta))
	}
}

func (m *metrics) recordNACK(ctx context.Context) {
	if m != nil {
		m.nacks.Add(ctx, 1)
	}
}

func (m *metrics) recordResync(ctx context.Context, names int) {
	if m != nil {
		m.resyncNames.Record(ctx, int64(names))
	}
}

func (m *metrics) recordResponse(ctx context.Context, bytes, resources, removals int) {
	if m == nil {
		return
	}
	m.responseSize.Record(ctx, int64(bytes))
	m.responseEntries.Add(ctx, int64(resources), entryResource)
	m.responseEntries.Add(ctx, int64(removals), entryRemoval)
}

func (m *metrics) recordIdleWithdrawal(ctx context.Context, names int) {
	if m != nil {
		m.idleWithdrawn.Record(ctx, int64(names))
	}
}

func (m *metrics) recordRotation(ctx context.Context, d time.Duration, resources int) {
	if m == nil {
		return
	}
	m.rotationDuration.Record(ctx, d.Seconds())
	m.rotationResources.Record(ctx, int64(resources))
}
