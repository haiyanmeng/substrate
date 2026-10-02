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
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// capacityReportConcurrency bounds the SetWorkerCapacity calls in flight.
const capacityReportConcurrency = 16

// capacityClient is the part of ateapipb.WorkerServiceClient the reporter
// uses.
type capacityClient interface {
	SetWorkerCapacity(ctx context.Context, in *ateapipb.SetWorkerCapacityRequest, opts ...grpc.CallOption) (*ateapipb.SetWorkerCapacityResponse, error)
}

// capacityReporter reports the configured capacity for every fake Worker on
// this node, once, and marks itself ready when all of them are reported.
//
// fake-atelet usually starts before fake-workersync has created the Workers,
// so NotFound is retried until timeout. The retry is bounded because NotFound
// also means the two fakes disagree about this node: SetWorkerCapacity reports
// a Worker on another node as NotFound, the same as a missing one.
type capacityReporter struct {
	client   capacityClient
	workers  []string
	capacity *ateapipb.WorkerResources
	timeout  time.Duration

	initialBackoff time.Duration
	maxBackoff     time.Duration

	ready atomic.Bool
}

// run reports every Worker's capacity. On success the reporter is ready; on
// failure it stays unready, which fails the deploy's rollout wait.
func (r *capacityReporter) run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(capacityReportConcurrency)
	for _, name := range r.workers {
		g.Go(func() error { return r.reportOne(ctx, name) })
	}
	if err := g.Wait(); err != nil {
		return err
	}
	r.ready.Store(true)
	slog.InfoContext(ctx, "Reported capacity for every fake Worker on this node",
		slog.Int("workers", len(r.workers)), slog.Any("capacity", r.capacity))
	return nil
}

func (r *capacityReporter) reportOne(ctx context.Context, name string) error {
	backoff := r.initialBackoff
	for {
		_, err := r.client.SetWorkerCapacity(ctx, &ateapipb.SetWorkerCapacityRequest{
			Worker:   &ateapipb.ObjectRef{Name: name},
			Capacity: r.capacity,
		})
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return fmt.Errorf("while reporting capacity for Worker %s: %w", name, err)
		}
		slog.WarnContext(ctx, "Capacity report not accepted yet, retrying",
			slog.String("worker", name), slog.Any("err", err))
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up reporting capacity for Worker %s: %w (last error: %v)", name, ctx.Err(), err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, r.maxBackoff)
	}
}

// retryable reports whether err may clear on its own: a Worker fake-workersync
// has not created yet, or ate-api-server being briefly unavailable.
func retryable(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.Unavailable, codes.DeadlineExceeded, codes.Aborted, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}
