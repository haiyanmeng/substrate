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

package sdsmint

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func testCA(t *testing.T) *CA {
	t.Helper()
	ca, _, _, err := GenerateCA("sdsmint test CA", time.Hour, nil)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return ca
}

// parseChain splits a PEM chain into leaf and intermediates/root.
func parseChain(t *testing.T, chainPEM []byte) []*x509.Certificate {
	t.Helper()
	var certs []*x509.Certificate
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parsing chain: %v", err)
		}
		certs = append(certs, c)
	}
	return certs
}

func TestSignProducesUsableLeaf(t *testing.T) {
	ca := testCA(t)

	minted, err := ca.Sign("foo.example", 5*time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	chain := parseChain(t, minted.CertChainPEM)
	if len(chain) != 2 {
		t.Fatalf("chain length = %d, want 2 (leaf + CA)", len(chain))
	}
	leaf, root := chain[0], chain[1]

	if got := leaf.DNSNames; len(got) != 1 || got[0] != "foo.example" {
		t.Errorf("leaf DNSNames = %v, want [foo.example]", got)
	}
	if leaf.Subject.CommonName != "foo.example" {
		t.Errorf("leaf CN = %q, want foo.example", leaf.Subject.CommonName)
	}
	if leaf.IsCA {
		t.Error("leaf is marked as a CA")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("leaf KeyUsage = %v, want DigitalSignature only", leaf.KeyUsage)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("leaf EKU = %v, want [ServerAuth]", leaf.ExtKeyUsage)
	}
	if !root.Equal(ca.Certificate()) {
		t.Error("chain does not end in the CA certificate")
	}

	// The whole point is that a normal TLS client accepts this, so verify the
	// way one would.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:   "foo.example",
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not verify against the CA: %v", err)
	}

	// And the key must actually match the certificate.
	if _, err := tls.X509KeyPair(minted.CertChainPEM, minted.PrivateKeyPEM); err != nil {
		t.Errorf("minted chain/key is not a usable TLS keypair: %v", err)
	}
}

func TestSignIsUniquePerCall(t *testing.T) {
	ca := testCA(t)

	a, err := ca.Sign("a.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign a: %v", err)
	}
	b, err := ca.Sign("a.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign a again: %v", err)
	}

	if a.Serial == b.Serial {
		t.Error("two mints for the same host reused a serial number")
	}
	if string(a.PrivateKeyPEM) == string(b.PrivateKeyPEM) {
		t.Error("two mints for the same host reused a private key; each leaf must get a fresh keypair")
	}
}

func TestSignHonoursTTL(t *testing.T) {
	ca := testCA(t)
	before := time.Now()

	minted, err := ca.Sign("ttl.example", 90*time.Second)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	want := before.Add(90 * time.Second)
	if delta := minted.NotAfter.Sub(want); delta > 5*time.Second || delta < -5*time.Second {
		t.Errorf("NotAfter = %v, want within 5s of %v", minted.NotAfter, want)
	}
}

func TestSignIPLiteralGoesInSANIPAddresses(t *testing.T) {
	ca := testCA(t)

	minted, err := ca.Sign("10.1.2.3", time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	leaf := parseChain(t, minted.CertChainPEM)[0]

	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.1.2.3" {
		t.Errorf("leaf IPAddresses = %v, want [10.1.2.3]", leaf.IPAddresses)
	}
	if len(leaf.DNSNames) != 0 {
		t.Errorf("leaf DNSNames = %v, want empty for an IP literal", leaf.DNSNames)
	}
}

func TestSignRejectsEmptyHost(t *testing.T) {
	if _, err := testCA(t).Sign("", time.Minute); err == nil {
		t.Fatal("Sign(\"\") succeeded, want an error")
	}
}

func TestGenerateCARoundTripsThroughLoadCA(t *testing.T) {
	_, certPEM, keyPEM, err := GenerateCA("roundtrip CA", time.Hour, nil)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	loaded, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if _, err := loaded.Sign("after.reload", time.Minute); err != nil {
		t.Fatalf("Sign after reload: %v", err)
	}
}

func TestLoadCARejectsNonCACertificate(t *testing.T) {
	ca := testCA(t)
	// A leaf is not a CA; loading one must fail rather than silently produce
	// a signer that emits certificates nothing will chain.
	minted, err := ca.Sign("leaf.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: parseChain(t, minted.CertChainPEM)[0].Raw,
	})

	if _, err := LoadCA(leafPEM, minted.PrivateKeyPEM); err == nil {
		t.Fatal("LoadCA accepted a non-CA certificate")
	}
}

func TestLoadCARejectsGarbage(t *testing.T) {
	for name, tc := range map[string]struct{ cert, key []byte }{
		"empty":       {nil, nil},
		"not pem":     {[]byte("hello"), []byte("world")},
		"cert as key": {[]byte("-----BEGIN CERTIFICATE-----\nzz\n-----END CERTIFICATE-----\n"), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCA(tc.cert, tc.key); err == nil {
				t.Fatal("LoadCA succeeded, want an error")
			}
		})
	}
}

func TestGenerateCANameConstraints(t *testing.T) {
	ca, _, _, err := GenerateCA("constrained CA", time.Hour, []string{"example"})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate())

	inside, err := ca.Sign("ok.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign inside constraint: %v", err)
	}
	if _, err := parseChain(t, inside.CertChainPEM)[0].Verify(x509.VerifyOptions{
		DNSName: "ok.example", Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("in-constraint leaf failed to verify: %v", err)
	}

	// The name constraint is the backstop for a CA-key compromise: even with
	// the key, the holder cannot mint a leaf outside the permitted domain
	// that clients will accept.
	outside, err := ca.Sign("evil.test", time.Minute)
	if err != nil {
		t.Fatalf("Sign outside constraint: %v", err)
	}
	if _, err := parseChain(t, outside.CertChainPEM)[0].Verify(x509.VerifyOptions{
		DNSName: "evil.test", Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("leaf outside the CA name constraint verified; the constraint is not being enforced")
	}
}
