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
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"testing"
	"time"
)

func testNullMinter(t *testing.T, opts NullMinterOptions) *NullMinter {
	t.Helper()
	if opts.Validate == nil {
		opts.Validate = AllowGlobs([]string{"*.mitm.example"})
	}
	if opts.Host == "" {
		opts.Host = "*.mitm.example"
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	n, err := NewNullMinter(testCA(t), opts)
	if err != nil {
		t.Fatalf("NewNullMinter: %v", err)
	}
	return n
}

// The point of the null minter is that nothing is signed once it is running,
// so the pool has to be exhaustively reused rather than topped up.
func TestNullMinterNeverSignsAfterConstruction(t *testing.T) {
	const pool = 4
	n := testNullMinter(t, NullMinterOptions{Pool: pool})
	ctx := context.Background()

	seen := make(map[string]int)
	for i := range pool * 10 {
		cert, err := n.GetCertificate(ctx, fmt.Sprintf("h%d.mitm.example", i))
		if err != nil {
			t.Fatalf("GetCertificate: %v", err)
		}
		seen[cert.Serial]++
	}
	if len(seen) != pool {
		t.Fatalf("saw %d distinct certificates over %d calls; want exactly the pool of %d", len(seen), pool*10, pool)
	}
}

// Rotation pushes are versioned by serial. If consecutive calls for one host
// returned the same certificate, every rotation would carry the version Envoy
// already holds and the rotation phases would measure nothing at all.
func TestNullMinterChangesVersionBetweenCalls(t *testing.T) {
	n := testNullMinter(t, NullMinterOptions{Pool: 2})
	ctx := context.Background()

	first, err := n.GetCertificate(ctx, "a.mitm.example")
	if err != nil {
		t.Fatalf("first GetCertificate: %v", err)
	}
	second, err := n.GetCertificate(ctx, "a.mitm.example")
	if err != nil {
		t.Fatalf("second GetCertificate: %v", err)
	}
	if first.Serial == second.Serial {
		t.Error("two consecutive calls returned the same serial, so a rotation push would be a no-op")
	}
}

// The synthetic host set is walked by name, so the wildcard leaf has to
// actually verify for those names or the load generator measures TLS failures.
func TestNullMinterLeafCoversTheSyntheticHosts(t *testing.T) {
	n := testNullMinter(t, NullMinterOptions{Pool: 1})

	block, _ := pem.Decode(n.Sample().CertChainPEM)
	if block == nil {
		t.Fatal("pre-signed leaf is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the pre-signed leaf: %v", err)
	}
	if err := leaf.VerifyHostname("h12345.mitm.example"); err != nil {
		t.Errorf("leaf does not cover a synthetic host: %v", err)
	}
	if err := leaf.VerifyHostname("elsewhere.example"); err == nil {
		t.Error("leaf covers a host outside the wildcard")
	}
}

func TestNullMinterStillEnforcesTheAllowlist(t *testing.T) {
	n := testNullMinter(t, NullMinterOptions{Validate: AllowGlobs([]string{"*.mitm.example"})})

	_, err := n.GetCertificate(context.Background(), "evil.test")
	if err == nil {
		t.Fatal("a disallowed host was served a pre-signed leaf")
	}
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("error = %v, want it to wrap ErrHostNotAllowed", err)
	}
}

func TestNullMinterRequiresValidateAndHost(t *testing.T) {
	if _, err := NewNullMinter(testCA(t), NullMinterOptions{Host: "*.mitm.example"}); err == nil {
		t.Error("NewNullMinter accepted a nil Validate")
	}
	if _, err := NewNullMinter(testCA(t), NullMinterOptions{Validate: AllowGlobs([]string{"*.x"})}); err == nil {
		t.Error("NewNullMinter accepted an empty Host")
	}
}

// The pool TTL is deliberately independent of the server's rotation TTL: the
// rotation phases run a short rotation period against long-lived leaves,
// because what is being measured is the push storm and not expiry.
func TestNullMinterPoolTTLIsIndependent(t *testing.T) {
	n := testNullMinter(t, NullMinterOptions{Pool: 1, TTL: 48 * time.Hour})
	if got := time.Until(n.Sample().NotAfter); got < 47*time.Hour {
		t.Errorf("pre-signed leaf expires in %v; want the requested 48h", got)
	}
}
