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

// This file turns a parsed config into a running process: signer, minter,
// listeners, gRPC server, shutdown. The flags it reads are cmd.go's.

package sdsmint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/serverboot"
)

func run(ctx context.Context, cfg config) error {
	logger, err := newLogger(cfg.LogLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	if cfg.UDSPath == "" {
		return errors.New("--uds-path is required")
	}
	if cfg.CAPoolPath == "" {
		return errors.New("--ca-pool-path is required")
	}
	if err := cfg.validateTTL(); err != nil {
		return err
	}

	signer, err := loadSigner(cfg.CAPoolPath, cfg.CAID, certauth.Options{KeyRotation: cfg.LeafCertTTL})
	if err != nil {
		return err
	}

	// Instruments exist only when something exports them. A nil *metrics
	// no-ops every call site, which also keeps enabled() from paying for the
	// proto.Size of each response nobody will read.
	var counters *metrics
	if cfg.MetricsAddr != "" {
		mp, err := serverboot.InitMetrics(ctx, serviceName)
		if err != nil {
			return fmt.Errorf("initializing metrics: %w", err)
		}
		defer serverboot.ShutdownProvider("MeterProvider", mp.Shutdown)
		if counters, err = newMetrics(mp); err != nil {
			return err
		}
	}

	// Named m because minter is the type.
	m, err := newMinter(signer, minterOptions{
		TTL:           cfg.LeafCertTTL,
		CacheCapacity: cfg.LeafCacheSize,
		Logger:        logger,
		Metrics:       counters,
	})
	if err != nil {
		return fmt.Errorf("building minter: %w", err)
	}

	if cfg.MetricsAddr != "" {
		if err := serveHTTP(logger, "metrics", cfg.MetricsAddr, metricsHandler()); err != nil {
			return err
		}
	}
	if cfg.PprofAddr != "" {
		if err := serveHTTP(logger, "pprof", cfg.PprofAddr, pprofHandler()); err != nil {
			return err
		}
	}

	lis, err := listen(cfg.UDSPath)
	if err != nil {
		return err
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	secretservice.RegisterSecretDiscoveryServiceServer(grpcServer, newServer(m, serverOptions{
		Logger:      logger,
		TTL:         cfg.LeafCertTTL,
		IdleTimeout: cfg.Idle,
		Metrics:     counters,
	}))

	logger.Info("sdsmint listening",
		slog.String("network", lis.Addr().Network()),
		slog.String("address", lis.Addr().String()),
		slog.Any("config", cfg),
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

		// shutdownGrace is how long a signaled server waits for in-flight RPCs before
		// tearing open streams down. Envoy's SDS stream never ends on its own, so this
		// is the normal path, not the exceptional one.
		const shutdownGrace = 2 * time.Second
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

func loadSigner(poolPath, id string, opts certauth.Options) (*certauth.Signer, error) {
	poolBytes, err := os.ReadFile(poolPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA pool %s: %w", poolPath, err)
	}
	pool, err := localca.Unmarshal(poolBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing CA pool %s: %w", poolPath, err)
	}
	signer, err := certauth.New(pool, id, opts)
	if err != nil {
		return nil, fmt.Errorf("loading CA from %s: %w", poolPath, err)
	}
	return signer, nil
}
