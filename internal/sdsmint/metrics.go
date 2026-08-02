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
	"sync/atomic"
	"time"
)

// Metrics counts what the scalability harness needs to attribute a bottleneck.
//
// Without it the only server-side signal is the issuance log, and parsing logs
// at rate is both slow and a perturbation of the thing being measured. Every
// method is safe on a nil *Metrics, so the counters stay optional: passing one
// in is what turns them on.
//
// Counters are monotonic unless the name says otherwise; the two gauges are
// StreamsLive and NamesLive.
type Metrics struct {
	mintsIssued  atomic.Int64
	mintsDenied  atomic.Int64
	cacheHits    atomic.Int64
	signNanos    atomic.Int64
	signMaxNanos atomic.Int64

	streamsOpened atomic.Int64
	streamsLive   atomic.Int64
	subscribes    atomic.Int64
	unsubscribes  atomic.Int64
	namesLive     atomic.Int64
	nacks         atomic.Int64

	// resyncRequests counts requests carrying initial_resource_versions, which
	// is how Envoy replays its live set after the stream drops. resyncNames is
	// how many names those replays carried -- the cost of a reconnect.
	resyncRequests atomic.Int64
	resyncNames    atomic.Int64

	responses        atomic.Int64
	responseBytes    atomic.Int64
	maxResponseBytes atomic.Int64
	resourcesSent    atomic.Int64
	removalsSent     atomic.Int64

	// idleSweeps counts sweeps that withdrew something, not sweeps that ran --
	// a tick that found nothing idle is not an event worth a counter.
	idleSweeps      atomic.Int64
	idleWithdrawals atomic.Int64

	rotations         atomic.Int64
	rotationResources atomic.Int64
	rotationNanos     atomic.Int64
	rotationMaxNanos  atomic.Int64
}

// enabled reports whether counters are on. Callers use it to skip work that
// only exists to feed a counter, such as sizing a response.
func (m *Metrics) enabled() bool { return m != nil }

// storeMax raises dst to v if v is larger. Used for the max-latency and
// max-size watermarks, which is where the interesting outliers live -- a mean
// hides a single 4MB rotation response completely.
func storeMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

func (m *Metrics) recordMint(d time.Duration) {
	if m == nil {
		return
	}
	m.mintsIssued.Add(1)
	m.signNanos.Add(int64(d))
	storeMax(&m.signMaxNanos, int64(d))
}

func (m *Metrics) recordCacheHit() {
	if m != nil {
		m.cacheHits.Add(1)
	}
}

func (m *Metrics) recordDenial() {
	if m != nil {
		m.mintsDenied.Add(1)
	}
}

func (m *Metrics) recordStreamOpen() {
	if m != nil {
		m.streamsOpened.Add(1)
		m.streamsLive.Add(1)
	}
}

// recordStreamClose also drops the names the stream was holding, since they
// die with it. NamesLive is a gauge across all streams, so a stream that goes
// away without this would leak its whole subscription set into the total.
func (m *Metrics) recordStreamClose(names int) {
	if m != nil {
		m.streamsLive.Add(-1)
		m.namesLive.Add(int64(-names))
	}
}

func (m *Metrics) recordSubscribe(n int) {
	if m != nil {
		m.subscribes.Add(int64(n))
	}
}

func (m *Metrics) recordUnsubscribe(n int) {
	if m != nil {
		m.unsubscribes.Add(int64(n))
	}
}

func (m *Metrics) addLiveNames(delta int) {
	if m != nil {
		m.namesLive.Add(int64(delta))
	}
}

func (m *Metrics) recordNACK() {
	if m != nil {
		m.nacks.Add(1)
	}
}

func (m *Metrics) recordResync(names int) {
	if m != nil {
		m.resyncRequests.Add(1)
		m.resyncNames.Add(int64(names))
	}
}

func (m *Metrics) recordResponse(bytes, resources, removals int) {
	if m == nil {
		return
	}
	m.responses.Add(1)
	m.responseBytes.Add(int64(bytes))
	storeMax(&m.maxResponseBytes, int64(bytes))
	m.resourcesSent.Add(int64(resources))
	m.removalsSent.Add(int64(removals))
}

func (m *Metrics) recordIdleWithdrawal(names int) {
	if m != nil {
		m.idleSweeps.Add(1)
		m.idleWithdrawals.Add(int64(names))
	}
}

func (m *Metrics) recordRotation(d time.Duration, resources int) {
	if m == nil {
		return
	}
	m.rotations.Add(1)
	m.rotationResources.Add(int64(resources))
	m.rotationNanos.Add(int64(d))
	storeMax(&m.rotationMaxNanos, int64(d))
}

// Snapshot returns the counters as a flat map, ready to be serialised as JSON.
// Flat rather than nested so a harness can diff two snapshots key by key.
func (m *Metrics) Snapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	s := map[string]int64{
		"mints_issued":         m.mintsIssued.Load(),
		"mints_denied":         m.mintsDenied.Load(),
		"cache_hits":           m.cacheHits.Load(),
		"sign_nanos_total":     m.signNanos.Load(),
		"sign_nanos_max":       m.signMaxNanos.Load(),
		"streams_opened":       m.streamsOpened.Load(),
		"streams_live":         m.streamsLive.Load(),
		"subscribes":           m.subscribes.Load(),
		"unsubscribes":         m.unsubscribes.Load(),
		"names_live":           m.namesLive.Load(),
		"nacks":                m.nacks.Load(),
		"resync_requests":      m.resyncRequests.Load(),
		"resync_names":         m.resyncNames.Load(),
		"responses":            m.responses.Load(),
		"response_bytes":       m.responseBytes.Load(),
		"response_bytes_max":   m.maxResponseBytes.Load(),
		"resources_sent":       m.resourcesSent.Load(),
		"removals_sent":        m.removalsSent.Load(),
		"idle_sweeps":          m.idleSweeps.Load(),
		"idle_withdrawals":     m.idleWithdrawals.Load(),
		"rotations":            m.rotations.Load(),
		"rotation_resources":   m.rotationResources.Load(),
		"rotation_nanos_total": m.rotationNanos.Load(),
		"rotation_nanos_max":   m.rotationMaxNanos.Load(),
	}
	// Derived averages, so a harness reading one sample does not have to keep
	// the previous one around to divide.
	if n := s["mints_issued"]; n > 0 {
		s["sign_nanos_avg"] = s["sign_nanos_total"] / n
	}
	if n := s["rotations"]; n > 0 {
		s["rotation_nanos_avg"] = s["rotation_nanos_total"] / n
		s["rotation_resources_avg"] = s["rotation_resources"] / n
	}
	if n := s["responses"]; n > 0 {
		s["response_bytes_avg"] = s["response_bytes"] / n
	}
	return s
}
