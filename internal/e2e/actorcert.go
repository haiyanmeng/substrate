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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"path"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/substratex509"
)

// Where the actor signing CA lives. Fixed by hack/install-ate.sh's
// create_actor_id_ca_pool_secret and passed to ate-apiserver as
// --actor-id-ca-pool.
const (
	actorCAPoolNamespace = "ate-system"
	actorCAPoolSecret    = "actor-id-ca-pool"
	actorCAPoolSecretKey = "pool"
)

// actorCertificateLifetime is short because these certificates are minted for
// one test and should not outlive it. ate-apiserver issues an hour; nothing
// downstream reads the value, so a mismatch here is not a fidelity problem.
const actorCertificateLifetime = 10 * time.Minute

// MintActorCredential signs a certificate that names the given actor and
// returns it as a PEM credential -- certificate then private key -- ready to
// present as a TLS client certificate.
//
// This is how a test gets to be an actor. In production the chain is
// atunnel -> atelet -> ate-apiserver, and ate-apiserver derives the actor from
// the worker Pod's real assignment; a test process has no assignment to derive
// from, so it signs with the CA directly. The certificate is byte-for-byte the
// shape ate-apiserver produces (cmd/ateapi/internal/actoridentity/actoridentity.go),
// which is what makes it acceptable to anything that authenticates actors.
//
// Two consequences worth stating. This reads the signing key out of the CA pool
// Secret, so it works only against a test cluster whose kubeconfig could read
// that Secret anyway -- the same argument the sdsmint suite makes for reading
// the MITM pool. And it deliberately skips ate-apiserver, so it proves nothing
// about minting; internal/atunnel/credential_test.go covers that path.
func MintActorCredential(t *testing.T, ctx context.Context, atespace, actorName, actorUID string) string {
	t.Helper()
	return credentialPEM(t, actorTemplate(t, atespace, actorName, actorUID), actorSigningCA(t, ctx))
}

// actorTemplate builds the certificate for one actor.
func actorTemplate(t *testing.T, atespace, actorName, actorUID string) *x509.Certificate {
	t.Helper()
	template := actorCertificateTemplate(t, atespace, actorName)
	if err := substratex509.AddActorIdentityToCertificate(&substratex509.ActorIdentity{
		Atespace:  atespace,
		ActorName: actorName,
		ActorUid:  actorUID,
		Purpose:   substratex509.ActorIdentityPurposeAtunnel,
	}, template); err != nil {
		t.Fatalf("adding ActorIdentity for %s/%s: %v", atespace, actorName, err)
	}
	return template
}

// MintForgedActorCredential signs a certificate whose ActorIdentity extension
// holds identityJSON verbatim, bypassing the validation
// substratex509.AddActorIdentityToCertificate applies on the way in.
//
// It exists for the cases a well-formed mint cannot produce: an identity with a
// Purpose other than atunnel, or with a field left empty. ate-apiserver always
// sets Purpose atunnel, so the check that rejects anything else -- the one
// cmd/atelet/credentialbroker.go points at -- is only reachable with a
// certificate forged here.
//
// The template is built with a valid identity and the extension value then
// overwritten, which reuses the real OID rather than restating it.
func MintForgedActorCredential(t *testing.T, ctx context.Context, atespace, actorName, identityJSON string) string {
	t.Helper()
	return credentialPEM(t, forgedActorTemplate(t, atespace, actorName, identityJSON), actorSigningCA(t, ctx))
}

// forgedActorTemplate builds a certificate whose ActorIdentity extension holds
// identityJSON verbatim.
func forgedActorTemplate(t *testing.T, atespace, actorName, identityJSON string) *x509.Certificate {
	t.Helper()
	template := actorTemplate(t, atespace, actorName, "placeholder")
	if len(template.ExtraExtensions) != 1 {
		t.Fatalf("template has %d extra extensions, want exactly the ActorIdentity one", len(template.ExtraExtensions))
	}
	template.ExtraExtensions[0].Value = []byte(identityJSON)
	return template
}

// actorCertificateTemplate mirrors the template ate-apiserver builds in
// actoridentity.go's MintCert. Keep the two in step: a divergence here makes a
// test pass or fail for a reason no real actor would ever hit.
func actorCertificateTemplate(t *testing.T, atespace, actorName string) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generating serial number: %v", err)
	}
	return &x509.Certificate{
		SerialNumber: serial,
		URIs: []*url.URL{{
			Scheme: "spiffe",
			Host:   "substrate-actor.local",
			Path:   path.Join("atespace", atespace, "actor", actorName),
		}},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(actorCertificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		Issuer: pkix.Name{
			CommonName: "api.ate-system.svc.cluster.local",
		},
	}
}

// credentialPEM signs template with ca and returns a certificate-then-key PEM
// bundle. A fresh key per certificate: nothing here renews, and reusing one
// would make two "different" actors share a key.
//
// The CA is a parameter rather than looked up here so the shape of what this
// produces can be checked without a cluster; see actorcert_test.go.
func credentialPEM(t *testing.T, template *x509.Certificate, ca *localca.CA) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating actor private key: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.RootCertificate, &key.PublicKey, ca.SigningKey)
	if err != nil {
		t.Fatalf("signing actor certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling actor private key: %v", err)
	}

	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// ate-apiserver returns its intermediates after the leaf; include them so a
	// pool that later grows a delegated signer keeps working here.
	for _, intermediate := range ca.IntermediateCertificates {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: intermediate.Raw})...)
	}
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	return string(out)
}

// actorSigningCA reads the CA that ate-apiserver signs actor certificates with,
// straight from the Secret the deployment mounts, so a test certificate is
// trusted by exactly what trusts a real one.
func actorSigningCA(t *testing.T, ctx context.Context) *localca.CA {
	t.Helper()
	secret, err := GetClients().K8s.CoreV1().Secrets(actorCAPoolNamespace).Get(ctx, actorCAPoolSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading actor CA pool secret %s/%s: %v", actorCAPoolNamespace, actorCAPoolSecret, err)
	}
	pool, err := localca.Unmarshal(secret.Data[actorCAPoolSecretKey])
	if err != nil {
		t.Fatalf("parsing actor CA pool from %s/%s key %q: %v", actorCAPoolNamespace, actorCAPoolSecret, actorCAPoolSecretKey, err)
	}
	if len(pool.CAs) == 0 {
		t.Fatalf("actor CA pool %s/%s contains no CAs", actorCAPoolNamespace, actorCAPoolSecret)
	}
	// CAs[0], because that is the one actoridentity.go signs with. Selecting by
	// ID here would drift from the server the moment the pool holds two.
	return pool.CAs[0]
}
