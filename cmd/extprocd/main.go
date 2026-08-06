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

// Command extprocd is the egress gateway's policy enforcement point.
//
// It runs as a sidecar in the atenet-egress pod and answers Envoy's ext_proc
// calls at the CONNECT checkpoint: given the actor identity Envoy authenticated
// in the TLS handshake and the destination the actor asked for, allow, deny, or
// route to MITM. It fails closed -- Envoy is configured with
// failure_mode_allow: false, so extprocd being down stops actor egress rather
// than opening it.
//
// It also assembles the gateway's client-CA bundle; see
// extproc.ClientCABundler for why that lives here.
//
// It answers a second checkpoint on the tunnel's inner request after MITM
// (extproc.CheckpointInner), which is where the two policies that need a
// hostname are decided and where credentials are injected. That one is opt-in:
// --inner-listen is empty by default, and while it is empty the CONNECT
// checkpoint denies ALLOW_BY_HOSTNAME and BASIC_CREDENTIAL_INJECT rather than
// letting them through undecided. manifests/ate-install/atenet-egress.yaml sets
// it; the Envoy side needs three pieces to match, two of which fail silently,
// and they are documented there.
//
// The policy table is hardcoded. --policy-source has no default, so a
// deployment has to say so out loud.
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

	"github.com/agent-substrate/substrate/internal/extproc"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
)

// policySourceHardcoded is the only value --policy-source accepts today. It is
// spelled out rather than defaulted so that "which policies is this gateway
// enforcing?" is answerable from the Deployment alone.
const policySourceHardcoded = "hardcoded"

var (
	connectListen = pflag.String("connect-listen", "127.0.0.1:19600", "Address for the CONNECT-checkpoint ext_proc server. Bind to pod loopback: this port authorizes egress and must not be reachable off-pod.")
	innerListen   = pflag.String("inner-listen", "", "Address for the inner (post-MITM) checkpoint ext_proc server. Empty disables it, which makes the CONNECT checkpoint deny the policies that need it.")
	adminListen   = pflag.String("admin-listen", "127.0.0.1:19602", "Address for /stats and /healthz.")
	policySource  = pflag.String("policy-source", "", `Where the policy table comes from. Only "hardcoded" is implemented.`)
	clientCAIn    = pflag.StringSlice("client-ca-in", nil, "PEM files to concatenate into the gateway's client-CA bundle, in order.")
	clientCAOut   = pflag.String("client-ca-out", "", "Where to write the concatenated client-CA bundle. Empty disables bundling.")
	clientCAEvery = pflag.Duration("client-ca-refresh", time.Minute, "How often to re-read --client-ca-in.")
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
	if *policySource != policySourceHardcoded {
		return fmt.Errorf("--policy-source must be %q (got %q); there is no default, because a gateway enforcing a demo table should say so in its Deployment",
			policySourceHardcoded, *policySource)
	}

	// A real source starts this Store empty and fills it from a refresher, with
	// readiness gated on the first successful load. The hardcoded table is
	// published up front, so /healthz is green immediately.
	snap := extproc.HardcodedSnapshot()
	store := extproc.NewStore(snap)
	stats := extproc.NewStats()

	slog.Info("Policy table loaded",
		slog.String("source", *policySource),
		slog.Int("rev", snap.Rev),
		slog.Int("actors", snap.Len()))

	connectLis, err := net.Listen("tcp", *connectListen)
	if err != nil {
		return fmt.Errorf("listening for the connect checkpoint: %w", err)
	}
	adminLis, err := net.Listen("tcp", *adminListen)
	if err != nil {
		return fmt.Errorf("listening for admin: %w", err)
	}

	// Opened before the servers start so that a bad --inner-listen fails here
	// rather than after Envoy has begun sending traffic to a half-up gateway.
	var innerLis net.Listener
	if *innerListen != "" {
		innerLis, err = net.Listen("tcp", *innerListen)
		if err != nil {
			return fmt.Errorf("listening for the inner checkpoint: %w", err)
		}
	}

	var connectOpts []extproc.ServerOption
	if innerLis != nil {
		connectOpts = append(connectOpts, extproc.WithInnerCheckpoint())
	} else {
		slog.Warn("Inner checkpoint disabled: ALLOW_BY_HOSTNAME and BASIC_CREDENTIAL_INJECT will be denied at CONNECT")
	}

	admin := &http.Server{
		Handler:           extproc.AdminHandler(store, stats),
		ReadHeaderTimeout: 5 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)

	if *clientCAOut != "" {
		bundler := &extproc.ClientCABundler{
			Inputs:   *clientCAIn,
			Output:   *clientCAOut,
			Interval: *clientCAEvery,
		}
		// Written synchronously, before anything else runs: Envoy's front door
		// cannot load its validation context until this file exists, and a
		// missing input should stop the pod here with a specific error.
		if err := bundler.RunOnce(); err != nil {
			return err
		}
		g.Go(func() error { return bundler.Run(gctx) })
	} else if len(*clientCAIn) > 0 {
		return fmt.Errorf("--client-ca-in was given without --client-ca-out")
	}

	g.Go(func() error {
		slog.Info("Serving CONNECT checkpoint", slog.String("addr", connectLis.Addr().String()))
		return extproc.NewServer(extproc.CheckpointConnect, store, stats, connectOpts...).Serve(gctx, connectLis)
	})
	if innerLis != nil {
		g.Go(func() error {
			slog.Info("Serving inner checkpoint", slog.String("addr", innerLis.Addr().String()))
			return extproc.NewServer(extproc.CheckpointInner, store, stats).Serve(gctx, innerLis)
		})
	}
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
