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

// Command sdsmintd is the minting SDS server from mint.md: it answers Envoy's
// on-demand SDS requests by issuing a TLS leaf for the requested name, which
// the sni certificate mapper sets to the SNI of the connection being handled.
//
// It listens on a unix domain socket by default. mint.md lists a local-only
// channel as a required control, because leaf private keys transit this
// connection.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/poc/sdsmint"
)

// shutdownGrace is how long a signalled server waits for in-flight RPCs before
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
	caCert := flag.String("ca-cert", "", "path to the MITM CA certificate PEM; generated if it and --ca-key do not exist")
	caKey := flag.String("ca-key", "", "path to the MITM CA private key PEM")
	caConstraint := flag.String("ca-name-constraint", "", "comma-separated DNS domains to name-constrain a generated CA to; empty means unconstrained")
	ttl := flag.Duration("ttl", 5*time.Minute, "leaf certificate lifetime")
	cacheCap := flag.Int("cache-cap", 256, "maximum number of cached leaves")
	rotate := flag.Bool("rotate", false, "proactively push a re-minted leaf at ~2/3 of TTL for every live subscription")
	logLevel := flag.String("log-level", "info", "one of debug, info, warn, error")
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
	if *caCert == "" || *caKey == "" {
		return errors.New("--ca-cert and --ca-key are both required")
	}

	ca, err := loadOrGenerateCA(logger, *caCert, *caKey, *caConstraint)
	if err != nil {
		return err
	}

	minter, err := sdsmint.NewMinter(ca, sdsmint.MinterOptions{
		Validate: sdsmint.AllowGlobs(allow),
		TTL:      *ttl,
		Cap:      *cacheCap,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("building minter: %w", err)
	}

	lis, err := listen(*uds, *addr)
	if err != nil {
		return err
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, sdsmint.NewServer(minter, sdsmint.ServerOptions{
		Logger: logger,
		Rotate: *rotate,
		TTL:    *ttl,
	}))

	logger.Info("sdsmintd listening",
		slog.String("network", lis.Addr().Network()),
		slog.String("address", lis.Addr().String()),
		slog.Any("allow", []string(allow)),
		slog.Duration("ttl", *ttl),
		slog.Bool("rotate", *rotate),
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

// loadOrGenerateCA reads the CA from disk, or creates a throwaway one if
// neither file exists. Generation is a convenience for the PoC; a real
// deployment always supplies a managed CA.
func loadOrGenerateCA(logger *slog.Logger, certPath, keyPath, constraint string) (*sdsmint.CA, error) {
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)

	switch {
	case certErr == nil && keyErr == nil:
		ca, err := sdsmint.LoadCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("loading CA: %w", err)
		}
		logger.Info("loaded MITM CA",
			slog.String("subject", ca.Certificate().Subject.String()),
			slog.Time("not_after", ca.Certificate().NotAfter),
		)
		return ca, nil

	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		var domains []string
		if constraint != "" {
			domains = strings.Split(constraint, ",")
		}
		ca, newCertPEM, newKeyPEM, err := sdsmint.GenerateCA("sdsmint PoC MITM CA", 24*time.Hour, domains)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating CA directory: %w", err)
		}
		if err := os.WriteFile(certPath, newCertPEM, 0o644); err != nil {
			return nil, fmt.Errorf("writing CA cert: %w", err)
		}
		if err := os.WriteFile(keyPath, newKeyPEM, 0o600); err != nil {
			return nil, fmt.Errorf("writing CA key: %w", err)
		}
		logger.Warn("generated a throwaway MITM CA; do not use this outside the PoC",
			slog.String("cert", certPath),
			slog.String("key", keyPath),
			slog.Any("name_constraint", domains),
		)
		return ca, nil

	default:
		return nil, fmt.Errorf("CA cert and key must both exist or both be absent (cert: %v, key: %v)", certErr, keyErr)
	}
}
