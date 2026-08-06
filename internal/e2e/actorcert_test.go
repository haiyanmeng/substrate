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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/substratex509"
)

// These run without a cluster, against a throwaway CA. They are worth having
// because the e2e suites that use this helper can only fail one way -- "the
// gateway refused" -- and a certificate this file built wrong would look
// exactly like the policy working. Checking the shape here means an e2e failure
// is about the gateway.

// TestActorCredentialCarriesTheIdentity is the assertion the whole helper
// exists for: what the egress PEP reads back out of the certificate is the
// actor the test asked to be.
func TestActorCredentialCarriesTheIdentity(t *testing.T) {
	credential := credentialPEM(t, actorTemplate(t, "acme-prod", "metrics-shipper", "uid-123"), throwawayCA(t))

	leaf := leafOf(t, credential)
	identity, err := substratex509.ActorIdentityFromCertificate(leaf)
	if err != nil {
		t.Fatalf("ActorIdentityFromCertificate: %v", err)
	}
	if identity == nil {
		t.Fatal("the minted certificate carries no ActorIdentity")
	}
	if identity.Atespace != "acme-prod" || identity.ActorName != "metrics-shipper" || identity.ActorUid != "uid-123" {
		t.Errorf("identity is %+v, want acme-prod/metrics-shipper uid-123", identity)
	}
	// The purpose the egress PEP enforces. Minting one that is not atunnel would
	// make every e2e case fail as an unusable identity.
	if identity.Purpose != substratex509.ActorIdentityPurposeAtunnel {
		t.Errorf("identity purpose is %q, want %q", identity.Purpose, substratex509.ActorIdentityPurposeAtunnel)
	}
}

// TestActorCredentialIsUsableAsAClientCertificate covers the other half of the
// contract: the probe loads this with tls.X509KeyPair, so a bundle in the wrong
// order or with the wrong key encoding fails at the front door rather than at
// the policy layer.
func TestActorCredentialIsUsableAsAClientCertificate(t *testing.T) {
	credential := credentialPEM(t, actorTemplate(t, "acme-prod", "wide-open", "uid-1"), throwawayCA(t))

	if _, err := tls.X509KeyPair([]byte(credential), []byte(credential)); err != nil {
		t.Fatalf("tls.X509KeyPair over the minted credential: %v", err)
	}
	if !strings.HasPrefix(credential, "-----BEGIN CERTIFICATE-----") {
		t.Error("the credential does not start with the leaf certificate")
	}
	if !strings.Contains(credential, "-----BEGIN PRIVATE KEY-----") {
		t.Error("the credential carries no private key")
	}
}

// TestForgedActorCredentialDefeatsValidation checks the forge does what it
// claims: put JSON in the extension that AddActorIdentityToCertificate would
// have rejected, so the e2e suite can reach the checks that only fire on input
// no honest minter produces.
func TestForgedActorCredentialDefeatsValidation(t *testing.T) {
	ca := throwawayCA(t)
	cases := []struct {
		name string
		json string
	}{{
		name: "a purpose other than atunnel",
		json: `{"Atespace":"acme-prod","ActorName":"wide-open","ActorUid":"uid-1","Purpose":"session"}`,
	}, {
		name: "an incomplete identity",
		json: `{"Atespace":"acme-prod","ActorName":"","ActorUid":"uid-1","Purpose":"atunnel"}`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credential := credentialPEM(t, forgedActorTemplate(t, "acme-prod", "wide-open", tc.json), ca)

			// Rejected, and rejected as an error rather than as "no identity
			// here" -- the e2e case asserts on the former's denial reason.
			identity, err := substratex509.ActorIdentityFromCertificate(leafOf(t, credential))
			if err == nil {
				t.Fatalf("ActorIdentityFromCertificate accepted the forged identity and returned %+v", identity)
			}
			t.Logf("rejected as expected: %v", err)
		})
	}
}

// throwawayCA stands in for the cluster's actor CA. Nothing verifies the chain
// in these tests; it only has to be able to sign.
func throwawayCA(t *testing.T) *localca.CA {
	t.Helper()
	ca, err := localca.GenerateED25519CA("test-actor-ca")
	if err != nil {
		t.Fatalf("generating a throwaway CA: %v", err)
	}
	return ca
}

func leafOf(t *testing.T, credentialPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(credentialPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("the credential does not begin with a PEM CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the minted leaf: %v", err)
	}
	return leaf
}
