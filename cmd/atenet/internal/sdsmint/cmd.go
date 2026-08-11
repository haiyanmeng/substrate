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

// This file is the `atenet sdsmint` cobra wrapper. It listens on a unix domain
// socket by default: a local-only channel is a required control here, because
// leaf private keys transit this connection. See doc.go for the package.

package sdsmint

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/internal/localca"
)

// shutdownGrace is how long a signaled server waits for in-flight RPCs before
// tearing open streams down. Envoy's SDS stream never ends on its own, so this
// is the normal path, not the exceptional one.
const shutdownGrace = 2 * time.Second

type config struct {
	UDS                  string
	Addr                 string
	CAPool               string
	CAID                 string
	CACertOut            string
	CAConstraint         string
	CAAllowUnconstrained bool
	CAIntermediateTTL    time.Duration
	TTL                  time.Duration
	CacheCap             int
	Rotate               bool
	Idle                 time.Duration
	LogLevel             string
	MetricsAddr          string
	Allow                []string
	AllowAny             bool
}

func NewSdsmintCmd() *cobra.Command {
	var cfg config

	cmd := &cobra.Command{
		Use:   "sdsmint",
		Short: "Minting SDS server that issues a TLS leaf for the SNI Envoy asks for",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}

	cmd.Flags().StringVar(&cfg.UDS, "uds", "", "unix socket to listen on (mutually exclusive with --addr)")
	cmd.Flags().StringVar(&cfg.Addr, "addr", "", "TCP address to listen on; prefer --uds, since leaf private keys transit this channel")
	cmd.Flags().StringVar(&cfg.CAPool, "ca-pool", "", "path to a localca pool JSON holding the MITM CA, the format substrate mounts its other CAs in; a throwaway pool is generated if the file does not exist")
	cmd.Flags().StringVar(&cfg.CAID, "ca-id", "", "which CA in the pool to sign with; empty takes the first")
	cmd.Flags().StringVar(&cfg.CACertOut, "ca-cert-out", "", "path to write the trust-anchor certificate PEM to, for configuring the clients that must trust this CA")
	cmd.Flags().StringVar(&cfg.CAConstraint, "ca-name-constraint", "", "comma-separated DNS domains to name-constrain a generated CA to; applies only when generating, since a supplied CA's constraints are fixed at issuance")
	cmd.Flags().BoolVar(&cfg.CAAllowUnconstrained, "ca-allow-unconstrained", false, "start even if the CA carries no DNS name constraint, which lets its key forge a certificate for any name on the internet")
	cmd.Flags().DurationVar(&cfg.CAIntermediateTTL, "ca-intermediate-ttl", 0, "delegate leaf signing to an in-memory intermediate of this lifetime, re-issued at ~2/3 of it; 0 signs leaves with the root key directly")
	cmd.Flags().DurationVar(&cfg.TTL, "ttl", 5*time.Minute, "leaf certificate lifetime")
	cmd.Flags().IntVar(&cfg.CacheCap, "cache-cap", 256, "maximum number of cached leaves")
	cmd.Flags().BoolVar(&cfg.Rotate, "rotate", false, "proactively push a re-minted leaf at ~2/3 of TTL for every live subscription")
	cmd.Flags().DurationVar(&cfg.Idle, "idle", 0, "withdraw a secret the proxy has not re-requested in this long, so the live set can shrink; 0 holds every name for the life of the stream")
	cmd.Flags().StringVar(&cfg.LogLevel, "log-level", "info", "one of debug, info, warn, error")
	cmd.Flags().StringVar(&cfg.MetricsAddr, "metrics-addr", "", "TCP address to serve /metrics and /debug/pprof on; empty disables both")
	// StringArray rather than StringSlice: this flag is the entire egress
	// policy, and StringSlice would split a pattern containing a comma into two
	// patterns instead of rejecting it.
	cmd.Flags().StringArrayVar(&cfg.Allow, "allow", nil, "hostname pattern that may be minted, e.g. '*.example.com'; repeatable, and required unless --allow-any is set")
	cmd.Flags().BoolVar(&cfg.AllowAny, "allow-any", false, "mint for any syntactically valid hostname, at any depth and in any TLD; only for a gateway whose destination policy lives elsewhere, since it leaves nothing here to refuse a name")

	return cmd
}

func run(ctx context.Context, cfg config) error {
	logger, err := newLogger(cfg.LogLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	// Exactly one of the two, so that "which names does this gateway mint
	// for" is answered in one place. Accepting both would let an --allow list
	// sit in the manifest looking like policy while --allow-any quietly made
	// it decorative.
	switch {
	case len(cfg.Allow) == 0 && !cfg.AllowAny:
		return errors.New("--allow is required: refusing to start a minting service with no hostname allowlist; pass --allow-any to mint for every name deliberately")
	case len(cfg.Allow) > 0 && cfg.AllowAny:
		return errors.New("--allow and --allow-any are mutually exclusive: --allow-any already admits every name the patterns would")
	}
	if (cfg.UDS == "") == (cfg.Addr == "") {
		return errors.New("exactly one of --uds or --addr must be set")
	}
	if cfg.CAPool == "" {
		return errors.New("--ca-pool is required")
	}

	ca, err := loadOrGenerateCA(logger, cfg.CAPool, cfg.CAID, cfg.CAConstraint, Options{
		AllowUnconstrained:   cfg.CAAllowUnconstrained,
		IntermediateLifetime: cfg.CAIntermediateTTL,
		Logger:               logger,
	})
	if err != nil {
		return err
	}
	if cfg.CACertOut != "" {
		if err := writeFileAtomic(cfg.CACertOut, ca.CertificatePEM(), 0o644); err != nil {
			return fmt.Errorf("writing the trust anchor: %w", err)
		}
	}

	metrics := &Metrics{}

	validate := AllowGlobs(cfg.Allow)
	if cfg.AllowAny {
		validate = AllowAny()
	}

	// Named m, not minter: minter is now an unexported type in this package.
	m, err := NewMinter(ca, MinterOptions{
		Validate: validate,
		TTL:      cfg.TTL,
		Cap:      cfg.CacheCap,
		Logger:   logger,
		Metrics:  metrics,
	})
	if err != nil {
		return fmt.Errorf("building minter: %w", err)
	}

	if cfg.MetricsAddr != "" {
		if err := serveMetrics(logger, cfg.MetricsAddr, metrics); err != nil {
			return err
		}
	}

	lis, err := listen(cfg.UDS, cfg.Addr)
	if err != nil {
		return err
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, NewServer(m, ServerOptions{
		Logger:      logger,
		Rotate:      cfg.Rotate,
		TTL:         cfg.TTL,
		IdleTimeout: cfg.Idle,
		Metrics:     metrics,
	}))

	logger.Info("sdsmint listening",
		slog.String("network", lis.Addr().Network()),
		slog.String("address", lis.Addr().String()),
		// "any" rather than an empty list, so that the one log line an
		// operator reads to see what this gateway will sign for cannot be
		// mistaken for "nothing".
		slog.Any("allow", allowForLog(cfg)),
		slog.Duration("ttl", cfg.TTL),
		slog.Bool("rotate", cfg.Rotate),
		slog.Duration("idle", cfg.Idle),
	)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
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

// allowForLog renders the effective allowlist for the startup line.
func allowForLog(cfg config) []string {
	if cfg.AllowAny {
		return []string{"any"}
	}
	return cfg.Allow
}

// serveMetrics starts the observability listener and returns once it is bound,
// so a harness that reads /metrics immediately after startup cannot race it.
//
// It is a separate TCP address rather than something on the SDS socket because
// it is for the operator, not the proxy, and because pprof over the same pipe
// that carries private keys is a bad idea.
func serveMetrics(logger *slog.Logger, addr string, metrics *Metrics) error {
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
func loadOrGenerateCA(logger *slog.Logger, poolPath, id, constraint string, opts Options) (*CA, error) {
	poolBytes, err := os.ReadFile(poolPath)

	switch {
	case err == nil:
		pool, err := localca.Unmarshal(poolBytes)
		if err != nil {
			return nil, fmt.Errorf("parsing CA pool %s: %w", poolPath, err)
		}
		ca, err := FromPool(pool, id, opts)
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
		ca, entry, err := GenerateCA("sdsmint PoC MITM CA", 24*time.Hour, domains, opts)
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

func logCA(logger *slog.Logger, msg string, ca *CA, opts Options) {
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
