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

// Command fake-workersync stands in for atecontroller's worker syncer in
// control-plane benchmarks. It registers --workers-per-node fake Workers on
// every node carrying --node-label, none of them backed by a worker pod, and
// deletes them when it shuts down. fake-atelet on each of those nodes reports
// their capacity.
//
// The real worker syncer deletes every Worker with no live pod when it
// starts, so atecontroller must not run alongside this. See
// benchmarking/workloads/deploy.sh --fake-data-plane.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/agent-substrate/substrate/internal/benchmarking/boomer/boomerutil"
	"github.com/agent-substrate/substrate/internal/benchmarking/fakeworker"
	"github.com/agent-substrate/substrate/internal/serverboot"
	"github.com/agent-substrate/substrate/internal/version"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	apiEndpoint    = pflag.String("api-endpoint", "k8s:///api.ate-system.svc.cluster.local:443", "ate-api-server gRPC dial target.")
	run            = pflag.String("run", "", "Run prefix shared with fake-atelet. Required.")
	workersPerNode = pflag.Int("workers-per-node", 0, "Fake Workers to register on each node. Required.")
	nodeLabel      = pflag.String("node-label", "", `Label selector for the benchmark nodes, as "key=value". Required.`)
	namespace      = pflag.String("worker-namespace", "benchmark-workloads", "WorkerNamespace recorded on each fake Worker.")
	pool           = pflag.String("worker-pool", "benchmark-ateom", "WorkerPool recorded on each fake Worker.")
	sandboxClass   = pflag.String("sandbox-class", "gvisor", "Sandbox class recorded on each fake Worker; must match the benchmark ActorTemplates'.")
	workerLabels   = pflag.String("worker-labels", "workload=benchmark-ateom", `Labels recorded on each fake Worker, as "k=v,k=v"; must satisfy the templates' workerSelector.`)
	concurrency    = pflag.Int("concurrency", 32, "Worker create and delete calls in flight.")
	cleanup        = pflag.Bool("cleanup", false, "Delete the fake Workers for --run on the labeled nodes, then exit.")
	deleteTimeout  = pflag.Duration("delete-timeout", 5*time.Minute, "How long shutdown spends deleting the fake Workers.")

	showVersion  = pflag.Bool("version", false, "Print version and exit.")
	logLevelFlag = pflag.String("log-level", "info", "Minimum log level: debug, info, warn, or error.")
)

func main() {
	pflag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}
	serverboot.InitLogger()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := serverboot.SetLogLevel(*logLevelFlag); err != nil {
		serverboot.Fatal(ctx, "Invalid --log-level", err)
	}

	if err := fakeworker.ValidateRun(*run); err != nil {
		serverboot.Fatal(ctx, "Invalid --run", err)
	}
	if err := fakeworker.ValidateWorkersPerNode(*workersPerNode); err != nil {
		serverboot.Fatal(ctx, "Invalid --workers-per-node", err)
	}
	if *nodeLabel == "" {
		serverboot.Fatal(ctx, "Invalid flags", errors.New("--node-label is required"))
	}
	if *concurrency < 1 {
		serverboot.Fatal(ctx, "Invalid --concurrency", fmt.Errorf("must be at least 1, got %d", *concurrency))
	}
	labels, err := parseLabels(*workerLabels)
	if err != nil {
		serverboot.Fatal(ctx, "Invalid --worker-labels", err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		serverboot.Fatal(ctx, "Failed to load in-cluster config", err)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to create Kubernetes client", err)
	}
	nodes, err := labeledNodes(ctx, kc, *nodeLabel)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to list the benchmark nodes", err)
	}

	conn, client, err := boomerutil.DialControl(*apiEndpoint)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to dial ate-api-server", err)
	}
	defer conn.Close()

	r := &registrar{
		client:         client,
		run:            *run,
		workersPerNode: *workersPerNode,
		namespace:      *namespace,
		pool:           *pool,
		sandboxClass:   *sandboxClass,
		labels:         labels,
		concurrency:    *concurrency,
	}

	if *cleanup {
		if err := r.unregister(ctx, nodes); err != nil {
			serverboot.Fatal(ctx, "Failed to delete fake Workers", err)
		}
		return
	}

	if err := r.register(ctx, nodes); err != nil {
		serverboot.Fatal(ctx, "Failed to register fake Workers", err)
	}
	<-ctx.Done()

	// Shutdown runs after boomer has deleted its actors (teardown order), so
	// no Actor is left assigned to a Worker deleted here.
	delCtx, cancel := context.WithTimeout(context.Background(), *deleteTimeout)
	defer cancel()
	if err := r.unregister(delCtx, nodes); err != nil {
		slog.ErrorContext(delCtx, "Failed to delete every fake Worker; run with --cleanup, or let the next atecontroller's startup sweep remove the rest", slog.Any("err", err))
	}
}

// labeledNodes lists the names of the nodes matching selector, sorted.
func labeledNodes(ctx context.Context, kc kubernetes.Interface, selector string) ([]string, error) {
	list, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.Name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no node matches %q", selector)
	}
	slices.Sort(names)
	return names, nil
}

// parseLabels parses "k=v,k=v". Empty yields no labels.
func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("label %q is not key=value", pair)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("label %q given more than once", k)
		}
		out[k] = v
	}
	return out, nil
}
