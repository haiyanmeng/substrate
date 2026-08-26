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

package router

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	secretgrpc "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
)

// credentialBundleSecret describes a projected podCertificate credential
// bundle as an SDS secret.
//
// The watch is the whole point of routing a certificate through SDS. Envoy
// honors TlsCertificate.watched_directory only for a TlsCertificate delivered
// by SDS; on a static bootstrap tls_certificates entry the field is accepted
// and silently ignored, and the files behind it are read once at config load.
// A gateway configured that way keeps serving the leaf it started with after
// kubelet rotates the bundle underneath it, until that leaf expires and every
// handshake fails.
//
// See https://pkg.go.dev/github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3#:~:text=This%20only%20applies%20when%20a%20%E2%80%9CTlsCertificate%E2%80%9C%20is%20delivered%20by%20SDS
func credentialBundleSecret(name, certPath string) *tlsv3.Secret {
	return &tlsv3.Secret{
		Name: name,
		Type: &tlsv3.Secret_TlsCertificate{
			TlsCertificate: &tlsv3.TlsCertificate{
				// The pod certificate is projected as a single PEM bundle
				// holding both the cert chain and the private key, so both
				// DataSources point at the same file.
				CertificateChain: &corev3.DataSource{
					Specifier: &corev3.DataSource_Filename{
						Filename: certPath,
					},
				},
				PrivateKey: &corev3.DataSource{
					Specifier: &corev3.DataSource_Filename{
						Filename: certPath,
					},
				},
				WatchedDirectory: &corev3.WatchedDirectory{
					Path: filepath.Dir(certPath),
				},
			},
		},
	}
}

// GatewayCertSecretName is the SDS secret carrying a statically configured
// dataplane's serving certificate. The gateway's Envoy names it in
// tls_certificate_sds_secret_configs, so it must match the egress manifests.
const GatewayCertSecretName = "gateway_serving_cert"

// sdsNodeKey is the single snapshot key SdsServer serves under. The gateway's
// Envoy is a fixed sidecar in the same pod rather than one of many nodes, so
// its --service-node value carries no information here and sdsNodeHash maps
// every node to this key.
const sdsNodeKey = "gateway"

type sdsNodeHash struct{}

func (sdsNodeHash) ID(*corev3.Node) string { return sdsNodeKey }

// sdsSnapshotVersion never changes. The secret names files on disk and Envoy
// re-reads them itself when the watched directory changes, so a rotation
// produces no new resource to publish.
const sdsSnapshotVersion = "1"

// SdsServer delivers one secret — a statically configured dataplane's serving
// certificate — over SDS, which is what makes that certificate rotate at all;
// see credentialBundleSecret for why naming the file in the bootstrap does
// not.
//
// The egress gateway is the user. It takes its whole configuration from a
// bootstrap file and runs no xDS control plane, so this is deliberately not
// the ingress XdsServer: no listeners, clusters, or routes, and nothing that
// needs Kubernetes access.
type SdsServer struct {
	certPath string
	snapshot cachev3.SnapshotCache
	srv      serverv3.Server
}

func NewSdsServer(certPath string) *SdsServer {
	// Not an ADS cache: the gateway reaches this over a plain SDS
	// api_config_source, one resource type on its own stream.
	cache := cachev3.NewSnapshotCache(false, sdsNodeHash{}, nil)
	return &SdsServer{
		certPath: certPath,
		snapshot: cache,
		srv:      serverv3.NewServer(context.Background(), cache, nil),
	}
}

func (s *SdsServer) setSnapshot(ctx context.Context) error {
	snapshot, err := cachev3.NewSnapshot(sdsSnapshotVersion, map[resourcev3.Type][]types.Resource{
		resourcev3.SecretType: {credentialBundleSecret(GatewayCertSecretName, s.certPath)},
	})
	if err != nil {
		return fmt.Errorf("failed to build SDS snapshot: %w", err)
	}
	return s.snapshot.SetSnapshot(ctx, sdsNodeKey, snapshot)
}

func (s *SdsServer) Serve(ctx context.Context, lis net.Listener) error {
	// Published before the first request can arrive, so the gateway is never
	// answered from an empty cache and left holding an uninitialized listener.
	// Unlike the ingress snapshot this one is complete from the start, so a
	// failure here is fatal rather than something a later update recovers.
	if err := s.setSnapshot(ctx); err != nil {
		return err
	}

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)
	secretgrpc.RegisterSecretDiscoveryServiceServer(grpcServer, s.srv)

	errChan := make(chan error, 1)
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		// Hard stop, for the same reason XdsServer.Serve takes one: the SDS
		// stream is open-ended, so GracefulStop would block until Envoy
		// disconnects — which during shutdown it only does by dying. Envoy
		// keeps the last delivered secret across a control-plane disconnect,
		// and this context is cancelled only after the gateway has drained.
		grpcServer.Stop()
		return nil
	case err := <-errChan:
		return err
	}
}
