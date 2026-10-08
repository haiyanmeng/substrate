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

// Package resumecold drives ate-api-server's cold resume path: every
// ResumeActor targets a paused actor, so ateapi takes the actor lease, binds
// a worker, and waits on an atelet restore. Each resume is followed by a
// pause that makes the actor cold again. Run on the fake data plane
// (benchmarking/workloads/deploy.sh --fake-data-plane), it measures what a
// cold resume costs ateapi and Postgres without the router or the restore in
// the way. It sends the actors themselves no requests, since none exist
// there.
package resumecold

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/internal/ateinterceptors"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/boomerutil"
	bmetrics "github.com/agent-substrate/substrate/internal/benchmarking/boomer/metrics"
	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/userclass"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	userClass        = "ResumeColdUser"
	templateName     = "glutton"
	templateAtespace = "benchmark-workloads"
)

func init() {
	userclass.Add(userclass.Entry{
		Name:       "resumecold",
		LocustFile: "resumecold.py",
		UserClass:  userClass,
		Init:       initResumeCold,
	})
}

// errAlreadyRunning marks a ResumeActor that found its actor running, so no
// cold resume happened and the sample must not count as one.
var errAlreadyRunning = errors.New("actor was already running: not a cold resume")

// initResumeCold creates the fleet and makes every actor cold before
// returning, so the worker connects to the locust master only once the
// warm-up is over and it stays out of the measured steps.
func initResumeCold(cfg *userclass.Config) (taskFn func(), shutdown func(context.Context)) {
	if cfg.Tracer == nil {
		cfg.Tracer = otel.Tracer("substrate-boomer/resumecold")
	}
	rt := newRuntime(cfg)
	rt.startFleet(context.Background())
	return rt.iterate, rt.shutdown
}

type runtime struct {
	cfg *userclass.Config
	// ready holds the cold actors no user is cycling. Its capacity is the
	// fleet size, so returning an actor never blocks.
	ready chan string
	// fleetMu guards fleet, every actor that joined, for shutdown.
	fleetMu sync.Mutex
	fleet   []string
	limiter *rate.Limiter
	// limitMu guards limit, the TargetRPS the limiter was last set to.
	limitMu sync.Mutex
	limit   float64
	// retryBackoff spaces the retries of a delete.
	retryBackoff time.Duration
	// exhaustedWait is how long a user waits after finding no cold actor, so
	// an unpaced run does not spin on the failure counter.
	exhaustedWait time.Duration
	// cycleMu guards closing and every cycles.Add, so no cycle starts once
	// shutdown has begun waiting for the running ones.
	cycleMu sync.Mutex
	closing bool
	cycles  sync.WaitGroup
}

func newRuntime(cfg *userclass.Config) *runtime {
	n := max(cfg.Actors, 1)
	// Burst 1 spaces resumes evenly: Wait hands each caller the next slot
	// rather than releasing a second's budget at once.
	return &runtime{
		cfg:           cfg,
		ready:         make(chan string, n),
		fleet:         make([]string, 0, n),
		limiter:       rate.NewLimiter(rate.Inf, 1),
		retryBackoff:  time.Second,
		exhaustedWait: 10 * time.Millisecond,
	}
}

// startFleet creates cfg.Actors actors and makes each cold: create, a first
// resume, then a pause. An actor that fails a step is deleted and left out;
// the run goes on with the rest, and the log says how many there are.
func (r *runtime) startFleet(ctx context.Context) {
	n := cap(r.ready)
	if err := r.ensureAtespace(ctx); err != nil {
		slog.Error("resumecold: CreateAtespace failed", slog.String("err", err.Error()))
		return
	}

	// Bounds the RPCs in flight while warming: each first resume is a cold
	// start on a worker, and a few hundred at once would queue in ateapi.
	const warmParallelism = 16
	sem := make(chan struct{}, warmParallelism)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.addActor(ctx)
		}()
	}
	wg.Wait()
	slog.Info("resumecold: fleet ready",
		slog.Int("wanted", n), slog.Int("cold", len(r.ready)))
}

// addActor creates one actor, makes it cold, and puts it in the ready queue.
func (r *runtime) addActor(ctx context.Context) {
	name := "rc-" + uuid.NewString()
	if err := r.warm(ctx, name); err != nil {
		slog.Warn("resumecold: actor did not warm; leaving it out",
			slog.String("actor", name), slog.String("err", err.Error()))
		_ = r.delete(ctx, name)
		return
	}
	r.fleetMu.Lock()
	r.fleet = append(r.fleet, name)
	r.fleetMu.Unlock()
	r.ready <- name
}

// warm creates one actor, resumes it, and pauses it.
func (r *runtime) warm(ctx context.Context, name string) error {
	if err := r.call(ctx, "CreateActor", func(ctx context.Context, tr *metadata.MD) error {
		_, err := r.cfg.APIStub.CreateActor(ctx, &ateapipb.CreateActorRequest{
			Actor: &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: r.cfg.Atespace, Name: name},
				ActorTemplate: &ateapipb.ObjectRef{Atespace: templateAtespace, Name: templateName},
			},
		}, grpc.Trailer(tr))
		return err
	}); err != nil {
		return err
	}
	if err := r.resume(ctx, "ResumeActorWarm", name, false); err != nil {
		return err
	}
	return r.pause(ctx, "Warm", name)
}

// iterate is the boomer task: one paced cold resume of a ready actor, then
// the pause that makes it cold again. Only the resume is paced, so the
// target rate is cold resumes per second. Finding the queue empty is
// reported as a FleetExhausted failure: the fleet, not the server, set the
// rate, and the step is not valid.
func (r *runtime) iterate() {
	ctx := context.Background()
	r.pace(ctx)
	if !r.startCycle() {
		return
	}
	defer r.cycles.Done()
	var name string
	select {
	case name = <-r.ready:
	default:
		bmetrics.RecordFailure("grpc", "FleetExhausted", userClass, 0, "no cold actor ready")
		time.Sleep(r.exhaustedWait)
		return
	}
	err := r.resume(ctx, "ResumeActorCold", name, true)
	if err == nil {
		err = r.pause(ctx, "", name)
	}
	if err != nil {
		// The actor may have been left running. Pausing it again makes it
		// cold for the next cycle; on an actor already paused, the error
		// is expected and ignored.
		_ = r.pause(ctx, "Recover", name)
	}
	r.ready <- name
}

// startCycle registers a cycle with shutdown, or reports false once shutdown
// has begun.
func (r *runtime) startCycle() bool {
	r.cycleMu.Lock()
	defer r.cycleMu.Unlock()
	if r.closing {
		return false
	}
	r.cycles.Add(1)
	return true
}

// pace blocks until the limiter grants the next resume slot, applying the
// current TargetRPS first. Zero means unpaced.
func (r *runtime) pace(ctx context.Context) {
	target := r.cfg.Dyn.Load().TargetRPS
	r.limitMu.Lock()
	if target != r.limit {
		r.limit = target
		if target > 0 {
			r.limiter.SetLimit(rate.Limit(target))
		} else {
			r.limiter.SetLimit(rate.Inf)
		}
	}
	r.limitMu.Unlock()
	_ = r.limiter.Wait(ctx)
}

// resume calls ResumeActor. With wantCold, a response that reports no resume
// happened fails the sample with errAlreadyRunning.
func (r *runtime) resume(ctx context.Context, metricName, name string, wantCold bool) error {
	return r.call(ctx, metricName, func(ctx context.Context, tr *metadata.MD) error {
		resp, err := r.cfg.APIStub.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
			Actor: r.ref(name),
		}, grpc.Trailer(tr))
		if err == nil && wantCold && !resp.GetResumed() {
			return errAlreadyRunning
		}
		return err
	})
}

// pause calls PauseActor. suffix keeps warm-up and recovery calls apart from
// the measured ones.
func (r *runtime) pause(ctx context.Context, suffix, name string) error {
	return r.call(ctx, "PauseActor"+suffix, func(ctx context.Context, tr *metadata.MD) error {
		_, err := r.cfg.APIStub.PauseActor(ctx, &ateapipb.PauseActorRequest{
			Actor: r.ref(name),
		}, grpc.Trailer(tr))
		return err
	})
}

// shutdown waits for the running cycles, then deletes the fleet. Boomer
// stops scheduling tasks before shutdown but does not wait for the ones in
// flight, and a delete that races one of them loses the actor lease and
// fails with Aborted. DeleteActor with AnyState removes an actor in any
// state, so a cycle cut short by ctx still leaves nothing behind.
func (r *runtime) shutdown(ctx context.Context) {
	r.cycleMu.Lock()
	r.closing = true
	r.cycleMu.Unlock()
	drained := make(chan struct{})
	go func() {
		r.cycles.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
	}

	r.fleetMu.Lock()
	names := r.fleet
	r.fleet = nil
	r.fleetMu.Unlock()

	const parallelism = 16
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	var leftMu sync.Mutex
	left := 0
	for _, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if r.delete(ctx, name) != nil {
				leftMu.Lock()
				left++
				leftMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if left > 0 {
		slog.Warn("resumecold: actors left behind after shutdown",
			slog.Int("count", left), slog.String("atespace", r.cfg.Atespace))
	}
}

// delete deletes an actor, retrying while another operation holds its lease.
func (r *runtime) delete(ctx context.Context, name string) error {
	const attempts = 5
	var err error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-time.After(r.retryBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err = r.call(ctx, "DeleteActor", func(ctx context.Context, tr *metadata.MD) error {
			_, err := r.cfg.APIStub.DeleteActor(ctx, &ateapipb.DeleteActorRequest{
				Actor:    r.ref(name),
				AnyState: true,
			}, grpc.Trailer(tr))
			return err
		})
		if c := status.Code(err); c != codes.Aborted && c != codes.Unavailable {
			return err
		}
	}
	return err
}

func (r *runtime) ref(name string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: r.cfg.Atespace, Name: name}
}

func (r *runtime) ensureAtespace(ctx context.Context) error {
	return r.call(ctx, "CreateAtespace", func(ctx context.Context, tr *metadata.MD) error {
		_, err := r.cfg.APIStub.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
			Atespace: &ateapipb.Atespace{
				Metadata: &ateapipb.ResourceMetadata{Name: r.cfg.Atespace},
			},
		}, grpc.Trailer(tr))
		if status.Code(err) == codes.AlreadyExists {
			return nil
		}
		return err
	})
}

// call times one unary RPC and reports it. The client latency goes to locust
// and Prometheus; the server's own elapsed time, from the trailer that
// ateinterceptors.ServerUnaryInterceptor sets, goes to Prometheus as a
// second histogram, so the gap between the two is network and client time.
func (r *runtime) call(ctx context.Context, name string, do func(context.Context, *metadata.MD) error) error {
	ctx, span := r.cfg.Tracer.Start(ctx, name)
	defer span.End()

	start := time.Now()
	var tr metadata.MD
	err := do(ctx, &tr)
	latency := time.Since(start)

	if serverLatency, source := boomerutil.ElapsedFromMD(tr, ateinterceptors.ServerElapsedTrailer, 0); source == boomerutil.SourceServer {
		bmetrics.RecordServerLatency("grpc", name, userClass, serverLatency)
	}
	boomerutil.LogSampledTrace(span, name, latency, boomerutil.SourceClient, err)
	if err != nil {
		bmetrics.RecordFailure("grpc", name, userClass, latency, err.Error())
		return err
	}
	bmetrics.RecordSuccess("grpc", name, userClass, latency, 0)
	return nil
}
