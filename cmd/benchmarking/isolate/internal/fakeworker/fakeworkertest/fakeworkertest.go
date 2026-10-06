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

// Package fakeworkertest issues pod-identity-shaped certificates for tests of
// the fake data plane's capacity relay: a throwaway CA, and leaf certificates
// that carry a SPIFFE ID as their only SAN, as the pod-identity signer's do.
package fakeworkertest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a throwaway signer whose files live in a test's temp dir.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
	// TrustBundle is the path of the CA certificate as a PEM trust bundle.
	TrustBundle string
}

// NewCA creates a CA and writes its trust bundle.
func NewCA(t *testing.T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fakeworkertest CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ca := &CA{cert: cert, key: key, dir: dir, TrustBundle: filepath.Join(dir, "trust-bundle.pem")}
	writePEM(t, ca.TrustBundle, pem.Block{Type: "CERTIFICATE", Bytes: der})
	return ca
}

// Issue writes a credential bundle (leaf certificate, then its PKCS #8 key)
// for spiffeID and returns its path.
func (ca *CA) Issue(t *testing.T, spiffeID string) string {
	t.Helper()
	id, err := url.Parse(spiffeID)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{id},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.dir, serial.String()+"-bundle.pem")
	writePEM(t, path, pem.Block{Type: "CERTIFICATE", Bytes: der}, pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	return path
}

func writePEM(t *testing.T, path string, blocks ...pem.Block) {
	t.Helper()
	var out []byte
	for _, b := range blocks {
		out = append(out, pem.EncodeToMemory(&b)...)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
