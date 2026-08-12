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

// Command extprocd is the Envoy external processor for the egress gateway's
// inner checkpoint — the decrypted leg of the MITM, where the destination
// hostname is finally visible. See cmd/extprocd/internal/inner for what that
// checkpoint can and cannot see, and
// docs/dev/egress-pluggable-extproc-contract.md for the wire contract.
//
// It is a separate Deployment rather than another sidecar in the gateway pod.
// The CONNECT checkpoint's processor is a sidecar because it is called once per
// tunnel and needs the ate API; this one is called once per HTTP request on
// every tunnel, so its load tracks request rate rather than actor count and it
// has to be able to scale on its own.
//
// The gateway calls it fail-closed. An extprocd that is unreachable does not
// degrade MITM'd egress, it stops it — see the availability note in the
// manifest.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/cmd/extprocd/internal/inner"
	"github.com/agent-substrate/substrate/internal/version"
)

// shutdownGrace is how long a signaled server waits for in-flight streams. Each
// stream is one request's headers, so this is short on purpose: anything still
// open past it is a request whose gateway-side deadline has expired anyway.
const shutdownGrace = 5 * time.Second

var (
	addr = pflag.String("addr", ":50051", "TCP address to serve the ext_proc gRPC service on")

	// StringArray rather than StringSlice: a hostname cannot contain a comma,
	// so splitting on one can only turn a typo into two silently-accepted
	// entries.
	denyHosts = pflag.StringArray("deny-host", nil,
		"hostname that MITM'd egress may not reach, matched case-insensitively and ignoring the port; repeatable, and allow-all when unset")

	logLevel    = pflag.String("log-level", "info", "one of debug, info, warn, error")
	showVersion = pflag.Bool("version", false, "Print version and exit.")
)

func main() {
	pflag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}
	if err := run(context.Background()); err != nil {
		slog.Error("extprocd failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	logger, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	policy, err := buildPolicy()
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *addr, err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(grpcServer, inner.NewServer(policy, inner.GitHubToken(), logger))

	logger.Info("extprocd listening",
		slog.String("address", lis.Addr().String()),
		// Logged as the effective list, so the one line an operator reads to
		// see what this processor refuses cannot be mistaken for "nothing".
		slog.Any("deny_hosts", *denyHosts),
		// Whether a token is compiled in. Without this line, "the constant is
		// empty" and "the request did not match the GitHub hosts" are the same
		// observation from outside: no Authorization header, and a 401.
		slog.Bool("github_token", inner.GitHubTokenConfigured()),
	)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
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

// buildPolicy turns the flags into the one Policy this process serves.
func buildPolicy() (inner.Policy, error) {
	if len(*denyHosts) == 0 {
		return inner.AllowAll(), nil
	}
	policy, err := inner.DenyHosts(*denyHosts)
	if err != nil {
		return nil, fmt.Errorf("--deny-host: %w", err)
	}
	return policy, nil
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}
