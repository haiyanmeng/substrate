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
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/agent-substrate/substrate/internal/benchmarking/fakeworker"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// workerClient is the part of ateapipb.ControlClient the registrar uses.
type workerClient interface {
	CreateWorker(ctx context.Context, in *ateapipb.CreateWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
	DeleteWorker(ctx context.Context, in *ateapipb.DeleteWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
}

// registrar creates and deletes the fake Workers for a fixed set of nodes.
// Every name is derived (see fakeworker), so deleting needs only the run
// prefix and the node list, not a record of what was created.
type registrar struct {
	client         workerClient
	run            string
	workersPerNode int
	namespace      string
	pool           string
	sandboxClass   string
	labels         map[string]string
	concurrency    int
}

// workers returns the Worker records for nodes. Epoch stays 0: a Worker whose
// epoch rises past its observed epoch has every Actor on it crashed, since in
// production that means its ateom restarted.
func (r *registrar) workers(nodes []string) []*ateapipb.Worker {
	var out []*ateapipb.Worker
	for _, node := range nodes {
		for i, name := range fakeworker.Names(r.run, node, r.workersPerNode) {
			out = append(out, &ateapipb.Worker{
				Metadata:        &ateapipb.ResourceMetadata{Name: name},
				WorkerNamespace: r.namespace,
				WorkerPool:      r.pool,
				WorkerPod:       name,
				WorkerPodUid:    fakeworker.PodUID(name),
				NodeName:        node,
				Ips:             []string{fakeworker.IP(i)},
				SandboxClass:    r.sandboxClass,
				Labels:          maps.Clone(r.labels),
			})
		}
	}
	return out
}

// register creates every fake Worker on nodes. An existing Worker counts as
// created, so a restart is idempotent.
func (r *registrar) register(ctx context.Context, nodes []string) error {
	if len(nodes) == 0 {
		return errors.New("no nodes to register fake Workers on")
	}
	workers := r.workers(nodes)
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(r.concurrency)
	for _, w := range workers {
		g.Go(func() error {
			_, err := r.client.CreateWorker(ctx, &ateapipb.CreateWorkerRequest{Worker: w})
			if err != nil && status.Code(err) != codes.AlreadyExists {
				return fmt.Errorf("while creating Worker %s: %w", w.GetMetadata().GetName(), err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	slog.InfoContext(ctx, "Registered fake Workers", slog.Int("nodes", len(nodes)), slog.Int("workers", len(workers)))
	return nil
}

// unregister deletes every fake Worker on nodes. A Worker already gone counts
// as deleted. It keeps going past a failed delete, so one error does not
// leave the rest behind, and reports every failure.
func (r *registrar) unregister(ctx context.Context, nodes []string) error {
	workers := r.workers(nodes)
	var g errgroup.Group
	g.SetLimit(r.concurrency)
	errs := make([]error, len(workers))
	for i, w := range workers {
		g.Go(func() error {
			name := w.GetMetadata().GetName()
			_, err := r.client.DeleteWorker(ctx, &ateapipb.DeleteWorkerRequest{Worker: &ateapipb.ObjectRef{Name: name}})
			if err != nil && status.Code(err) != codes.NotFound {
				errs[i] = fmt.Errorf("while deleting Worker %s: %w", name, err)
			}
			return nil
		})
	}
	_ = g.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	slog.InfoContext(ctx, "Deleted fake Workers", slog.Int("nodes", len(nodes)), slog.Int("workers", len(workers)))
	return nil
}
