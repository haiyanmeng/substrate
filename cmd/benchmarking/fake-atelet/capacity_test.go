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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeWorkerService answers SetWorkerCapacity from a per-Worker queue of
// errors, then success.
type fakeWorkerService struct {
	mu       sync.Mutex
	failures map[string][]error
	reported map[string]*ateapipb.WorkerResources
}

func (f *fakeWorkerService) SetWorkerCapacity(_ context.Context, in *ateapipb.SetWorkerCapacityRequest, _ ...grpc.CallOption) (*ateapipb.SetWorkerCapacityResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetWorker().GetName()
	if q := f.failures[name]; len(q) > 0 {
		f.failures[name] = q[1:]
		return nil, q[0]
	}
	if f.reported == nil {
		f.reported = map[string]*ateapipb.WorkerResources{}
	}
	f.reported[name] = in.GetCapacity()
	return &ateapipb.SetWorkerCapacityResponse{}, nil
}

func newReporter(client capacityClient, workers []string, timeout time.Duration) *capacityReporter {
	return &capacityReporter{
		client:         client,
		workers:        workers,
		capacity:       &ateapipb.WorkerResources{Actors: 5},
		timeout:        timeout,
		initialBackoff: time.Millisecond,
		maxBackoff:     5 * time.Millisecond,
	}
}

func TestCapacityReporterReportsEveryWorkerThenIsReady(t *testing.T) {
	svc := &fakeWorkerService{}
	workers := []string{"fake-r1-aaaaaaaa-0", "fake-r1-aaaaaaaa-1", "fake-r1-aaaaaaaa-2"}
	r := newReporter(svc, workers, time.Second)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !r.ready.Load() {
		t.Error("not ready after every Worker was reported")
	}
	for _, w := range workers {
		if !proto.Equal(svc.reported[w], r.capacity) {
			t.Errorf("Worker %s reported %v, want %v", w, svc.reported[w], r.capacity)
		}
	}
}

// fake-atelet usually starts before fake-workersync has created the Workers.
func TestCapacityReporterRetriesUntilTheWorkerExists(t *testing.T) {
	notYet := status.Error(codes.NotFound, "Worker not found")
	svc := &fakeWorkerService{failures: map[string][]error{
		"w0": {notYet, notYet, status.Error(codes.Unavailable, "ate-api-server restarting")},
	}}
	r := newReporter(svc, []string{"w0"}, time.Second)
	if err := r.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !r.ready.Load() || svc.reported["w0"] == nil {
		t.Error("Worker not reported after its transient failures cleared")
	}
}

// NotFound also means the two fakes disagree about this node, so the retry
// must give up rather than leave the pod retrying forever.
func TestCapacityReporterGivesUpAtTimeoutAndStaysUnready(t *testing.T) {
	forever := make([]error, 10000)
	for i := range forever {
		forever[i] = status.Error(codes.NotFound, "Worker not found")
	}
	svc := &fakeWorkerService{failures: map[string][]error{"w0": forever}}
	r := newReporter(svc, []string{"w0", "w1"}, 50*time.Millisecond)
	start := time.Now()
	if err := r.run(context.Background()); err == nil {
		t.Fatal("run succeeded with a Worker that never exists")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %v, want it bounded by the 50ms timeout", elapsed)
	}
	if r.ready.Load() {
		t.Error("ready with a Worker that has no capacity")
	}
}

func TestCapacityReporterDoesNotRetryPermanentErrors(t *testing.T) {
	var calls atomic.Int32
	client := capacityClientFunc(func() error {
		calls.Add(1)
		return status.Error(codes.PermissionDenied, "caller is not atelet")
	})
	r := newReporter(client, []string{"w0"}, time.Second)
	if err := r.run(context.Background()); err == nil {
		t.Fatal("run succeeded, want PermissionDenied surfaced")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("SetWorkerCapacity called %d times, want 1: PermissionDenied cannot clear by retrying", got)
	}
}

type capacityClientFunc func() error

func (f capacityClientFunc) SetWorkerCapacity(context.Context, *ateapipb.SetWorkerCapacityRequest, ...grpc.CallOption) (*ateapipb.SetWorkerCapacityResponse, error) {
	if err := f(); err != nil {
		return nil, err
	}
	return &ateapipb.SetWorkerCapacityResponse{}, nil
}

func TestRetryable(t *testing.T) {
	for _, c := range []codes.Code{codes.NotFound, codes.Unavailable, codes.DeadlineExceeded, codes.Aborted, codes.ResourceExhausted} {
		if !retryable(status.Error(c, "")) {
			t.Errorf("%v not retryable, want retryable", c)
		}
	}
	for _, c := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument, codes.Internal} {
		if retryable(status.Error(c, "")) {
			t.Errorf("%v retryable, want not", c)
		}
	}
}

func TestHealthHandler(t *testing.T) {
	var ready atomic.Bool
	h := healthHandler(&ready)
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", got)
	}
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz before capacity = %d, want 503", got)
	}
	ready.Store(true)
	if got := get("/readyz"); got != http.StatusOK {
		t.Errorf("/readyz after capacity = %d, want 200", got)
	}
}
