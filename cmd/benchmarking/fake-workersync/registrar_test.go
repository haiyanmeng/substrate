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
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/agent-substrate/substrate/internal/benchmarking/fakeworker"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeControl is an in-memory Worker registry.
type fakeControl struct {
	mu        sync.Mutex
	workers   map[string]*ateapipb.Worker
	createErr map[string]error
	deleteErr map[string]error
}

func newFakeControl() *fakeControl {
	return &fakeControl{workers: map[string]*ateapipb.Worker{}, createErr: map[string]error{}, deleteErr: map[string]error{}}
}

func (f *fakeControl) CreateWorker(_ context.Context, in *ateapipb.CreateWorkerRequest, _ ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetWorker().GetMetadata().GetName()
	if err := f.createErr[name]; err != nil {
		return nil, err
	}
	if _, ok := f.workers[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Worker %s already exists", name)
	}
	f.workers[name] = in.GetWorker()
	return in.GetWorker(), nil
}

func (f *fakeControl) DeleteWorker(_ context.Context, in *ateapipb.DeleteWorkerRequest, _ ...grpc.CallOption) (*ateapipb.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := in.GetWorker().GetName()
	if err := f.deleteErr[name]; err != nil {
		return nil, err
	}
	w, ok := f.workers[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Worker %s not found", name)
	}
	delete(f.workers, name)
	return w, nil
}

func (f *fakeControl) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.workers))
}

func newRegistrar(c workerClient) *registrar {
	return &registrar{
		client:         c,
		run:            "r1",
		workersPerNode: 3,
		namespace:      "benchmark-workloads",
		pool:           "benchmark-ateom",
		sandboxClass:   "gvisor",
		labels:         map[string]string{"workload": "benchmark-ateom"},
		concurrency:    4,
	}
}

func TestRegisterCreatesEveryWorkerWithItsFields(t *testing.T) {
	c := newFakeControl()
	r := newRegistrar(c)
	nodes := []string{"node-a", "node-b"}
	if err := r.register(context.Background(), nodes); err != nil {
		t.Fatalf("register: %v", err)
	}
	var want []string
	for _, n := range nodes {
		want = append(want, fakeworker.Names("r1", n, 3)...)
	}
	slices.Sort(want)
	if got := c.names(); !slices.Equal(got, want) {
		t.Fatalf("registered %v, want %v", got, want)
	}

	// The fields placement and fake-atelet depend on.
	name := fakeworker.Name("r1", "node-b", 2)
	w := c.workers[name]
	switch {
	case w.GetNodeName() != "node-b":
		t.Errorf("NodeName = %q, want node-b: fake-atelet reports capacity only for its own node", w.GetNodeName())
	case w.GetSandboxClass() != "gvisor":
		t.Errorf("SandboxClass = %q, want gvisor", w.GetSandboxClass())
	case w.GetLabels()["workload"] != "benchmark-ateom":
		t.Errorf("Labels = %v, want workload=benchmark-ateom, which the benchmark templates select", w.GetLabels())
	case w.GetWorkerPod() != name || w.GetWorkerPodUid() != fakeworker.PodUID(name):
		t.Errorf("WorkerPod/WorkerPodUid = %q/%q, want %q/%q", w.GetWorkerPod(), w.GetWorkerPodUid(), name, fakeworker.PodUID(name))
	case !slices.Equal(w.GetIps(), []string{fakeworker.IP(2)}):
		t.Errorf("Ips = %v, want [%s]", w.GetIps(), fakeworker.IP(2))
	case w.GetEpoch() != 0:
		t.Errorf("Epoch = %d, want 0: a rising epoch crashes the Worker's Actors", w.GetEpoch())
	case w.GetWorkerNamespace() != "benchmark-workloads" || w.GetWorkerPool() != "benchmark-ateom":
		t.Errorf("WorkerNamespace/WorkerPool = %q/%q", w.GetWorkerNamespace(), w.GetWorkerPool())
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	c := newFakeControl()
	r := newRegistrar(c)
	for i := range 2 {
		if err := r.register(context.Background(), []string{"node-a"}); err != nil {
			t.Fatalf("register #%d: %v", i+1, err)
		}
	}
	if got := len(c.names()); got != 3 {
		t.Errorf("%d Workers after registering twice, want 3", got)
	}
}

func TestRegisterSurfacesOtherErrors(t *testing.T) {
	c := newFakeControl()
	c.createErr[fakeworker.Name("r1", "node-a", 1)] = status.Error(codes.InvalidArgument, "bad worker")
	if err := newRegistrar(c).register(context.Background(), []string{"node-a"}); err == nil {
		t.Error("register succeeded with a Worker ate-api-server rejected")
	}
}

func TestRegisterRejectsNoNodes(t *testing.T) {
	if err := newRegistrar(newFakeControl()).register(context.Background(), nil); err == nil {
		t.Error("register succeeded with no nodes, leaving every fake-atelet without Workers")
	}
}

func TestUnregisterDeletesEveryWorkerAndToleratesMissing(t *testing.T) {
	c := newFakeControl()
	r := newRegistrar(c)
	if err := r.register(context.Background(), []string{"node-a", "node-b"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// One already gone, as after a cut-short shutdown.
	if _, err := c.DeleteWorker(context.Background(), &ateapipb.DeleteWorkerRequest{Worker: &ateapipb.ObjectRef{Name: fakeworker.Name("r1", "node-a", 0)}}); err != nil {
		t.Fatal(err)
	}
	if err := r.unregister(context.Background(), []string{"node-a", "node-b"}); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if got := c.names(); len(got) != 0 {
		t.Errorf("Workers left after unregister: %v", got)
	}
}

func TestUnregisterKeepsGoingPastAFailure(t *testing.T) {
	c := newFakeControl()
	r := newRegistrar(c)
	if err := r.register(context.Background(), []string{"node-a"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	failing := fakeworker.Name("r1", "node-a", 0)
	c.deleteErr[failing] = errors.New("ate-api-server unavailable")
	if err := r.unregister(context.Background(), []string{"node-a"}); err == nil {
		t.Error("unregister succeeded with a failed delete")
	}
	if got := c.names(); !slices.Equal(got, []string{failing}) {
		t.Errorf("Workers left = %v, want only the one whose delete failed", got)
	}
}

func TestLabeledNodes(t *testing.T) {
	node := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	kc := fake.NewClientset(
		node("node-b", map[string]string{"ate.dev/fake-data-plane": "true"}),
		node("node-a", map[string]string{"ate.dev/fake-data-plane": "true"}),
		node("node-c", nil),
	)
	got, err := labeledNodes(context.Background(), kc, "ate.dev/fake-data-plane=true")
	if err != nil {
		t.Fatalf("labeledNodes: %v", err)
	}
	if want := []string{"node-a", "node-b"}; !slices.Equal(got, want) {
		t.Errorf("labeledNodes = %v, want %v", got, want)
	}
	if _, err := labeledNodes(context.Background(), kc, "ate.dev/fake-data-plane=absent"); err == nil {
		t.Error("labeledNodes succeeded with no matching node")
	}
}

func TestParseLabels(t *testing.T) {
	got, err := parseLabels("workload=benchmark-ateom, tier=bench")
	if err != nil {
		t.Fatalf("parseLabels: %v", err)
	}
	if want := map[string]string{"workload": "benchmark-ateom", "tier": "bench"}; !maps.Equal(got, want) {
		t.Errorf("parseLabels = %v, want %v", got, want)
	}
	if got, err := parseLabels(""); err != nil || len(got) != 0 {
		t.Errorf("parseLabels(\"\") = %v, %v; want empty", got, err)
	}
	for _, bad := range []string{"novalue", "=v", "a=1,a=2"} {
		if _, err := parseLabels(bad); err == nil {
			t.Errorf("parseLabels(%q) = nil error, want one", bad)
		}
	}
}
