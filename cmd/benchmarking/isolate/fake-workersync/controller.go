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
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/agent-substrate/substrate/cmd/benchmarking/isolate/internal/fakeworker"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
)

// workerClient is the part of ateapipb.ControlClient the controller uses.
type workerClient interface {
	CreateWorker(ctx context.Context, in *ateapipb.CreateWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
	DeleteWorker(ctx context.Context, in *ateapipb.DeleteWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
	DrainWorker(ctx context.Context, in *ateapipb.DrainWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
	GetWorker(ctx context.Context, in *ateapipb.GetWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
	UpdateWorker(ctx context.Context, in *ateapipb.UpdateWorkerRequest, opts ...grpc.CallOption) (*ateapipb.Worker, error)
	ListWorkers(ctx context.Context, in *ateapipb.ListWorkersRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkersResponse, error)
	ListWorkerActorAssignments(ctx context.Context, in *ateapipb.ListWorkerActorAssignmentsRequest, opts ...grpc.CallOption) (*ateapipb.ListWorkerActorAssignmentsResponse, error)
}

// capacityRelay sends a capacity report to the fake-atelet relay at addr.
type capacityRelay interface {
	Report(ctx context.Context, addr string, req *ateapipb.SetWorkerCapacityRequest) error
}

// node is a benchmark node and what it can allocate.
type node struct {
	name        string
	allocatable corev1.ResourceList
}

// cluster is what the controller reads from and writes to Kubernetes.
type cluster interface {
	// Pools returns every WorkerPool.
	Pools() ([]*atev1alpha1.WorkerPool, error)
	// Nodes returns the benchmark nodes, sorted by name.
	Nodes(ctx context.Context) ([]node, error)
	// Relays returns the fake-atelet relay address on each node that has one.
	Relays(ctx context.Context) (map[string]string, error)
	// UpdateStatus writes a WorkerPool's status.
	UpdateStatus(ctx context.Context, wp *atev1alpha1.WorkerPool) error
}

// fakeWorker is a fake Worker the controller has registered.
type fakeWorker struct {
	namespace, pool string
	index           int
	node            string
	// created is set once ate-api-server has the Worker. A Worker is tracked
	// from before its create, so a create the server commits but the caller
	// sees fail is still retired, or deleted at shutdown.
	created bool
	// labels and sandboxClass are what the Worker is registered with.
	labels       map[string]string
	sandboxClass string
	// reported is the capacity ate-api-server last accepted for it; nil
	// until one is.
	reported *ateapipb.WorkerResources
	draining bool
}

// controller stands in for atecontroller's WorkerPool controller and worker
// syncer together. For every WorkerPool it keeps spec.replicas fake Workers,
// none backed by a pod, has each one's capacity reported through the
// fake-atelet on its node, and writes the pool's status from them.
//
// It keeps the registered Workers in memory and lists them from ate-api-server
// only at startup (adopt). A pass with nothing changed and nothing draining
// makes no ate-api-server calls, so a steady fleet adds no load to the system
// under test.
type controller struct {
	client      workerClient
	relay       capacityRelay
	cluster     cluster
	run         string
	concurrency int

	mu      sync.Mutex
	workers map[string]*fakeWorker
}

func newController(client workerClient, relay capacityRelay, cl cluster, run string, concurrency int) *controller {
	return &controller{client: client, relay: relay, cluster: cl, run: run, concurrency: concurrency, workers: map[string]*fakeWorker{}}
}

// adopt loads the fake Workers of this run that are already registered, as
// after a restart, so they are reconciled rather than created again.
func (c *controller) adopt(ctx context.Context) error {
	prefix := fakeworker.Prefix(c.run)
	var token string
	for {
		page, err := c.client.ListWorkers(ctx, &ateapipb.ListWorkersRequest{PageSize: 1000, PageToken: token})
		if err != nil {
			return fmt.Errorf("while listing Workers: %w", err)
		}
		for _, w := range page.GetWorkers() {
			name := w.GetMetadata().GetName()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			index, ok := fakeworker.Index(c.run, w.GetWorkerNamespace(), w.GetWorkerPool(), name)
			if !ok {
				continue
			}
			c.workers[name] = &fakeWorker{
				namespace:    w.GetWorkerNamespace(),
				pool:         w.GetWorkerPool(),
				index:        index,
				node:         w.GetNodeName(),
				labels:       w.GetLabels(),
				sandboxClass: w.GetSandboxClass(),
				created:      true,
				draining:     w.GetStatus().GetState() == ateapipb.WorkerState_WORKER_STATE_DRAINING,
			}
		}
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	slog.InfoContext(ctx, "Adopted registered fake Workers", slog.Int("workers", len(c.workers)))
	return nil
}

// desiredWorker is a fake Worker some WorkerPool wants.
type desiredWorker struct {
	pool  *atev1alpha1.WorkerPool
	index int
}

// reconcile brings the fake Workers and the pools' status in line with the
// WorkerPools. Every step is idempotent, so a failed pass is retried by the
// next one.
func (c *controller) reconcile(ctx context.Context) error {
	pools, err := c.cluster.Pools()
	if err != nil {
		return fmt.Errorf("while listing WorkerPools: %w", err)
	}
	nodes, err := c.cluster.Nodes(ctx)
	if err != nil {
		return fmt.Errorf("while listing nodes: %w", err)
	}
	relays, err := c.cluster.Relays(ctx)
	if err != nil {
		return fmt.Errorf("while listing fake-atelet pods: %w", err)
	}
	nodeNames := make([]string, len(nodes))
	allocatable := map[string]corev1.ResourceList{}
	for i, n := range nodes {
		nodeNames[i] = n.name
		allocatable[n.name] = n.allocatable
	}

	desired := map[string]desiredWorker{}
	var errs []error
	for _, wp := range pools {
		if _, err := maxActors(wp); err != nil {
			errs = append(errs, err)
			continue
		}
		for i := range int(wp.Spec.Replicas) {
			desired[fakeworker.Name(c.run, wp.Namespace, wp.Name, i)] = desiredWorker{pool: wp, index: i}
		}
	}

	var errMu sync.Mutex
	fail := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		errs = append(errs, err)
	}

	// Retire first, so a Worker that is replaced is recreated in the same pass
	// once no Actor holds it. A draining Worker is retired until it is gone,
	// even if its pool wants it again: it takes no new actors and cannot be
	// undrained. sandbox_class is immutable on a Worker, and editing it on a
	// real pool replaces every worker pod, so its fake Workers are replaced.
	c.mu.Lock()
	var unwanted []string
	for name, w := range c.workers {
		d, ok := desired[name]
		if !ok || w.draining || w.sandboxClass != string(d.pool.Spec.SandboxClass) {
			unwanted = append(unwanted, name)
		}
	}
	c.mu.Unlock()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency)
	for _, name := range unwanted {
		g.Go(func() error {
			if err := c.retire(gctx, name); err != nil {
				fail(err)
			}
			return nil
		})
	}
	_ = g.Wait()

	g, gctx = errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency)
	for _, name := range slices.Sorted(maps.Keys(desired)) {
		d := desired[name]
		g.Go(func() error {
			if err := c.ensure(gctx, name, d, nodeNames, allocatable, relays); err != nil {
				fail(err)
			}
			return nil
		})
	}
	_ = g.Wait()

	for _, wp := range pools {
		if err := c.syncStatus(ctx, wp); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ensure registers the desired Worker if it is not yet, brings its labels in
// line with the pool's, then has its capacity reported if ate-api-server has
// not accepted the current value. The Worker is tracked before CreateWorker
// is called and marked created after, so a failed create is retried by the
// next pass, and one the server committed anyway is never lost: retire and
// deleteAll take a NotFound as done.
func (c *controller) ensure(ctx context.Context, name string, d desiredWorker, nodes []string, allocatable map[string]corev1.ResourceList, relays map[string]string) error {
	c.mu.Lock()
	w := c.workers[name]
	created := w != nil && w.created
	c.mu.Unlock()
	if w == nil {
		if len(nodes) == 0 {
			return errors.New("no benchmark node to place fake Workers on")
		}
		w = &fakeWorker{
			namespace:    d.pool.Namespace,
			pool:         d.pool.Name,
			index:        d.index,
			node:         fakeworker.Node(d.index, nodes),
			labels:       maps.Clone(d.pool.GetLabels()),
			sandboxClass: string(d.pool.Spec.SandboxClass),
		}
		c.mu.Lock()
		c.workers[name] = w
		c.mu.Unlock()
	}
	if !created {
		if err := c.create(ctx, name, w, d.pool); err != nil {
			return err
		}
		c.mu.Lock()
		w.created = true
		c.mu.Unlock()
	}
	if w.draining {
		// Still held by an Actor; recreated once retire has deleted it.
		return nil
	}
	if !maps.Equal(w.labels, d.pool.GetLabels()) {
		if err := c.updateLabels(ctx, name, w, d.pool.GetLabels()); err != nil {
			return err
		}
	}
	want, err := capacity(d.pool, allocatable[w.node])
	if err != nil {
		return err
	}
	if proto.Equal(w.reported, want) {
		return nil
	}
	addr := relays[w.node]
	if addr == "" {
		return fmt.Errorf("no fake-atelet on node %s yet for Worker %s", w.node, name)
	}
	if err := c.relay.Report(ctx, addr, &ateapipb.SetWorkerCapacityRequest{
		Worker:   &ateapipb.ObjectRef{Name: name},
		Capacity: want,
	}); err != nil {
		return fmt.Errorf("while reporting capacity for Worker %s: %w", name, err)
	}
	c.mu.Lock()
	w.reported = want
	c.mu.Unlock()
	return nil
}

// create registers the Worker with the fields the worker syncer would copy
// from the pool and its pod. Epoch stays 0: a rising epoch makes
// ate-api-server crash every Actor on the Worker.
func (c *controller) create(ctx context.Context, name string, w *fakeWorker, wp *atev1alpha1.WorkerPool) error {
	_, err := c.client.CreateWorker(ctx, &ateapipb.CreateWorkerRequest{Worker: &ateapipb.Worker{
		Metadata:        &ateapipb.ResourceMetadata{Name: name},
		WorkerNamespace: wp.Namespace,
		WorkerPool:      wp.Name,
		WorkerPod:       name,
		WorkerPodUid:    fakeworker.PodUID(name),
		NodeName:        w.node,
		Ips:             []string{fakeworker.IP(w.index)},
		SandboxClass:    string(wp.Spec.SandboxClass),
		Labels:          maps.Clone(wp.GetLabels()),
	}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("while creating Worker %s: %w", name, err)
	}
	return nil
}

// updateLabels writes the pool's labels onto a registered Worker, as the
// worker syncer does when a pool's labels change. UpdateWorker replaces the
// whole resource and takes the version it was read at as its precondition, so
// the Worker is read first; a conflicting write fails the call and the next
// pass retries it.
func (c *controller) updateLabels(ctx context.Context, name string, w *fakeWorker, want map[string]string) error {
	got, err := c.client.GetWorker(ctx, &ateapipb.GetWorkerRequest{Worker: &ateapipb.ObjectRef{Name: name}})
	if err != nil {
		return fmt.Errorf("while reading Worker %s: %w", name, err)
	}
	if !maps.Equal(got.GetLabels(), want) {
		got.Labels = maps.Clone(want)
		if _, err := c.client.UpdateWorker(ctx, &ateapipb.UpdateWorkerRequest{Worker: got}); err != nil {
			return fmt.Errorf("while updating Worker %s labels: %w", name, err)
		}
	}
	c.mu.Lock()
	w.labels = maps.Clone(want)
	c.mu.Unlock()
	return nil
}

// retire removes a Worker no pool wants, the way a real one goes when its pod
// does: drained first so nothing new lands on it, deleted once no Actor is
// assigned to it.
func (c *controller) retire(ctx context.Context, name string) error {
	c.mu.Lock()
	w := c.workers[name]
	c.mu.Unlock()
	ref := &ateapipb.ObjectRef{Name: name}
	if !w.draining {
		if _, err := c.client.DrainWorker(ctx, &ateapipb.DrainWorkerRequest{Worker: ref}); err != nil {
			if status.Code(err) == codes.NotFound {
				c.forget(name)
				return nil
			}
			return fmt.Errorf("while draining Worker %s: %w", name, err)
		}
		c.mu.Lock()
		w.draining = true
		c.mu.Unlock()
	}
	assigned, err := c.client.ListWorkerActorAssignments(ctx, &ateapipb.ListWorkerActorAssignmentsRequest{Worker: ref, PageSize: 1})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			c.forget(name)
			return nil
		}
		return fmt.Errorf("while listing Actors on Worker %s: %w", name, err)
	}
	if len(assigned.GetActorAssignments()) > 0 {
		return nil
	}
	if _, err := c.client.DeleteWorker(ctx, &ateapipb.DeleteWorkerRequest{Worker: ref}); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("while deleting Worker %s: %w", name, err)
	}
	c.forget(name)
	return nil
}

func (c *controller) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.workers, name)
}

// syncStatus writes the pool's status as the WorkerPool controller does from
// its Deployment: a replica is a registered Worker, ready once its capacity
// is reported, and the selector is the one worker pods would carry.
func (c *controller) syncStatus(ctx context.Context, wp *atev1alpha1.WorkerPool) error {
	want := atev1alpha1.WorkerPoolStatus{
		Selector: labels.SelectorFromSet(labels.Set{fakeworker.WorkerPoolLabel: wp.Name}).String(),
	}
	c.mu.Lock()
	for _, w := range c.workers {
		if w.namespace != wp.Namespace || w.pool != wp.Name || !w.created || w.draining {
			continue
		}
		want.Replicas++
		if w.reported != nil {
			want.ReadyReplicas++
		}
	}
	c.mu.Unlock()
	if wp.Status == want {
		return nil
	}
	updated := wp.DeepCopy()
	updated.Status = want
	if err := c.cluster.UpdateStatus(ctx, updated); err != nil {
		return fmt.Errorf("while updating WorkerPool %s/%s status: %w", wp.Namespace, wp.Name, err)
	}
	return nil
}

// deleteAll deletes every fake Worker this run registered, as at shutdown,
// after the benchmark has deleted its actors. It keeps going past a failure
// and reports every one.
func (c *controller) deleteAll(ctx context.Context) error {
	c.mu.Lock()
	names := slices.Sorted(maps.Keys(c.workers))
	c.mu.Unlock()
	var g errgroup.Group
	g.SetLimit(c.concurrency)
	errs := make([]error, len(names))
	for i, name := range names {
		g.Go(func() error {
			_, err := c.client.DeleteWorker(ctx, &ateapipb.DeleteWorkerRequest{Worker: &ateapipb.ObjectRef{Name: name}})
			if err != nil && status.Code(err) != codes.NotFound {
				errs[i] = fmt.Errorf("while deleting Worker %s: %w", name, err)
				return nil
			}
			c.forget(name)
			return nil
		})
	}
	_ = g.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	slog.InfoContext(ctx, "Deleted fake Workers", slog.Int("workers", len(names)))
	return nil
}

// maxActors is the pool's actor capacity per Worker: ateom's default, unless
// the pool's annotation overrides it.
func maxActors(wp *atev1alpha1.WorkerPool) (int, error) {
	v, ok := wp.Annotations[fakeworker.MaxActorsAnnotation]
	if !ok {
		return fakeworker.DefaultMaxActors, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("WorkerPool %s/%s: %s must be a positive integer, got %q", wp.Namespace, wp.Name, fakeworker.MaxActorsAnnotation, v)
	}
	return n, nil
}

// capacity is what a real worker of the pool would report on a node with
// allocatable: cpu and memory from the pool's limits, or, with no limit set,
// the node's allocatable, which is what the downward API projects into ateom
// for an unset limit. Quantities are spelled as ateom spells them.
func capacity(wp *atev1alpha1.WorkerPool, allocatable corev1.ResourceList) (*ateapipb.WorkerResources, error) {
	actors, err := maxActors(wp)
	if err != nil {
		return nil, err
	}
	var limits corev1.ResourceList
	if wp.Spec.Template != nil && wp.Spec.Template.Resources != nil {
		limits = wp.Spec.Template.Resources.Limits
	}
	pick := func(name corev1.ResourceName) resource.Quantity {
		if q, ok := limits[name]; ok {
			return q
		}
		return allocatable[name]
	}
	cpu, memory := pick(corev1.ResourceCPU), pick(corev1.ResourceMemory)
	out := &ateapipb.WorkerResources{Actors: int32(actors)}
	var l []*ateapipb.Limits
	if m := cpu.MilliValue(); m > 0 {
		l = append(l, &ateapipb.Limits{Name: "cpu", Quantity: resource.NewMilliQuantity(m, resource.DecimalSI).String()})
	}
	if b := memory.Value(); b > 0 {
		l = append(l, &ateapipb.Limits{Name: "memory", Quantity: resource.NewQuantity(b, resource.BinarySI).String()})
	}
	if len(l) > 0 {
		out.Resources = &ateapipb.Resources{Limits: l}
	}
	return out, nil
}
