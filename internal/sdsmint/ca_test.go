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
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/localca"
)

// testCA is name-constrained to "example", which is the configuration NewCA
// accepts without being argued with, and which every host used below sits
// under. Tests that need the unconstrained case ask for it explicitly.
func testCA(t *testing.T) *CA {
	t.Helper()
	ca, _, err := GenerateCA("sdsmint test CA", time.Hour, []string{"example"}, Options{})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return ca
}

// entryPEM renders a generated pool entry as the cert/key PEM pair LoadCA
// takes, so the PEM path can still be exercised now that generation produces a
// pool entry rather than PEM.
func entryPEM(t *testing.T, entry *localca.CA) (certPEM, keyPEM []byte) {
	t.Helper()
	keyDER, err := x509.MarshalPKCS8PrivateKey(entry.SigningKey)
	if err != nil {
		t.Fatalf("marshalling CA key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: entry.RootCertificate.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
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
	_, entry, err := GenerateCA("roundtrip CA", time.Hour, []string{"example"}, Options{})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certPEM, keyPEM := entryPEM(t, entry)

	loaded, err := LoadCA(certPEM, keyPEM, Options{})
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if _, err := loaded.Sign("after.reload.example", time.Minute); err != nil {
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

	if _, err := LoadCA(leafPEM, minted.PrivateKeyPEM, Options{AllowUnconstrained: true}); err == nil {
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
			if _, err := LoadCA(tc.cert, tc.key, Options{AllowUnconstrained: true}); err == nil {
				t.Fatal("LoadCA succeeded, want an error")
			}
		})
	}
}

func TestGenerateCANameConstraints(t *testing.T) {
	ca, _, err := GenerateCA("constrained CA", time.Hour, []string{"example"}, Options{})
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

// --------------------------------------------------------- name constraints

func TestNewCARefusesAnUnconstrainedCAByDefault(t *testing.T) {
	entry, err := GenerateRoot(GenerateRootOptions{CommonName: "wide open", Lifetime: time.Hour})
	if err != nil {
		t.Fatalf("GenerateRoot: %v", err)
	}

	// The gap this closes: before, a CA supplied without name constraints was
	// accepted silently, and --ca-name-constraint was consulted only when
	// generating. An operator who brought their own CA got no constraint and
	// no warning that they had none.
	_, err = NewCA(entry.RootCertificate, entry.SigningKey, Options{})
	if err == nil {
		t.Fatal("NewCA accepted a CA with no name constraint")
	}
	if !strings.Contains(err.Error(), "name constraint") {
		t.Errorf("error does not mention the name constraint: %v", err)
	}

	if _, err := NewCA(entry.RootCertificate, entry.SigningKey, Options{AllowUnconstrained: true}); err != nil {
		t.Fatalf("NewCA with AllowUnconstrained: %v", err)
	}
}

func TestLoadCARefusesAnUnconstrainedCAByDefault(t *testing.T) {
	entry, err := GenerateRoot(GenerateRootOptions{CommonName: "wide open", Lifetime: time.Hour})
	if err != nil {
		t.Fatalf("GenerateRoot: %v", err)
	}
	certPEM, keyPEM := entryPEM(t, entry)

	if _, err := LoadCA(certPEM, keyPEM, Options{}); err == nil {
		t.Fatal("LoadCA accepted a CA with no name constraint")
	}
}

func TestNewCARejectsAMismatchedKey(t *testing.T) {
	a, err := GenerateRoot(GenerateRootOptions{CommonName: "a", Lifetime: time.Hour, PermittedDNSDomains: []string{"example"}})
	if err != nil {
		t.Fatalf("GenerateRoot a: %v", err)
	}
	b, err := GenerateRoot(GenerateRootOptions{CommonName: "b", Lifetime: time.Hour, PermittedDNSDomains: []string{"example"}})
	if err != nil {
		t.Fatalf("GenerateRoot b: %v", err)
	}

	// Signing with the wrong key produces a chain nothing can verify, and the
	// failure otherwise surfaces at the first handshake rather than at load.
	if _, err := NewCA(a.RootCertificate, b.SigningKey, Options{}); err == nil {
		t.Fatal("NewCA accepted a key that does not match the certificate")
	}
}

// ---------------------------------------------------------------- the pool

func TestFromPoolRoundTrip(t *testing.T) {
	entry, err := GenerateRoot(GenerateRootOptions{
		CommonName:          "pooled CA",
		Lifetime:            time.Hour,
		PermittedDNSDomains: []string{"example"},
	})
	if err != nil {
		t.Fatalf("GenerateRoot: %v", err)
	}

	// Through the same serialization podcertcontroller's CAs use.
	poolBytes, err := localca.Marshal(&localca.Pool{CAs: []*localca.CA{entry}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	pool, err := localca.Unmarshal(poolBytes)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	ca, err := FromPool(pool, "", Options{})
	if err != nil {
		t.Fatalf("FromPool: %v", err)
	}
	minted, err := ca.Sign("host.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	if _, err := parseChain(t, minted.CertChainPEM)[0].Verify(x509.VerifyOptions{
		DNSName: "host.example", Roots: roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf from a pooled CA does not verify: %v", err)
	}
}

func TestFromPoolSelectsByID(t *testing.T) {
	var entries []*localca.CA
	for _, id := range []string{"first", "second"} {
		entry, err := GenerateRoot(GenerateRootOptions{
			CommonName: id, Lifetime: time.Hour, PermittedDNSDomains: []string{"example"},
		})
		if err != nil {
			t.Fatalf("GenerateRoot %s: %v", id, err)
		}
		entry.ID = id
		entries = append(entries, entry)
	}
	pool := &localca.Pool{CAs: entries}

	ca, err := FromPool(pool, "second", Options{})
	if err != nil {
		t.Fatalf("FromPool: %v", err)
	}
	if got := ca.Certificate().Subject.CommonName; got != "second" {
		t.Errorf("selected CA CN = %q, want %q", got, "second")
	}

	// An empty ID takes the first, which is what a single-CA pool relies on.
	ca, err = FromPool(pool, "", Options{})
	if err != nil {
		t.Fatalf("FromPool(''): %v", err)
	}
	if got := ca.Certificate().Subject.CommonName; got != "first" {
		t.Errorf("default CA CN = %q, want %q", got, "first")
	}

	// A typo in --ca-id must not silently fall back to some other CA.
	if _, err := FromPool(pool, "third", Options{}); err == nil {
		t.Fatal("FromPool accepted an unknown CA ID")
	}
}

func TestFromPoolRejectsAnEmptyPool(t *testing.T) {
	if _, err := FromPool(&localca.Pool{}, "", Options{AllowUnconstrained: true}); err == nil {
		t.Fatal("FromPool accepted an empty pool")
	}
	if _, err := FromPool(nil, "", Options{AllowUnconstrained: true}); err == nil {
		t.Fatal("FromPool accepted a nil pool")
	}
}

// -------------------------------------------------- the delegated intermediate

func intermediateCA(t *testing.T, lifetime time.Duration) *CA {
	t.Helper()
	ca, _, err := GenerateCA("delegating CA", time.Hour, []string{"example"}, Options{
		IntermediateLifetime: lifetime,
	})
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return ca
}

func TestIntermediateChainVerifies(t *testing.T) {
	ca := intermediateCA(t, 30*time.Minute)

	minted, err := ca.Sign("host.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	chain := parseChain(t, minted.CertChainPEM)
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3 (leaf + intermediate + root)", len(chain))
	}
	leaf, intermediate, root := chain[0], chain[1], chain[2]

	if !root.Equal(ca.Certificate()) {
		t.Error("chain does not end in the trust anchor")
	}
	if !intermediate.Equal(ca.IssuerCertificate()) {
		t.Error("the middle certificate is not the reported issuer")
	}
	if !intermediate.IsCA || !intermediate.MaxPathLenZero {
		t.Errorf("intermediate is not a leaf-only CA: IsCA=%v MaxPathLenZero=%v",
			intermediate.IsCA, intermediate.MaxPathLenZero)
	}
	// The constraint has to survive delegation, or the intermediate becomes a
	// way to launder an unconstrained signing capability out of a constrained
	// root.
	if len(intermediate.PermittedDNSDomains) == 0 {
		t.Error("intermediate dropped the root's name constraint")
	}

	// Verify the way a TLS client does: only the root is trusted, and the
	// intermediate has to come from the chain the server presented.
	roots := x509.NewCertPool()
	roots.AddCert(root)
	inters := x509.NewCertPool()
	inters.AddCert(intermediate)
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: "host.example", Roots: roots, Intermediates: inters,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("delegated leaf does not verify: %v", err)
	}
	if _, err := tls.X509KeyPair(minted.CertChainPEM, minted.PrivateKeyPEM); err != nil {
		t.Errorf("delegated chain/key is not a usable TLS keypair: %v", err)
	}

	// Delegation must not change what clients are configured to trust.
	if ca.Certificate().Equal(ca.IssuerCertificate()) {
		t.Error("Certificate() returned the intermediate; it must stay the trust anchor")
	}
	// The name constraint still bites through the extra hop.
	outside, err := ca.Sign("evil.test", time.Minute)
	if err != nil {
		t.Fatalf("Sign outside constraint: %v", err)
	}
	outChain := parseChain(t, outside.CertChainPEM)
	outInters := x509.NewCertPool()
	outInters.AddCert(outChain[1])
	if _, err := outChain[0].Verify(x509.VerifyOptions{
		DNSName: "evil.test", Roots: roots, Intermediates: outInters,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("a delegated leaf outside the name constraint verified")
	}
}

func TestIntermediateRefusedWhenTheRootForbidsOne(t *testing.T) {
	// GenerateRoot without AllowIntermediate sets pathLenConstraint 0. Signing
	// an intermediate under it yields chains every verifier rejects, so this
	// has to fail at construction rather than at the first handshake.
	entry, err := GenerateRoot(GenerateRootOptions{
		CommonName: "leaf-only root", Lifetime: time.Hour, PermittedDNSDomains: []string{"example"},
	})
	if err != nil {
		t.Fatalf("GenerateRoot: %v", err)
	}
	if !entry.RootCertificate.MaxPathLenZero {
		t.Fatal("precondition: expected a pathLenConstraint of 0")
	}

	_, err = NewCA(entry.RootCertificate, entry.SigningKey, Options{IntermediateLifetime: time.Minute})
	if err == nil {
		t.Fatal("NewCA issued an intermediate under a pathLenConstraint-0 root")
	}
	if !strings.Contains(err.Error(), "pathLenConstraint") {
		t.Errorf("error does not explain the path length problem: %v", err)
	}
}

func TestIntermediateRenewsWhenDue(t *testing.T) {
	ca := intermediateCA(t, 30*time.Minute)
	first := ca.IssuerCertificate()

	if _, err := ca.Sign("before.example", time.Minute); err != nil {
		t.Fatalf("Sign before renewal: %v", err)
	}
	if !ca.IssuerCertificate().Equal(first) {
		t.Fatal("the intermediate was replaced before its renewal point")
	}

	// Force the renewal point into the past rather than sleeping: this is
	// exactly the condition issuerFor tests, and it keeps the test instant.
	old := ca.current.Load()
	ca.current.Store(&issuer{
		cert: old.cert, key: old.key, chainDER: old.chainDER,
		renewAt: time.Now().Add(-time.Second),
	})

	minted, err := ca.Sign("after.example", time.Minute)
	if err != nil {
		t.Fatalf("Sign after renewal: %v", err)
	}
	second := ca.IssuerCertificate()
	if second.Equal(first) {
		t.Fatal("the intermediate was not renewed past its renewal point")
	}
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Error("the renewed intermediate reused a serial number")
	}

	// The renewed chain must still verify against the unchanged trust anchor,
	// which is the whole reason clients can keep pinning the root.
	chain := parseChain(t, minted.CertChainPEM)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	inters := x509.NewCertPool()
	inters.AddCert(chain[1])
	if _, err := chain[0].Verify(x509.VerifyOptions{
		DNSName: "after.example", Roots: roots, Intermediates: inters,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf from the renewed intermediate does not verify: %v", err)
	}
}

func TestSignClampsLeafLifetimeToItsIssuer(t *testing.T) {
	ca := intermediateCA(t, 2*time.Minute)
	issuerNotAfter := ca.IssuerCertificate().NotAfter

	// A leaf outliving its issuer is accepted at handshake time and rejected
	// later, with an error that points at the leaf rather than at the CA.
	minted, err := ca.Sign("long.example", time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if minted.NotAfter.After(issuerNotAfter) {
		t.Errorf("leaf NotAfter = %v, outlives its issuer at %v", minted.NotAfter, issuerNotAfter)
	}
	if leaf := parseChain(t, minted.CertChainPEM)[0]; leaf.NotAfter.After(issuerNotAfter) {
		t.Errorf("encoded leaf NotAfter = %v, outlives its issuer at %v", leaf.NotAfter, issuerNotAfter)
	}
}

func TestSignIsSafeUnderConcurrentRenewal(t *testing.T) {
	ca := intermediateCA(t, 30*time.Minute)
	old := ca.current.Load()
	ca.current.Store(&issuer{
		cert: old.cert, key: old.key, chainDER: old.chainDER,
		renewAt: time.Now().Add(-time.Second),
	})

	// Run under -race: a burst arriving past the renewal point must produce
	// one new intermediate, not one per caller.
	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := ca.Sign("burst.example", time.Minute)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Sign: %v", err)
		}
	}
	if ca.IssuerCertificate().Equal(old.cert) {
		t.Error("no renewal happened")
	}
}
