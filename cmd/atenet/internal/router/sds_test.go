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
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discoverygrpc "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretgrpc "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

const testGatewayCertPath = "/run/servicedns.podcert.ate.dev/credential-bundle.pem"

// serveTestSdsServer starts an SdsServer on a loopback port and returns an SDS
// client for it.
func serveTestSdsServer(t *testing.T, certPath string) secretgrpc.SecretDiscoveryServiceClient {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create tcp listener: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serveErr := make(chan error, 1)
	go func() { serveErr <- NewSdsServer(certPath).Serve(ctx, lis) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveErr:
			if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
				t.Errorf("Serve error returned: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Timeout exceeded waiting for Serve to return after cancellation")
		}
	})

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("Failed to dial SDS server: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return secretgrpc.NewSecretDiscoveryServiceClient(conn)
}

// fetchGatewaySecret asks for the gateway serving certificate as the node name
// Envoy would send, and returns the one secret it gets back.
func fetchGatewaySecret(t *testing.T, client secretgrpc.SecretDiscoveryServiceClient, nodeID string) *tlsv3.Secret {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	stream, err := client.StreamSecrets(ctx)
	if err != nil {
		t.Fatalf("Failed to open SDS stream: %v", err)
	}
	if err := stream.Send(&discoverygrpc.DiscoveryRequest{
		Node:          &corev3.Node{Id: nodeID},
		TypeUrl:       resourcev3.SecretType,
		ResourceNames: []string{GatewayCertSecretName},
	}); err != nil {
		t.Fatalf("Failed to send SDS discovery request: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Failed to receive SDS discovery response: %v", err)
	}

	resources := resp.GetResources()
	if len(resources) != 1 {
		t.Fatalf("Expected 1 secret resource over SDS, got %d", len(resources))
	}
	secret := &tlsv3.Secret{}
	if err := resources[0].UnmarshalTo(secret); err != nil {
		t.Fatalf("Failed to unmarshal SDS resource into Secret: %v", err)
	}
	return secret
}

// TestSdsServer_ServesGatewayCert fetches the gateway serving certificate over
// a real SDS stream, as the statically configured egress Envoy does. The
// watched directory is the assertion that matters: it is inert on the
// bootstrap tls_certificates entry this replaces, and only takes effect
// because the certificate arrives this way.
func TestSdsServer_ServesGatewayCert(t *testing.T) {
	secret := fetchGatewaySecret(t, serveTestSdsServer(t, testGatewayCertPath), "atenet-egress")

	if secret.GetName() != GatewayCertSecretName {
		t.Errorf("Expected secret name %q, got %q", GatewayCertSecretName, secret.GetName())
	}
	tlsCert := secret.GetTlsCertificate()
	if got := tlsCert.GetCertificateChain().GetFilename(); got != testGatewayCertPath {
		t.Errorf("Expected certificate chain filename %q, got %q", testGatewayCertPath, got)
	}
	if got := tlsCert.GetPrivateKey().GetFilename(); got != testGatewayCertPath {
		t.Errorf("Expected private key filename %q, got %q", testGatewayCertPath, got)
	}
	if got, want := tlsCert.GetWatchedDirectory().GetPath(), filepath.Dir(testGatewayCertPath); got != want {
		t.Errorf("Expected watched directory %q, got %q", want, got)
	}
}

// TestSdsServer_ServesAnyNodeID pins the node-independence the gateway relies
// on: its Envoy identifies itself by whatever --service-node the manifest
// passes, and nothing sets that to match a key here. A cache keyed on the node
// ID would answer with an empty response instead, which Envoy reads as "the
// secret does not exist" and the :443 listener never initializes.
func TestSdsServer_ServesAnyNodeID(t *testing.T) {
	client := serveTestSdsServer(t, testGatewayCertPath)

	for _, nodeID := range []string{"atenet-egress", NodeID, ""} {
		t.Run("node="+nodeID, func(t *testing.T) {
			if got := fetchGatewaySecret(t, client, nodeID).GetName(); got != GatewayCertSecretName {
				t.Errorf("Expected secret name %q for node %q, got %q", GatewayCertSecretName, nodeID, got)
			}
		})
	}
}

// TestSdsServer_ProjectedVolumeRotation checks the reload contract the secret
// relies on end to end: after a kubelet podCertificate rotation the filename
// in the secret resolves to the new bundle, and the swap happens on a symlink
// directly inside the watched directory — the move Envoy reloads on. Envoy's
// own reload is out of unit-test reach and belongs to e2e.
func TestSdsServer_ProjectedVolumeRotation(t *testing.T) {
	dir := t.TempDir()
	const certA, certB = "gateway-cert-a", "gateway-cert-b"
	certPath := filepath.Join(dir, "credential-bundle.pem")

	const tsDirA = "..2026_08_26_01_00_00.0000000001"
	const tsDirB = "..2026_08_26_01_49_49.0000000002"
	writeProjectedVolume(t, dir, tsDirA, makeServingBundle(t, certA))

	tlsCert := fetchGatewaySecret(t, serveTestSdsServer(t, certPath), "atenet-egress").GetTlsCertificate()

	chainPath := tlsCert.GetCertificateChain().GetFilename()
	if got := readServingCN(t, chainPath); got != certA {
		t.Fatalf("Expected initial bundle to serve %q, got %q", certA, got)
	}

	rotateProjectedVolume(t, dir, tsDirB, tsDirA, makeServingBundle(t, certB))

	if got := readServingCN(t, chainPath); got != certB {
		t.Errorf("Expected rotated bundle to serve %q, got %q", certB, got)
	}
}

// TestValidate_SdsPortWithoutCertPath rejects the combination that would leave
// the gateway waiting forever on a secret naming no file.
func TestValidate_SdsPortWithoutCertPath(t *testing.T) {
	cfg := routerConfig{Mode: ModeEgress, SdsPort: 50052}
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate() = nil, want an error for --port-sds without --envoy-cert-path")
	}
	if !strings.Contains(err.Error(), "--envoy-cert-path") {
		t.Errorf("validate() error = %q, want it to name --envoy-cert-path", err)
	}

	cfg.EnvoyCertPath = testGatewayCertPath
	if err := cfg.validate(); err != nil {
		t.Errorf("validate() = %v, want nil once --envoy-cert-path is set", err)
	}
}
