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
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

// push is one observed rotation push: when it landed on the stream, which
// certificate it carried, and when that certificate stops being valid.
type push struct {
	at       time.Time
	serial   string
	notAfter time.Time
}

// leafFromResource pulls the leaf x509 out of a delta Resource carrying a
// Secret, so a test can reason about the certificate Envoy would actually
// serve rather than just the xDS version string.
func leafFromResource(t *testing.T, res *discovery.Resource) *x509.Certificate {
	t.Helper()
	secret := unpackSecret(t, res)
	chain := secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes()
	block, _ := pem.Decode(chain)
	if block == nil {
		t.Fatalf("resource %q: certificate chain is not PEM", res.GetName())
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("resource %q: parsing leaf: %v", res.GetName(), err)
	}
	return leaf
}

// TestRotationOutlivesCacheReuse states the ordering the two independently
// chosen clocks have to obey. It is cheap and it names the invariant; the
// timing test below is what actually proves the consequence.
func TestRotationOutlivesCacheReuse(t *testing.T) {
	if !(reuseFraction < rotateFraction && rotateFraction < 1.0) {
		t.Fatalf("reuseFraction (%v) < rotateFraction (%v) < 1 is violated; rotation "+
			"refreshes via the minter cache, so inverting these makes every rotation "+
			"tick a no-op until the leaf has already expired", reuseFraction, rotateFraction)
	}
}

// TestRotationNeverServesAnExpiredLeaf pins down the interaction between two
// clocks that were chosen independently: the rotation ticker (2/3 of TTL) and
// the minter's cache entry lifetime (a full TTL).
//
// Envoy holds whatever leaf we last pushed until we push another one -- it has
// no expiry of its own for an on-demand secret. So the invariant that matters
// is not "we tick often enough", it is "every leaf is replaced before its own
// notAfter". This test collects the real push timeline and checks exactly that.
func TestRotationNeverServesAnExpiredLeaf(t *testing.T) {
	if testing.Short() {
		t.Skip("watches two full rotation cycles in real time")
	}

	// The TTL cannot be made arbitrarily small to speed this up, for two
	// reasons. The server floors the rotation interval at 1s. And x509
	// encodes notAfter with one-second granularity, so a leaf's real expiry
	// is up to a second earlier than ttl -- at a 1.5s TTL that alone eats
	// most of the rotation margin and the test goes flaky for a reason that
	// does not exist at the 5m production default.
	const ttl = 6 * time.Second
	// Rotation fires at 4s and 8s, so this covers two of them. Under the
	// pre-fix behaviour the tick at 4s was a cache hit that re-sent the old
	// leaf, and the replacement did not arrive until 8s -- two seconds after
	// that leaf had expired. That is the case this window is sized to catch.
	const observe = 9 * time.Second

	srv := testServer(t, ServerOptions{Rotate: true, TTL: ttl}, []string{"*.example"})
	stream, stop := startServer(t, srv)
	defer func() {
		if err := stop(); err != nil {
			t.Errorf("DeltaSecrets returned %v", err)
		}
	}()

	stream.requests <- &discovery.DeltaDiscoveryRequest{
		TypeUrl:                SecretTypeURL,
		ResourceNamesSubscribe: []string{"a.example"},
	}

	var pushes []push
	deadline := time.After(observe)
collect:
	for {
		select {
		case resp := <-stream.sent:
			for _, res := range resp.GetResources() {
				if res.GetName() != "a.example" {
					continue
				}
				leaf := leafFromResource(t, res)
				pushes = append(pushes, push{
					at:       time.Now(),
					serial:   res.GetVersion(),
					notAfter: leaf.NotAfter,
				})
			}
		case <-deadline:
			break collect
		}
	}

	if len(pushes) < 2 {
		t.Fatalf("only %d pushes in %v; expected the initial mint plus at least one rotation", len(pushes), observe)
	}

	// Report the timeline unconditionally: when this test fails the shape of
	// the failure is the finding, not the fact of it.
	start := pushes[0].at
	for i, p := range pushes {
		t.Logf("push %d at t+%-8v serial=%s valid until t+%v",
			i, p.at.Sub(start).Round(time.Millisecond), p.serial,
			p.notAfter.Sub(start).Round(time.Millisecond))
	}

	// Each leaf is served from the moment it is pushed until the next push
	// replaces it. If the next push lands after the current leaf's notAfter,
	// Envoy served an expired certificate for the difference.
	var worst time.Duration
	for i := 0; i < len(pushes)-1; i++ {
		if gap := pushes[i+1].at.Sub(pushes[i].notAfter); gap > worst {
			worst = gap
		}
	}
	if worst > 0 {
		t.Errorf("served an expired leaf for %v: a push landed that long after the "+
			"previous leaf's notAfter (TTL %v, rotation interval %v)",
			worst.Round(time.Millisecond), ttl, time.Duration(float64(ttl)*rotateFraction))
	}

	// The last leaf we pushed must still have been valid when we stopped
	// watching, or the run ended inside a staleness window the loop above
	// cannot see (there is no "next push" to measure against).
	if last := pushes[len(pushes)-1]; last.notAfter.Before(time.Now()) {
		t.Errorf("the most recently pushed leaf (serial %s) expired %v ago and nothing has replaced it",
			last.serial, time.Since(last.notAfter).Round(time.Millisecond))
	}
}
