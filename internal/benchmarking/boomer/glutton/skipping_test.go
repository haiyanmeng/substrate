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

package glutton

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/dynconfig"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/userclass"
	"go.opentelemetry.io/otel"
)

// countingRouter stands in for the atenet router on the fake data plane,
// where no actor answers: it fails every request and counts them.
func countingRouter(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		http.Error(w, "no actor behind the fake data plane", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestSpawnRunBatch_SkipPingCountsResumedActorsReady(t *testing.T) {
	router, requests := countingRouter(t)
	cfg := &userclass.Config{
		APIStub:          &fakeControlClient{},
		HTTPClient:       router.Client(),
		RouterURL:        router.URL,
		Atespace:         "test-space",
		Tracer:           otel.Tracer("test"),
		TotalActors:      5,
		SpawnConcurrency: 2,
		ActorDeadline:    5 * time.Second,
		SkipPing:         true,
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &spawnRuntime{cfg: cfg, runCtx: runCtx, cancelRun: cancel}

	rt.runBatch(runCtx)

	if got := rt.readyCount.Load(); got != 5 {
		t.Errorf("readyCount = %d, want 5: with SkipPing an actor is ready once resumed", got)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d requests to the router, want none with SkipPing", got)
	}
}

func TestGluttonIterate_SkipPingSendsTheActorNothing(t *testing.T) {
	router, requests := countingRouter(t)
	fakeCtrl := &fakeControlClient{}
	cfg := &userclass.Config{
		APIStub:    fakeCtrl,
		HTTPClient: router.Client(),
		RouterURL:  router.URL,
		Atespace:   "bench-test",
		Tracer:     otel.Tracer("test"),
		// Memory and CPU load are configured, and still skipped: both are
		// requests to the actor.
		Dyn: dynconfig.NewHolder(dynconfig.Config{
			LifecycleMode:   dynconfig.LifecycleModeSuspend,
			MemTarget:       "1Mi",
			CPUCores:        1,
			MaxPingsPerWake: 3,
		}),
		SkipPing: true,
	}
	rt := &taskRuntime{cfg: cfg}

	rt.iterate()

	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d requests to the actor, want none with SkipPing", got)
	}
	calls := fakeCtrl.recordedCalls()
	if !slices.Contains(calls, "ResumeActor") || !slices.Contains(calls, "SuspendActor") {
		t.Errorf("calls = %v, want a resume then a suspend", calls)
	}
}

// Without SkipPing the cycle still pings, so the flag is what turns it off.
func TestGluttonIterate_PingsWithoutSkipPing(t *testing.T) {
	router, requests := countingRouter(t)
	cfg := &userclass.Config{
		APIStub:    &fakeControlClient{},
		HTTPClient: router.Client(),
		RouterURL:  router.URL,
		Atespace:   "bench-test",
		Tracer:     otel.Tracer("test"),
		Dyn:        dynconfig.NewHolder(dynconfig.Config{LifecycleMode: dynconfig.LifecycleModeSuspend}),
	}
	rt := &taskRuntime{cfg: cfg}

	rt.iterate()

	if got := requests.Load(); got == 0 {
		t.Error("sent no request to the actor without SkipPing; the test would not catch a cycle that never pings")
	}
}
