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

// Package sdsmint e2e-tests the egress gateway's certificate minter.
//
// sdsmintd is an SDS server that mints a leaf certificate on demand for the
// SNI Envoy was asked for. Its unit tests cover the SDS protocol against a fake
// Envoy; what they cannot cover is the part that has actually broken in
// practice -- whether the deployed pod's Envoy, CA pool secret, allowlist and
// unix socket line up. Every assertion here is made on a certificate that came
// off a real handshake through the real gateway.
//
// The probe here is a plain Pod issuing its own CONNECT, which is the only way
// to drive the minter with an arbitrary SNI: it lets a suite ask for a name
// nobody has subscribed to, and for one deliberately outside the allowlist.
// internal/e2e/suites/actoregress covers the other side -- a real Actor whose
// traffic is redirected into the gateway -- and can assert only that the chain
// roots here, because it does not get to choose the identity it presents.
//
// Reaching the minter now takes an authorized CONNECT: extprocd decides, from
// the client certificate, whether the tunnel opens at all. The probe therefore
// presents an actor identity chosen for the route its policy selects rather
// than for its permissions -- see mitmRoutedCredential -- and every handshake
// goes through handshake(), which
// separates a policy refusal from a minting refusal before any assertion here
// gets to blame sdsmintd. Egress authorization itself is
// internal/e2e/suites/egressauthz.
package sdsmint

import (
	"context"
	"crypto/x509"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/e2e/fixtures/egressprobe/probeapi"
	"github.com/agent-substrate/substrate/internal/extproc"
)

const (
	// leafTTL is --ttl on the sdsmintd sidecar, and leafSkew is the backdating
	// internal/sdsmint/ca.go applies to NotBefore. Their sum is the validity
	// span every leaf should carry. Keep both in step with the manifest: a leaf
	// that suddenly lasts hours is the failure this pair is here to catch.
	leafTTL  = 5 * time.Minute
	leafSkew = 1 * time.Minute
)

// TestSdsmintMintsALeafPerSNI is the core functional assertion: two different
// SNIs through one gateway come back as two different certificates, each issued
// for the name that was asked for and each chaining to the MITM CA.
//
// The SNIs are derived from the test's own namespace so that repeated runs use
// names Envoy has never subscribed to. Reusing a name would be served from
// Envoy's live secret set (--idle is 30m) and the test would pass without
// sdsmintd having minted anything.
func TestSdsmintMintsALeafPerSNI(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)

	root := e2e.MITMRootCertificate(t, ctx)
	probe := e2e.StartEgressProbe(t, ctx, ns.Name)
	credential := mitmRoutedCredential(t, ctx)

	first := ns.Name + "-a.example.com"
	second := ns.Name + "-b.example.com"

	chains := map[string][]*x509.Certificate{}
	for _, sni := range []string{first, second} {
		result := handshake(t, ctx, probe, credential, sni)
		if !result.HandshakeOK {
			t.Fatalf("handshake for allowlisted SNI %q failed: %s", sni, result.HandshakeError)
		}
		chains[sni] = e2e.ParseCertChain(t, sni, result.ChainPEM)
	}

	for sni, chain := range chains {
		leaf := chain[0]

		// Minted for the name that was asked for, and only that name. A leaf
		// carrying anything else means the SNI Envoy policed is not the name
		// the certificate authorizes.
		if got := leaf.DNSNames; len(got) != 1 || got[0] != sni {
			t.Errorf("leaf for %q has DNSNames %v, want exactly [%q]", sni, got, sni)
		}
		if leaf.Subject.CommonName != sni {
			t.Errorf("leaf for %q has CN %q, want %q", sni, leaf.Subject.CommonName, sni)
		}

		// Chains to the MITM root the installer created, verified for the SNI
		// itself so the root's dNSName constraint is applied too.
		opts := x509.VerifyOptions{
			DNSName:       sni,
			Roots:         e2e.CertPool(root),
			Intermediates: e2e.CertPool(chain[1:]...),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		if _, err := leaf.Verify(opts); err != nil {
			t.Errorf("leaf for %q does not verify against the %s/%s MITM root: %v", sni, e2e.EgressNamespace, e2e.MITMCASecret, err)
		}

		// Short-lived, which is what bounds the damage from a leaked leaf key
		// and what makes the MITM CA tolerable at all. The window is generous
		// because the point is to catch a TTL that is wrong by an order of
		// magnitude, not clock skew.
		validity := leaf.NotAfter.Sub(leaf.NotBefore)
		if want := leafTTL + leafSkew; validity < want-time.Minute || validity > want+time.Minute {
			t.Errorf("leaf for %q is valid for %v, want about %v (--ttl in atenet-egress.yaml)", sni, validity, want)
		}
		if time.Now().After(leaf.NotAfter) {
			t.Errorf("leaf for %q was already expired when served (NotAfter %s)", sni, leaf.NotAfter)
		}

		// Signed by a delegated intermediate rather than by the root. This is
		// --ca-intermediate-ttl doing its job: the root signs a few times a
		// day instead of once per cache miss.
		if len(chain) < 2 {
			t.Errorf("chain for %q has only the leaf; expected a delegated intermediate", sni)
			continue
		}
		intermediate := chain[1]
		if intermediate.Equal(root) {
			t.Errorf("leaf for %q was signed by the root directly; --ca-intermediate-ttl is not in effect", sni)
		}
		// The backstop under the allowlist. The allowlist is what sdsmintd
		// agrees to mint; this is what the key is cryptographically able to
		// mint at all, and it is what --ca-allow-unconstrained would remove.
		if len(intermediate.PermittedDNSDomains) == 0 {
			t.Errorf("intermediate signing %q carries no dNSName constraint; the MITM CA can sign for any name", sni)
		}

		t.Logf("%s: served a %d-cert chain, leaf serial %s valid %v, issued by %q constrained to %v",
			sni, len(chain), leaf.SerialNumber.Text(16), validity, intermediate.Subject.CommonName, intermediate.PermittedDNSDomains)
	}

	if a, b := chains[first][0], chains[second][0]; a.SerialNumber.Cmp(b.SerialNumber) == 0 {
		t.Errorf("both SNIs were served the same certificate (serial %s); the gateway is not minting per name", a.SerialNumber.Text(16))
	}
}

// TestSdsmintRefusesSNIOutsideTheAllowlist is the security half. --allow is the
// entire egress policy for the MITM leg, so an SNI outside it must not produce
// a certificate -- sdsmintd withdraws the resource, Envoy has nothing to
// present, and the handshake fails. A pass here that used to fail means the
// gateway will sign for names it was never authorized for.
//
// The tunnel opens either way: the actor is authorized to CONNECT, and the name
// it then asks for is not something the CONNECT checkpoint can see. That is the
// separation handshake() enforces.
func TestSdsmintRefusesSNIOutsideTheAllowlist(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)

	probe := e2e.StartEgressProbe(t, ctx, ns.Name)
	credential := mitmRoutedCredential(t, ctx)

	// .invalid can never be delegated (RFC 6761), so this cannot collide with
	// a name someone later adds to the allowlist.
	sni := ns.Name + ".notallowed.invalid"
	result := handshake(t, ctx, probe, credential, sni)
	if result.HandshakeOK {
		chain := e2e.ParseCertChain(t, sni, result.ChainPEM)
		t.Fatalf("gateway served a certificate for non-allowlisted SNI %q (leaf CN %q, DNSNames %v) -- the allowlist is not being enforced",
			sni, chain[0].Subject.CommonName, chain[0].DNSNames)
	}
	t.Logf("non-allowlisted SNI %q was refused as expected: %s", sni, result.HandshakeError)
}

// handshake asks the probe for one inner TLS handshake, having first confirmed
// the gateway allowed the CONNECT that carries it.
//
// The two refusals look nothing alike and must not be confused. extprocd
// refuses the CONNECT with a 403 and no tunnel is created; sdsmintd refuses by
// declining to mint, which leaves Envoy with no certificate to present inside a
// tunnel that opened normally. Only the second is this suite's business, so a
// policy refusal fails here with that said plainly rather than being reported
// as an sdsmintd regression several assertions later.
func handshake(t *testing.T, ctx context.Context, probe *e2e.EgressProbe, credential, sni string) probeapi.Result {
	t.Helper()
	result := probe.Probe(t, ctx, probeapi.Request{SNI: sni, ClientCredentialPEM: credential})
	if !result.Connected {
		t.Fatalf("the gateway refused the CONNECT carrying SNI %q, so the MITM leg was never reached. "+
			"This is an egress authorization failure, not an sdsmintd one: %s", sni, result.ConnectError)
	}
	return result
}

// mitmRoutedCredential mints the actor identity the probe presents at the
// gateway's front door.
//
// The probe's own Pod certificate carries no ActorIdentity, and the gateway
// refuses a CONNECT it cannot attribute to an actor, so with it the probe never
// reaches the MITM leg at all.
//
// THE ACTOR IS CHOSEN FOR ITS ROUTE, NOT ITS PERMISSIONS. The CONNECT vhost
// branches on x-ate-egress-mode, and only the two hostname policies resolve to
// mitm; ALLOW_ALL and ALLOW_BY_IP_BLOCK are fully decided at the CONNECT and
// take the passthrough route straight out, which never reaches sdsmintd at all.
// So acme-prod/wide-open -- the obvious "constrains nothing" choice, and what
// this suite used before the route table branched -- would now dial
// 192.0.2.1:443 and time out. acme-prod/repo-reader is ALLOW_BY_HOSTNAME, so it
// is MITMed, which is the only thing this suite needs from it.
//
// Its hostname allowlist (github.com and friends) does not cover the SNIs below
// and does not have to: that list is checked at the inner checkpoint, which
// fires on the tunneled HTTP request, and the probe stops at the TLS
// handshake. If the probe ever learns to speak HTTP inside the tunnel, these
// tests will start failing on an authorization denial that has nothing to do
// with sdsmintd -- at which point the fix is a demo actor whose allowlist
// covers *.example.com, which today's exact-match allowlist cannot express for
// namespace-derived names.
func mitmRoutedCredential(t *testing.T, ctx context.Context) string {
	t.Helper()
	return e2e.MintActorCredential(t, ctx, extproc.DemoAtespace, "repo-reader", "uid-e2e-sdsmint")
}
