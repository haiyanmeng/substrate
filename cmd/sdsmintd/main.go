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

// Command sdsmintd is the minting SDS server: it answers Envoy's on-demand SDS
// requests by issuing a TLS leaf for the requested name, which the sni
// certificate mapper sets to the SNI of the connection being handled.
//
// It listens on a unix domain socket by default. A local-only channel is a
// required control here, because leaf private keys transit this connection.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // registers the profiling handlers on DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/sdsmint"
)

// shutdownGrace is how long a signaled server waits for in-flight RPCs before
// tearing open streams down. Envoy's SDS stream never ends on its own, so this
// is the normal path, not the exceptional one.
const shutdownGrace = 2 * time.Second

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	if err := run(); err != nil {
		slog.Error("sdsmintd failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	var allow stringList

	uds := flag.String("uds", "", "unix socket to listen on (mutually exclusive with --addr)")
	addr := flag.String("addr", "", "TCP address to listen on; prefer --uds, since leaf private keys transit this channel")
	caPool := flag.String("ca-pool", "", "path to a localca pool JSON holding the MITM CA, the format substrate mounts its other CAs in; a throwaway pool is generated if the file does not exist")
	caID := flag.String("ca-id", "", "which CA in the pool to sign with; empty takes the first")
	caCertOut := flag.String("ca-cert-out", "", "path to write the trust-anchor certificate PEM to, for configuring the clients that must trust this CA")
	caConstraint := flag.String("ca-name-constraint", "", "comma-separated DNS domains to name-constrain a generated CA to; applies only when generating, since a supplied CA's constraints are fixed at issuance")
	caAllowUnconstrained := flag.Bool("ca-allow-unconstrained", false, "start even if the CA carries no DNS name constraint, which lets its key forge a certificate for any name on the internet")
	caIntermediateTTL := flag.Duration("ca-intermediate-ttl", 0, "delegate leaf signing to an in-memory intermediate of this lifetime, re-issued at ~2/3 of it; 0 signs leaves with the root key directly")
	ttl := flag.Duration("ttl", 5*time.Minute, "leaf certificate lifetime")
	cacheCap := flag.Int("cache-cap", 256, "maximum number of cached leaves")
	rotate := flag.Bool("rotate", false, "proactively push a re-minted leaf at ~2/3 of TTL for every live subscription")
	idle := flag.Duration("idle", 0, "withdraw a secret the proxy has not re-requested in this long, so the live set can shrink; 0 holds every name for the life of the stream")
	logLevel := flag.String("log-level", "info", "one of debug, info, warn, error")
	metricsAddr := flag.String("metrics-addr", "", "TCP address to serve /metrics and /debug/pprof on; empty disables both")
	flag.Var(&allow, "allow", "hostname pattern that may be minted, e.g. '*.example.com'; repeatable and required")
	flag.Parse()

	logger, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	if len(allow) == 0 {
		return errors.New("--allow is required: refusing to start a minting service with no hostname allowlist")
	}
	if (*uds == "") == (*addr == "") {
		return errors.New("exactly one of --uds or --addr must be set")
	}
	if *caPool == "" {
		return errors.New("--ca-pool is required")
	}

	ca, err := loadOrGenerateCA(logger, *caPool, *caID, *caConstraint, sdsmint.Options{
		AllowUnconstrained:   *caAllowUnconstrained,
		IntermediateLifetime: *caIntermediateTTL,
		Logger:               logger,
	})
	if err != nil {
		return err
	}
	if *caCertOut != "" {
		if err := writeFileAtomic(*caCertOut, ca.CertificatePEM(), 0o644); err != nil {
			return fmt.Errorf("writing the trust anchor: %w", err)
		}
	}

	metrics := &sdsmint.Metrics{}

	minter, err := sdsmint.NewMinter(ca, sdsmint.MinterOptions{
		Validate: sdsmint.AllowGlobs(allow),
		TTL:      *ttl,
		Cap:      *cacheCap,
		Logger:   logger,
		Metrics:  metrics,
	})
	if err != nil {
		return fmt.Errorf("building minter: %w", err)
	}

	if *metricsAddr != "" {
		if err := serveMetrics(logger, *metricsAddr, metrics); err != nil {
			return err
		}
	}

	lis, err := listen(*uds, *addr)
	if err != nil {
		return err
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, sdsmint.NewServer(minter, sdsmint.ServerOptions{
		Logger:      logger,
		Rotate:      *rotate,
		TTL:         *ttl,
		IdleTimeout: *idle,
		Metrics:     metrics,
	}))

	logger.Info("sdsmintd listening",
		slog.String("network", lis.Addr().Network()),
		slog.String("address", lis.Addr().String()),
		slog.Any("allow", []string(allow)),
		slog.Duration("ttl", *ttl),
		slog.Bool("rotate", *rotate),
		slog.Duration("idle", *idle),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		// GracefulStop waits for in-flight RPCs to finish, but an xDS stream
		// is long-lived by design and only ends when Envoy closes it. Waiting
		// on it unconditionally deadlocks shutdown, so fall back to a hard
		// stop after a grace period.
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGrace):
			logger.Warn("graceful shutdown timed out; closing open streams",
				slog.Duration("grace", shutdownGrace))
			grpcServer.Stop()
		}
	}()

	if err := grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// serveMetrics starts the observability listener and returns once it is bound,
// so a harness that reads /metrics immediately after startup cannot race it.
//
// It is a separate TCP address rather than something on the SDS socket because
// it is for the operator, not the proxy, and because pprof over the same pipe
// that carries private keys is a bad idea.
func serveMetrics(logger *slog.Logger, addr string, metrics *sdsmint.Metrics) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening for metrics on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(metrics.Snapshot()); err != nil {
			logger.Warn("writing metrics", slog.String("error", err.Error()))
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// net/http/pprof registers onto DefaultServeMux in its init, so route to
	// that rather than re-registering each handler by hand.
	mux.Handle("/debug/pprof/", http.DefaultServeMux)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics listener failed", slog.String("error", err.Error()))
		}
	}()
	logger.Info("serving metrics", slog.String("address", lis.Addr().String()))
	return nil
}

// writeFileAtomic writes via a temporary name and renames into place, so a
// concurrent reader sees either the old contents or the new ones and never a
// half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}

func listen(uds, addr string) (net.Listener, error) {
	if addr != "" {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listening on %s: %w", addr, err)
		}
		return lis, nil
	}

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

// loadOrGenerateCA reads the CA from a localca pool, or creates a throwaway
// pool if the file does not exist. Generation is a convenience for the PoC; a
// real deployment mounts a pool Secret, exactly as podcertcontroller and
// ate-api-server already do for their own CAs.
func loadOrGenerateCA(logger *slog.Logger, poolPath, id, constraint string, opts sdsmint.Options) (*sdsmint.CA, error) {
	poolBytes, err := os.ReadFile(poolPath)

	switch {
	case err == nil:
		pool, err := localca.Unmarshal(poolBytes)
		if err != nil {
			return nil, fmt.Errorf("parsing CA pool %s: %w", poolPath, err)
		}
		ca, err := sdsmint.FromPool(pool, id, opts)
		if err != nil {
			return nil, fmt.Errorf("loading CA from %s: %w", poolPath, err)
		}
		logCA(logger, "loaded MITM CA", ca, opts)
		return ca, nil

	case errors.Is(err, os.ErrNotExist):
		var domains []string
		if constraint != "" {
			domains = strings.Split(constraint, ",")
		}
		ca, entry, err := sdsmint.GenerateCA("sdsmint PoC MITM CA", 24*time.Hour, domains, opts)
		if err != nil {
			if len(domains) == 0 && !opts.AllowUnconstrained {
				return nil, fmt.Errorf("%w; pass --ca-name-constraint to bound what this CA may impersonate", err)
			}
			return nil, err
		}
		newPoolBytes, err := localca.Marshal(&localca.Pool{CAs: []*localca.CA{entry}})
		if err != nil {
			return nil, fmt.Errorf("serializing the generated CA pool: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(poolPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating CA pool directory: %w", err)
		}
		// 0600: the pool carries the private key, unlike the trust anchor
		// written by --ca-cert-out.
		if err := writeFileAtomic(poolPath, newPoolBytes, 0o600); err != nil {
			return nil, fmt.Errorf("writing CA pool: %w", err)
		}
		logger.Warn("generated a throwaway MITM CA; do not use this outside the PoC",
			slog.String("pool", poolPath),
			slog.Any("name_constraint", domains),
		)
		logCA(logger, "generated MITM CA", ca, opts)
		return ca, nil

	default:
		return nil, fmt.Errorf("reading CA pool %s: %w", poolPath, err)
	}
}

func logCA(logger *slog.Logger, msg string, ca *sdsmint.CA, opts sdsmint.Options) {
	anchor := ca.Certificate()
	attrs := []any{
		slog.String("subject", anchor.Subject.String()),
		slog.Time("not_after", anchor.NotAfter),
		slog.Any("name_constraint", anchor.PermittedDNSDomains),
	}
	if opts.IntermediateLifetime > 0 {
		attrs = append(attrs,
			slog.Duration("intermediate_ttl", opts.IntermediateLifetime),
			slog.String("signing_as", ca.IssuerCertificate().Subject.CommonName),
		)
	}
	if len(anchor.PermittedDNSDomains) == 0 {
		logger.Warn(msg+" (UNCONSTRAINED: this key can forge any name)", attrs...)
		return
	}
	logger.Info(msg, attrs...)
}
