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

// This file holds every socket the process opens: the SDS one, which is always
// a unix domain socket because leaf private keys transit it, and the two
// observability ones.

package sdsmint

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // registers the profiling handlers on DefaultServeMux
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// listen binds the SDS socket. There is no TCP alternative on purpose: leaf
// private keys transit this channel, and a unix socket restricted to the
// proxy's UID is the only reach that is ever wanted.
func listen(uds string) (net.Listener, error) {
	// A stale socket from a previous run would make Listen fail with
	// "address already in use" even though nothing is running.
	if err := os.Remove(uds); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale socket %s: %w", uds, err)
	}
	if err := os.MkdirAll(filepath.Dir(uds), 0o755); err != nil {
		return nil, fmt.Errorf("creating socket directory: %w", err)
	}
	lis, err := net.Listen("unix", uds)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", uds, err)
	}
	// Only the proxy should be able to ask for certificates.
	if err := os.Chmod(uds, 0o600); err != nil {
		lis.Close()
		return nil, fmt.Errorf("restricting socket permissions: %w", err)
	}
	return lis, nil
}

// serveHTTP starts an observability listener and returns once it is bound, so
// a harness that reads an endpoint immediately after startup cannot race it.
func serveHTTP(logger *slog.Logger, name, addr string, handler http.Handler) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening for %s on %s: %w", name, addr, err)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(name+" listener failed", slog.String("error", err.Error()))
		}
	}()
	logger.Info("serving "+name, slog.String("address", lis.Addr().String()))
	return nil
}

// metricsHandler serves whatever serverboot.InitMetrics registered with the
// default Prometheus registerer.
//
// serverboot.StartMetricsServer serves the same thing, but it binds inside the
// goroutine; serveHTTP binds first, deliberately.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return mux
}

// pprofHandler is DefaultServeMux, which is where net/http/pprof registers in
// its init.
//
// This is a separate listener from the metrics one because the two want
// different reach. Metrics has to be scrapable at the pod IP. Profiling does
// not: it is unauthenticated, /debug/pprof/cmdline hands out this process's
// arguments, a CPU profile is a resource sink whose duration the caller picks,
// and the mux is the default one rather than one this package owns, so any
// dependency that registers a route is served here too. Loopback keeps all of
// that behind a kubectl port-forward or an exec.
//
// Nothing here exposes the signing key: a Go heap profile is sampled stack
// traces and allocation counts, not heap contents.
func pprofHandler() http.Handler { return http.DefaultServeMux }
