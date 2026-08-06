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

package e2e

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agent-substrate/substrate/internal/localca"
)

const (
	// Where the egress gateway and its MITM CA live. Both are fixed by
	// manifests/ate-install/atenet-egress.yaml and hack/install-ate.sh.
	EgressNamespace = "ate-system"
	// MITMCASecret holds the CA sdsmintd signs intercepted leaves under. It is
	// mounted on the sdsmintd container and nowhere else.
	MITMCASecret    = "egress-mitm-ca-pool"
	mitmCASecretKey = "pool"
	mitmCAID        = "mitm"
)

// MITMRootCertificate reads the trust anchor sdsmintd signs under, straight
// from the secret the sidecar mounts, so a test checks the chain against the CA
// that is actually deployed rather than one it was told about.
//
// This root is the sharpest evidence a test can get that traffic crossed the
// egress gateway: nothing else in the cluster can produce a chain that ends
// here, so a leaf that verifies against it could not have come from the
// destination the client thought it was talking to.
func MITMRootCertificate(t *testing.T, ctx context.Context) *x509.Certificate {
	t.Helper()
	secret, err := GetClients().K8s.CoreV1().Secrets(EgressNamespace).Get(ctx, MITMCASecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading MITM CA pool secret %s/%s: %v", EgressNamespace, MITMCASecret, err)
	}
	// This unmarshals the signing key along with the certificate. That is
	// unavoidable -- the pool is one blob -- and acceptable only because this
	// runs against a test cluster with a kubeconfig that could read the secret
	// anyway. Nothing below touches the key.
	pool, err := localca.Unmarshal(secret.Data[mitmCASecretKey])
	if err != nil {
		t.Fatalf("parsing MITM CA pool from %s/%s key %q: %v", EgressNamespace, MITMCASecret, mitmCASecretKey, err)
	}
	for _, ca := range pool.CAs {
		if ca.ID == mitmCAID {
			return ca.RootCertificate
		}
	}
	t.Fatalf("MITM CA pool %s/%s has no CA with id %q", EgressNamespace, MITMCASecret, mitmCAID)
	return nil
}

// ParseCertChain decodes a leaf-first PEM chain as served on a handshake. label
// names what the chain was served for and appears in failures.
func ParseCertChain(t *testing.T, label, chainPEM string) []*x509.Certificate {
	t.Helper()
	var chain []*x509.Certificate
	rest := []byte(chainPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parsing certificate served for %q: %v", label, err)
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		t.Fatalf("no certificates in the chain served for %q", label)
	}
	return chain
}

// CertPool collects certs into a pool, for the Roots and Intermediates of an
// x509.VerifyOptions built out of a served chain.
func CertPool(certs ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}
