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

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/cmd/benchmarking/isolate/internal/fakeworker"
	"github.com/agent-substrate/substrate/internal/credbundle"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// reportTimeout bounds one capacity report, relay hop included.
const reportTimeout = 10 * time.Second

// grpcRelay sends capacity reports to the fake-atelet relay on a Worker's
// node, as ateom sends them to the atelet on its own node. A rejected report
// carries ate-api-server's status unchanged.
type grpcRelay struct {
	creds credentials.TransportCredentials

	mu sync.Mutex
	// conns holds one connection per relay address. There is one relay per
	// fake node, so the map stays small; a connection to a relay that is gone
	// goes idle and holds nothing open.
	conns map[string]*grpc.ClientConn
}

// newGRPCRelay presents this pod's certificate and accepts only a server
// whose certificate chains to the pod-identity trust bundle and carries
// atelet's SPIFFE ID. The server is dialed by pod IP and its certificate has
// no DNS or IP SAN, so the chain is verified here rather than by hostname.
func newGRPCRelay(clientBundlePath, trustBundlePath, ateletID string) (*grpcRelay, error) {
	caBytes, err := os.ReadFile(trustBundlePath)
	if err != nil {
		return nil, fmt.Errorf("read trust bundle %s: %w", trustBundlePath, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("parse trust bundle %s", trustBundlePath)
	}
	cfg := &tls.Config{
		MinVersion:           tls.VersionTLS13,
		GetClientCertificate: credbundle.ClientLoader(clientBundlePath),
		InsecureSkipVerify:   true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("relay presented no certificate")
			}
			intermediates := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				intermediates.AddCert(c)
			}
			if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
			}); err != nil {
				return fmt.Errorf("verifying relay certificate: %w", err)
			}
			return fakeworker.VerifyPeerID(cs, ateletID)
		},
	}
	return &grpcRelay{creds: credentials.NewTLS(cfg), conns: map[string]*grpc.ClientConn{}}, nil
}

// Report sends req to the relay at addr (host:port).
func (g *grpcRelay) Report(ctx context.Context, addr string, req *ateapipb.SetWorkerCapacityRequest) error {
	conn, err := g.conn(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	_, err = ateapipb.NewWorkerServiceClient(conn).SetWorkerCapacity(ctx, req)
	return err
}

func (g *grpcRelay) conn(addr string) (*grpc.ClientConn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.conns[addr]; ok {
		return c, nil
	}
	c, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(g.creds))
	if err != nil {
		return nil, fmt.Errorf("dialing relay %s: %w", addr, err)
	}
	g.conns[addr] = c
	return c, nil
}

// Close closes every relay connection.
func (g *grpcRelay) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for addr, c := range g.conns {
		_ = c.Close()
		delete(g.conns, addr)
	}
}
