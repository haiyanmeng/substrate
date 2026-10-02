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

package resumecold

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/ateinterceptors"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/dynconfig"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/userclass"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeControlClient keeps each actor's state, so a resume of a running actor
// reports Resumed false the way ateapi does.
type fakeControlClient struct {
	ateapipb.ControlClient
	mu     sync.Mutex
	calls  []string // "<RPC> <actor>"
	states map[string]ateapipb.ActorState
	// resumeErr, when set, fails every ResumeActor without changing state.
	resumeErr error
	// serverElapsedUS, when set, is returned in the ServerElapsedTrailer.
	serverElapsedUS string
	// deleteAborts is how many DeleteActor calls fail with Aborted, as when
	// another operation holds the actor lease, before deletes succeed.
	deleteAborts int
	// resumeEntered and resumeGate, when set, hold every ResumeActor: it
	// signals resumeEntered and then waits for resumeGate to close.
	resumeEntered chan struct{}
	resumeGate    chan struct{}
}

func newFake() *fakeControlClient {
	return &fakeControlClient{states: map[string]ateapipb.ActorState{}}
}

func (f *fakeControlClient) record(rpc, actor string) {
	f.calls = append(f.calls, rpc+" "+actor)
}

func (f *fakeControlClient) setTrailer(opts []grpc.CallOption) {
	if f.serverElapsedUS == "" {
		return
	}
	for _, o := range opts {
		if t, ok := o.(grpc.TrailerCallOption); ok {
			*t.TrailerAddr = metadata.Pairs(ateinterceptors.ServerElapsedTrailer, f.serverElapsedUS)
		}
	}
}

func (f *fakeControlClient) setState(name string, s ateapipb.ActorState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[name] = s
}

func (f *fakeControlClient) state(name string) ateapipb.ActorState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[name]
}

func (f *fakeControlClient) CreateAtespace(ctx context.Context, in *ateapipb.CreateAtespaceRequest, opts ...grpc.CallOption) (*ateapipb.Atespace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("CreateAtespace", in.GetAtespace().GetMetadata().GetName())
	return &ateapipb.Atespace{}, nil
}

func (f *fakeControlClient) CreateActor(ctx context.Context, in *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetActor().GetMetadata().GetName()
	f.record("CreateActor", name)
	f.states[name] = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
	return &ateapipb.Actor{}, nil
}

func (f *fakeControlClient) GetActor(ctx context.Context, in *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetActor().GetName()
	f.record("GetActor", name)
	s, ok := f.states[name]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such actor")
	}
	return &ateapipb.Actor{Status: &ateapipb.ActorStatus{State: s}}, nil
}

func (f *fakeControlClient) ResumeActor(ctx context.Context, in *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	if f.resumeGate != nil {
		f.resumeEntered <- struct{}{}
		<-f.resumeGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetActor().GetName()
	f.record("ResumeActor", name)
	f.setTrailer(opts)
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	switch f.states[name] {
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return &ateapipb.ResumeActorResponse{Resumed: false}, nil
	case ateapipb.ActorState_ACTOR_STATE_PAUSED, ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		f.states[name] = ateapipb.ActorState_ACTOR_STATE_RUNNING
		return &ateapipb.ResumeActorResponse{Resumed: true}, nil
	}
	return nil, status.Error(codes.Aborted, "actor crashed")
}

func (f *fakeControlClient) PauseActor(ctx context.Context, in *ateapipb.PauseActorRequest, opts ...grpc.CallOption) (*ateapipb.PauseActorResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetActor().GetName()
	f.record("PauseActor", name)
	if f.states[name] != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return nil, status.Error(codes.FailedPrecondition, "not running")
	}
	f.states[name] = ateapipb.ActorState_ACTOR_STATE_PAUSED
	return &ateapipb.PauseActorResponse{}, nil
}

func (f *fakeControlClient) DeleteActor(ctx context.Context, in *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetActor().GetName()
	f.record("DeleteActor", name)
	if f.deleteAborts > 0 {
		f.deleteAborts--
		return nil, status.Error(codes.Aborted, "lease held")
	}
	delete(f.states, name)
	return &ateapipb.Actor{}, nil
}

// callsTo returns the actors of every recorded call to rpc, in order.
func (f *fakeControlClient) callsTo(rpc string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if r, actor, _ := strings.Cut(c, " "); r == rpc {
			out = append(out, actor)
		}
	}
	return out
}

func (f *fakeControlClient) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func newTestRuntime(fake *fakeControlClient, actors int) *runtime {
	r := newRuntime(&userclass.Config{
		APIStub:  fake,
		Atespace: "bench",
		Dyn:      dynconfig.NewHolder(dynconfig.Config{}),
		Tracer:   otel.Tracer("test"),
		Actors:   actors,
	})
	r.retryBackoff = time.Millisecond
	r.exhaustedWait = 0
	return r
}

// readyActors drains the ready queue and puts the actors back, returning
// them sorted.
func readyActors(r *runtime) []string {
	var out []string
	for len(r.ready) > 0 {
		out = append(out, <-r.ready)
	}
	for _, name := range out {
		r.ready <- name
	}
	slices.Sort(out)
	return out
}

func fleetActors(r *runtime) []string {
	r.fleetMu.Lock()
	defer r.fleetMu.Unlock()
	out := slices.Clone(r.fleet)
	slices.Sort(out)
	return out
}

func TestRegistered(t *testing.T) {
	e, ok := userclass.Lookup("resumecold")
	if !ok {
		t.Fatal("resumecold is not registered")
	}
	if e.LocustFile != "resumecold.py" || e.UserClass != userClass {
		t.Errorf("got entry %+v", e)
	}
}

func TestStartFleetMakesEveryActorCold(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 5)
	r.startFleet(context.Background())

	ready := readyActors(r)
	if len(ready) != 5 || !slices.Equal(ready, fleetActors(r)) {
		t.Fatalf("ready %v, fleet %v, want the same 5 actors", ready, fleetActors(r))
	}
	if got := fake.callsTo("CreateAtespace"); !slices.Equal(got, []string{"bench"}) {
		t.Errorf("CreateAtespace calls: got %v", got)
	}
	for _, rpc := range []string{"CreateActor", "ResumeActor", "PauseActor"} {
		got := fake.callsTo(rpc)
		slices.Sort(got)
		if !slices.Equal(got, ready) {
			t.Errorf("%s: got %v, want each of %v once", rpc, got, ready)
		}
	}
	for _, name := range ready {
		if got := fake.state(name); got != ateapipb.ActorState_ACTOR_STATE_PAUSED {
			t.Errorf("actor %s is %v, want paused", name, got)
		}
	}
}

func TestStartFleetLeavesOutActorsThatDoNotWarm(t *testing.T) {
	fake := newFake()
	fake.resumeErr = status.Error(codes.Unavailable, "down")
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())

	if got := readyActors(r); len(got) != 0 {
		t.Fatalf("got ready actors %v, want none", got)
	}
	if got := fake.callsTo("DeleteActor"); !slices.Equal(got, fake.callsTo("CreateActor")) {
		t.Errorf("deleted %v, want the created actor", got)
	}
}

func TestIterateResumesThenHibernates(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())
	name := readyActors(r)[0]
	fake.resetCalls()
	before := requestCount(t, "ResumeActorCold", "success")

	for range 3 {
		r.iterate()
	}
	want := []string{"ResumeActor " + name, "PauseActor " + name}
	want = slices.Concat(want, want, want)
	if !slices.Equal(fake.calls, want) {
		t.Errorf("calls: got %v, want %v", fake.calls, want)
	}
	if got := readyActors(r); !slices.Equal(got, []string{name}) {
		t.Errorf("ready: got %v, want [%s]", got, name)
	}
	if got := requestCount(t, "ResumeActorCold", "success") - before; got != 3 {
		t.Errorf("ResumeActorCold successes grew by %v, want 3", got)
	}
}

func TestIterateReportsAnEmptyQueue(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	before := requestCount(t, "FleetExhausted", "failure")
	r.iterate()
	if len(fake.calls) != 0 {
		t.Errorf("calls with no ready actor: %v", fake.calls)
	}
	if got := requestCount(t, "FleetExhausted", "failure") - before; got != 1 {
		t.Errorf("FleetExhausted failures grew by %v, want 1", got)
	}
}

func TestIterateFailsAResumeOfARunningActor(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())
	name := readyActors(r)[0]
	fake.setState(name, ateapipb.ActorState_ACTOR_STATE_RUNNING)
	fake.resetCalls()
	before := requestCount(t, "ResumeActorCold", "failure")

	r.iterate()
	if got := requestCount(t, "ResumeActorCold", "failure") - before; got != 1 {
		t.Errorf("ResumeActorCold failures grew by %v, want 1", got)
	}
	// Recovery pauses the running actor.
	want := []string{"ResumeActor " + name, "PauseActor " + name}
	if !slices.Equal(fake.calls, want) {
		t.Errorf("calls: got %v, want %v", fake.calls, want)
	}
	if got := fake.state(name); got != ateapipb.ActorState_ACTOR_STATE_PAUSED {
		t.Errorf("actor is %v, want paused", got)
	}
	if got := readyActors(r); !slices.Equal(got, []string{name}) {
		t.Errorf("ready: got %v, want [%s]", got, name)
	}
}

func TestIterateReturnsAColdActorAfterAFailedResume(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())
	name := readyActors(r)[0]
	fake.resumeErr = status.Error(codes.ResourceExhausted, "no free workers available")

	r.iterate()
	if got := readyActors(r); !slices.Equal(got, []string{name}) {
		t.Errorf("ready: got %v, want [%s]", got, name)
	}
	if got := fake.state(name); got != ateapipb.ActorState_ACTOR_STATE_PAUSED {
		t.Errorf("actor is %v, want paused", got)
	}
}

func TestIteratePacesResumes(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())

	r.cfg.Dyn.Store(dynconfig.Config{TargetRPS: 200})
	start := time.Now()
	for range 21 {
		r.iterate()
	}
	// The first resume goes at once; the other 20 take 5 ms each.
	if elapsed := time.Since(start); elapsed < 95*time.Millisecond {
		t.Errorf("21 resumes at 200 per second took %v, want >= 100ms", elapsed)
	}
}

func TestIterateRecordsServerLatency(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())
	fake.serverElapsedUS = "1500"
	before := serverLatencySum(t)
	r.iterate()
	if got := serverLatencySum(t) - before; got != 1.5 {
		t.Errorf("server latency sum grew by %v ms, want 1.5", got)
	}
}

func TestShutdownDeletesTheFleet(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 3)
	r.startFleet(context.Background())
	want := fleetActors(r)
	r.shutdown(context.Background())

	got := fake.callsTo("DeleteActor")
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

func TestShutdownRetriesAnAbortedDelete(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())
	name := fleetActors(r)[0]
	fake.deleteAborts = 2
	r.shutdown(context.Background())

	if got := fake.callsTo("DeleteActor"); len(got) != 3 {
		t.Errorf("DeleteActor calls = %v, want 3 calls", got)
	}
	if _, err := fake.GetActor(context.Background(), &ateapipb.GetActorRequest{Actor: r.ref(name)}); status.Code(err) != codes.NotFound {
		t.Errorf("actor still exists after shutdown: err = %v", err)
	}
}

func TestShutdownWaitsForARunningCycle(t *testing.T) {
	fake := newFake()
	r := newTestRuntime(fake, 1)
	r.startFleet(context.Background())
	name := fleetActors(r)[0]
	fake.resumeEntered = make(chan struct{})
	fake.resumeGate = make(chan struct{})
	cycleDone := make(chan struct{})
	go func() {
		r.iterate()
		close(cycleDone)
	}()
	<-fake.resumeEntered

	shutdownDone := make(chan struct{})
	go func() {
		r.shutdown(context.Background())
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned while a cycle was running")
	case <-time.After(20 * time.Millisecond):
	}
	if got := fake.callsTo("DeleteActor"); len(got) != 0 {
		t.Errorf("deleted %v while a cycle was running", got)
	}

	close(fake.resumeGate)
	<-cycleDone
	<-shutdownDone
	if got := fake.callsTo("DeleteActor"); !slices.Equal(got, []string{name}) {
		t.Errorf("deleted %v, want [%s]", got, name)
	}
	r.iterate()
	if got := fake.callsTo("ResumeActor"); len(got) != 2 {
		t.Errorf("ResumeActor calls = %v, want the warm-up and the one cycle only", got)
	}
}

// requestCount scrapes the default registry for the locust_requests_total
// of one request name and status, which is global to the test binary.
func requestCount(t *testing.T, name, result string) float64 {
	t.Helper()
	return scrape(t, `locust_requests_total{method="grpc",name="`+name+`",status="`+result+`",user_class="ResumeColdUser"} `)
}

// serverLatencySum scrapes the default registry for the ResumeActorCold
// server latency sum.
func serverLatencySum(t *testing.T) float64 {
	t.Helper()
	return scrape(t, `locust_server_duration_milliseconds_sum{method="grpc",name="ResumeActorCold",user_class="ResumeColdUser"} `)
}

func scrape(t *testing.T, prefix string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return f
		}
	}
	return 0
}
