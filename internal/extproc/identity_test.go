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

package extproc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/substratex509"
)

// actorCertPEM mints a certificate carrying an ActorIdentity, the way
// cmd/ateapi/internal/actoridentity does.
//
// Self-signed, because nothing in this package verifies the chain -- Envoy does
// that against the gateway's client-CA bundle before ext_proc is called at all,
// and by the time a certificate reaches here it is already trusted. What this
// package reads is the extension.
func actorCertPEM(t *testing.T, identity *substratex509.ActorIdentity) string {
	t.Helper()
	return signCert(t, certTemplate(t, identity))
}

// certTemplate builds the template ateapi would sign. A nil identity leaves the
// extension off, which is what an ordinary pod certificate looks like.
func certTemplate(t *testing.T, identity *substratex509.ActorIdentity) *x509.Certificate {
	t.Helper()

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "atespaces:acme-prod:actors:test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if identity != nil {
		if err := substratex509.AddActorIdentityToCertificate(identity, template); err != nil {
			t.Fatalf("adding the ActorIdentity extension: %v", err)
		}
	}
	return template
}

func signCert(t *testing.T, template *x509.Certificate) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// forgedIdentityCertPEM mints a certificate whose ActorIdentity extension holds
// arbitrary JSON.
//
// AddActorIdentityToCertificate validates on the way in, so an identity this
// package must reject cannot be minted through the front door. The template is
// built with a valid identity and the extension value then overwritten, which
// reuses the real OID rather than restating it here.
func forgedIdentityCertPEM(t *testing.T, jsonValue string) string {
	t.Helper()

	template := certTemplate(t, &substratex509.ActorIdentity{
		Atespace: "placeholder", ActorName: "placeholder", ActorUid: "placeholder",
		Purpose: substratex509.ActorIdentityPurposeAtunnel,
	})
	if len(template.ExtraExtensions) != 1 {
		t.Fatalf("template carries %d extra extensions, want exactly the ActorIdentity one", len(template.ExtraExtensions))
	}
	template.ExtraExtensions[0].Value = []byte(jsonValue)
	return signCert(t, template)
}

// xfccFor renders the header Envoy would set for an actor's certificate.
func xfccFor(t *testing.T, atespace, name string) string {
	t.Helper()
	return xfccWithCert(actorCertPEM(t, &substratex509.ActorIdentity{
		Atespace:  atespace,
		ActorName: name,
		ActorUid:  "uid-" + name,
		Purpose:   substratex509.ActorIdentityPurposeAtunnel,
	}))
}

// xfccWithCert wraps a PEM certificate the way Envoy's
// set_current_client_cert_details does: percent-encoded, quoted, alongside the
// other fields it emits.
func xfccWithCert(certPEM string) string {
	return fmt.Sprintf(`By=spiffe://cluster.local/ns/ate-system/sa/atenet-egress;`+
		`Hash=0123456789abcdef;`+
		`Subject="CN=worker,O=substrate";`+
		`Cert="%s"`, url.PathEscape(certPEM))
}

func TestActorFromXFCCReadsTheIdentity(t *testing.T) {
	key, uid, err := actorFromXFCC(xfccFor(t, DemoAtespace, "repo-reader"))
	if err != nil {
		t.Fatalf("actorFromXFCC: %v", err)
	}
	if want := (ActorKey{Atespace: DemoAtespace, Name: "repo-reader"}); key != want {
		t.Errorf("actor = %v, want %v", key, want)
	}
	if uid != "uid-repo-reader" {
		t.Errorf("uid = %q, want %q", uid, "uid-repo-reader")
	}
}

// A pod that is not an actor -- the e2e egressprobe, say -- presents a valid
// podidentity certificate with no ActorIdentity extension. That is not an
// error, it just has no policy, and the caller denies it as an unknown actor.
func TestActorFromXFCCAcceptsANonActorCertificate(t *testing.T) {
	key, uid, err := actorFromXFCC(xfccWithCert(actorCertPEM(t, nil)))
	if err != nil {
		t.Fatalf("actorFromXFCC: %v", err)
	}
	if !key.Zero() || uid != "" {
		t.Errorf("actor = %v, uid = %q; want the zero identity", key, uid)
	}
}

// The purpose check is the egress PEP the atelet credential broker was waiting
// for: a certificate minted for some other purpose must not be usable to open a
// tunnel.
func TestActorFromXFCCRejectsANonAtunnelPurpose(t *testing.T) {
	forged := forgedIdentityCertPEM(t, `{"Atespace":"acme-prod","ActorName":"repo-reader","ActorUid":"u1","Purpose":"session"}`)

	_, _, err := actorFromXFCC(xfccWithCert(forged))
	if err == nil {
		t.Fatal("a non-atunnel purpose was accepted")
	}
	if !strings.Contains(err.Error(), "Purpose") {
		t.Errorf("err = %v, want it to name the Purpose", err)
	}
}

// An identity missing a field is a mint-side bug, and the gateway must not
// paper over it by treating the certificate as an ordinary non-actor pod.
func TestActorFromXFCCRejectsAnIncompleteIdentity(t *testing.T) {
	forged := forgedIdentityCertPEM(t, `{"Atespace":"acme-prod","Purpose":"atunnel"}`)

	if _, _, err := actorFromXFCC(xfccWithCert(forged)); err == nil {
		t.Fatal("an identity with no ActorName was accepted")
	}
}

func TestActorFromXFCCRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"empty header", ""},
		{"no Cert field", `By=spiffe://x;Hash=abc;Subject="CN=worker"`},
		{"Cert is not PEM", `Cert="not-a-certificate"`},
		{"Cert is PEM but not a certificate", `Cert="` + url.PathEscape("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n") + `"`},
		{"Cert is a truncated certificate", `Cert="` + url.PathEscape("-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n") + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := actorFromXFCC(tc.value); err == nil {
				t.Error("malformed input was accepted")
			}
		})
	}
}

// A Subject is a DN and contains commas, which are also the element separator.
// Splitting naively truncates the header before Cert and reports "no Cert
// field" on a perfectly good request.
func TestXFCCFieldRespectsQuotedSeparators(t *testing.T) {
	header := `By=spiffe://x;Hash=abc;Subject="CN=worker,O=substrate,L=here";Cert="hello"`
	got, ok := xfccField(header, "Cert")
	if !ok || got != "hello" {
		t.Errorf("Cert = %q, %v; want %q, true", got, ok, "hello")
	}
	if subject, _ := xfccField(header, "Subject"); subject != "CN=worker,O=substrate,L=here" {
		t.Errorf("Subject = %q", subject)
	}
}

// XFCC is ordered outermost hop first, so the peer Envoy just authenticated is
// the last element. Under SANITIZE_SET there is only ever one, but reading the
// wrong end would fail silently if that changed.
func TestXFCCFieldTakesTheNearestHop(t *testing.T) {
	got, ok := xfccField(`Cert="outer",Cert="nearest"`, "Cert")
	if !ok || got != "nearest" {
		t.Errorf("Cert = %q, %v; want %q, true", got, ok, "nearest")
	}
}

// url.QueryUnescape would turn '+' into a space here. Base64 uses '+', so about
// half of all certificates would be corrupted -- and which half depends on the
// key material, which makes it look intermittent.
func TestPeerCertificateDecodingPreservesPlus(t *testing.T) {
	// Mint until a certificate whose PEM contains '+' turns up. P-256 bodies are
	// ~600 base64 characters, so this is effectively the first one.
	var certPEM string
	for i := 0; i < 50; i++ {
		certPEM = actorCertPEM(t, &substratex509.ActorIdentity{
			Atespace: DemoAtespace, ActorName: "plus", ActorUid: "u",
			Purpose: substratex509.ActorIdentityPurposeAtunnel,
		})
		if strings.Contains(certPEM, "+") {
			break
		}
	}
	if !strings.Contains(certPEM, "+") {
		t.Skip("no '+' in 50 generated certificates")
	}
	if _, err := parsePeerCertificate(url.PathEscape(certPEM)); err != nil {
		t.Fatalf("parsePeerCertificate: %v", err)
	}
}
