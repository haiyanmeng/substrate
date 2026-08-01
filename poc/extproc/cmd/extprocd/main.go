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

// Command extprocd is the egress-authorization ext_proc server for the PoC in
// poc/extproc. It serves both gateway checkpoints from one process, over two
// listeners, against one shared policy table.
//
// Two listeners rather than one because the checkpoints are separately
// configured in Envoy (two clusters), separately failure-isolated, and see
// different halves of the decision. One shared Store because a policy change
// must land on both at the same instant.
//
// Policies are hardcoded; see poc/extproc/hardcoded.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agent-substrate/substrate/poc/extproc"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
)

var (
	connectListen = pflag.String("connect-listen", "127.0.0.1:19600", "Address for the CONNECT-checkpoint ext_proc server.")
	innerListen   = pflag.String("inner-listen", "127.0.0.1:19601", "Address for the inner (post-MITM) checkpoint ext_proc server.")
	adminListen   = pflag.String("admin-listen", "127.0.0.1:19602", "Address for /stats, /healthz and the /echo test upstream.")
	logLevel      = pflag.String("log-level", "info", "Log level: debug, info, warn or error.")
)

func main() {
	pflag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("extprocd exited", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// In production this Store starts empty and a refresher fills it, with
	// readiness gated on the first successful load. The PoC publishes the
	// hardcoded table up front, so /healthz is green immediately.
	snap := extproc.HardcodedSnapshot()
	store := extproc.NewStore(snap)
	stats := extproc.NewStats()

	slog.Info("Policy table loaded",
		slog.Int("rev", snap.Rev),
		slog.Int("actors", snap.Len()))

	connectLis, err := net.Listen("tcp", *connectListen)
	if err != nil {
		return fmt.Errorf("listening for the connect checkpoint: %w", err)
	}
	innerLis, err := net.Listen("tcp", *innerListen)
	if err != nil {
		return fmt.Errorf("listening for the inner checkpoint: %w", err)
	}
	adminLis, err := net.Listen("tcp", *adminListen)
	if err != nil {
		return fmt.Errorf("listening for admin: %w", err)
	}

	admin := &http.Server{
		Handler:           extproc.AdminHandler(store, stats),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		slog.Info("Serving CONNECT checkpoint", slog.String("addr", connectLis.Addr().String()))
		return extproc.NewServer(extproc.CheckpointConnect, store, stats).Serve(gctx, connectLis)
	})
	g.Go(func() error {
		slog.Info("Serving inner checkpoint", slog.String("addr", innerLis.Addr().String()))
		return extproc.NewServer(extproc.CheckpointInner, store, stats).Serve(gctx, innerLis)
	})
	g.Go(func() error {
		slog.Info("Serving admin", slog.String("addr", adminLis.Addr().String()))
		if err := admin.Serve(adminLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), 5*time.Second)
		defer cancel()
		return admin.Shutdown(shutdownCtx)
	})

	return g.Wait()
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
