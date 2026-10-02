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

// Command fake-atelet stands in for atelet in control-plane benchmarks. It
// runs as atelet's DaemonSet would, on the benchmark nodes only, so
// ate-api-server dials it as the node's atelet. It answers every AteomHerder
// call with success after a delay, without running or saving any workload,
// and reports capacity for the fake Workers fake-workersync registers on its
// node.
//
// Actors it "runs" do not exist. Never run it on a cluster that serves real
// actors. See benchmarking/workloads/deploy.sh --fake-data-plane.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/agent-substrate/substrate/internal/ateapiauth"
	"github.com/agent-substrate/substrate/internal/atelet"
	"github.com/agent-substrate/substrate/internal/benchmarking/fakeworker"
	"github.com/agent-substrate/substrate/internal/credbundle"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/serverboot"
	"github.com/agent-substrate/substrate/internal/version"
	"github.com/agent-substrate/substrate/pkg/objectstorage"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	port             = pflag.Int("port", atelet.DefaultPort, "Port to serve AteomHerder on. ate-api-server dials atelet.DefaultPort.")
	healthListenAddr = pflag.String("health-listen-addr", ":9090", "Address to serve /healthz and /readyz on.")

	grpcServerCredBundle = pflag.String("grpc-server-cred-bundle", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "Credential bundle presented as the gRPC serving certificate and the ate-api-server client certificate.")
	clientCACerts        = pflag.String("client-ca-certs", "/run/podidentity.podcert.ate.dev/trust-bundle.pem", "CA bundle used to verify gRPC client certificates.")
	ateapiAddress        = pflag.String("ateapi-address", "dns:///api.ate-system.svc:443", "ate-api-server gRPC target for capacity reports.")
	ateapiCAFile         = pflag.String("ateapi-ca-file", "/run/servicedns.podcert.ate.dev/trust-bundle.pem", "CA bundle used to verify ate-api-server.")
	ateapiServerName     = pflag.String("ateapi-server-name", "api.ate-system.svc", "DNS name expected on the ate-api-server certificate.")

	nodeName       = pflag.String("node-name", os.Getenv("NODE_NAME"), "This node's name. Defaults to $NODE_NAME, set from the downward API.")
	run            = pflag.String("run", "", "Run prefix shared with fake-workersync. Required.")
	workersPerNode = pflag.Int("workers-per-node", 0, "Fake Workers fake-workersync registers on each node. Required.")

	capacityActors       = pflag.Int("worker-capacity-actors", 0, "Actors each fake Worker can hold. Required.")
	capacityResources    = pflag.String("worker-capacity-resources", "", `Resources each fake Worker reports, as "cpu=32,memory=128Gi". Must cover every resource the templates request: the scheduler reads an unreported one as zero.`)
	capacityRetryTimeout = pflag.Duration("capacity-retry-timeout", 5*time.Minute, "How long to keep retrying capacity reports while fake-workersync creates the Workers.")

	delay                       = pflag.Duration("delay", 0, "How long each AteomHerder call takes before it succeeds.")
	delayRun                    = pflag.Duration("delay-run", -1, "Override --delay for Run. Negative uses --delay.")
	delayRestore                = pflag.Duration("delay-restore", -1, "Override --delay for Restore. Negative uses --delay.")
	delayCheckpoint             = pflag.Duration("delay-checkpoint", -1, "Override --delay for Checkpoint. Negative uses --delay.")
	delayUploadPausedCheckpoint = pflag.Duration("delay-upload-paused-checkpoint", -1, "Override --delay for UploadPausedCheckpoint. Negative uses --delay.")
	delayTerminate              = pflag.Duration("delay-terminate", -1, "Override --delay for Terminate. Negative uses --delay.")

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

	if *nodeName == "" {
		serverboot.Fatal(ctx, "Invalid flags", errors.New("--node-name or $NODE_NAME is required"))
	}
	if err := fakeworker.ValidateRun(*run); err != nil {
		serverboot.Fatal(ctx, "Invalid --run", err)
	}
	if err := fakeworker.ValidateWorkersPerNode(*workersPerNode); err != nil {
		serverboot.Fatal(ctx, "Invalid --workers-per-node", err)
	}
	capacity, err := parseCapacity(*capacityActors, *capacityResources)
	if err != nil {
		serverboot.Fatal(ctx, "Invalid worker capacity", err)
	}
	callDelays, err := resolveDelays(*delay, *delayRun, *delayRestore, *delayCheckpoint, *delayUploadPausedCheckpoint, *delayTerminate)
	if err != nil {
		serverboot.Fatal(ctx, "Invalid --delay", err)
	}

	storage, err := newObjectStorage(ctx)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to create the object storage client", err)
	}

	// No Kubernetes client: fake-atelet never calls the Kubernetes API, so the
	// ate-api-server target is resolved through DNS rather than EndpointSlices.
	dialOpts, err := ateapiauth.DialOptions(ateapiauth.ClientConfig{
		CAFile:           *ateapiCAFile,
		ServerName:       *ateapiServerName,
		ClientCredBundle: *grpcServerCredBundle,
	})
	if err != nil {
		serverboot.Fatal(ctx, "Failed to build ate-api-server client credentials", err)
	}
	ateapiConn, err := grpc.NewClient(*ateapiAddress, dialOpts...)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to create ate-api-server client", err)
	}
	defer ateapiConn.Close()

	tlsCfg, err := serverTLSConfig(*grpcServerCredBundle, *clientCACerts)
	if err != nil {
		serverboot.Fatal(ctx, "Failed to build server TLS config", err)
	}
	lis, err := net.Listen("tcp", ":"+strconv.Itoa(*port))
	if err != nil {
		serverboot.Fatal(ctx, "Failed to listen", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	ateletpb.RegisterAteomHerderServer(srv, &herder{delays: callDelays, storage: storage})

	reporter := &capacityReporter{
		client:         ateapipb.NewWorkerServiceClient(ateapiConn),
		workers:        fakeworker.Names(*run, *nodeName, *workersPerNode),
		capacity:       capacity,
		timeout:        *capacityRetryTimeout,
		initialBackoff: 500 * time.Millisecond,
		maxBackoff:     10 * time.Second,
	}
	health := &http.Server{Addr: *healthListenAddr, Handler: healthHandler(&reporter.ready), ReadHeaderTimeout: 10 * time.Second}

	slog.WarnContext(ctx, "Serving the fake atelet: actor lifecycle calls succeed without running any workload",
		slog.String("node", *nodeName), slog.Int("workers", *workersPerNode), slog.Any("delays", fmt.Sprintf("%+v", callDelays)))

	go func() {
		if err := srv.Serve(lis); err != nil {
			serverboot.Fatal(ctx, "Failed to serve AteomHerder", err)
		}
	}()
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverboot.Fatal(ctx, "Failed to serve health endpoints", err)
		}
	}()
	go func() {
		// A failed report leaves the pod unready, which fails the deploy's
		// rollout wait; the pod keeps running so its logs stay readable.
		if err := reporter.run(ctx); err != nil {
			slog.ErrorContext(ctx, "Failed to report capacity; staying unready", slog.Any("err", err))
		}
	}()

	<-ctx.Done()
	slog.InfoContext(context.Background(), "Shutting down")
	srv.GracefulStop()
	_ = health.Shutdown(context.Background())
}

// healthHandler serves /healthz, always 200 while the process runs, and
// /readyz, 200 once every fake Worker on this node has its capacity reported.
func healthHandler(ready interface{ Load() bool }) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "capacity not reported yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// serverTLSConfig mirrors atelet's: the pod-identity certificate as the
// serving certificate, and client certificates required and verified.
func serverTLSConfig(servingBundlePath, clientCAPath string) (*tls.Config, error) {
	caBytes, err := os.ReadFile(clientCAPath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %s: %w", clientCAPath, err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("parse CA bundle from %s", clientCAPath)
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: credbundle.Loader(servingBundlePath),
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      clientCAs,
	}, nil
}

// newObjectStorage picks the backend the way atelet does, from
// ATE_STORAGE_BACKEND, so placeholders land where atelet would write.
func newObjectStorage(ctx context.Context) (objectstorage.ObjectStorage, error) {
	switch os.Getenv("ATE_STORAGE_BACKEND") {
	case "s3":
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("loading S3 config: %w", err)
		}
		return objectstorage.NewS3Client(s3.NewFromConfig(cfg, func(o *s3.Options) {
			if os.Getenv("AWS_S3_USE_PATH_STYLE") == "true" {
				o.UsePathStyle = true
			}
		})), nil
	default:
		return objectstorage.NewGCSClient(ctx)
	}
}
