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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMinter(t *testing.T, opts MinterOptions) Minter {
	t.Helper()
	if opts.Validate == nil {
		opts.Validate = AllowGlobs([]string{"*.example"})
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	m, err := NewMinter(testCA(t), opts)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m
}

func TestMinterCachesByHost(t *testing.T) {
	m := testMinter(t, MinterOptions{TTL: time.Minute})
	ctx := context.Background()

	first, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("first GetCertificate: %v", err)
	}
	second, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("second GetCertificate: %v", err)
	}

	if first.Serial != second.Serial {
		t.Errorf("cache miss on the second call: serials %s != %s", first.Serial, second.Serial)
	}

	other, err := m.GetCertificate(ctx, "b.example")
	if err != nil {
		t.Fatalf("GetCertificate for a different host: %v", err)
	}
	if other.Serial == first.Serial {
		t.Error("different hosts were served the same certificate")
	}
}

func TestMinterRemintsAfterTTL(t *testing.T) {
	m := testMinter(t, MinterOptions{TTL: 40 * time.Millisecond})
	ctx := context.Background()

	first, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	second, err := m.GetCertificate(ctx, "a.example")
	if err != nil {
		t.Fatalf("GetCertificate after TTL: %v", err)
	}

	if first.Serial == second.Serial {
		t.Error("cached certificate was served after its TTL expired")
	}
}

func TestMinterEnforcesAllowlist(t *testing.T) {
	m := testMinter(t, MinterOptions{
		Validate: AllowGlobs([]string{"*.allowed.test"}),
		TTL:      time.Minute,
	})
	ctx := context.Background()

	if _, err := m.GetCertificate(ctx, "ok.allowed.test"); err != nil {
		t.Fatalf("allowed host was rejected: %v", err)
	}

	_, err := m.GetCertificate(ctx, "evil.test")
	if err == nil {
		t.Fatal("disallowed host was minted")
	}
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("error = %v, want it to wrap ErrHostNotAllowed", err)
	}
}

func TestMinterRequiresValidate(t *testing.T) {
	// A nil allowlist would make this an open impersonation oracle for every
	// host the CA is trusted for, so construction must fail loudly.
	if _, err := NewMinter(testCA(t), MinterOptions{TTL: time.Minute}); err == nil {
		t.Fatal("NewMinter accepted a nil Validate")
	}
}

func TestMinterEvictsToCap(t *testing.T) {
	m := testMinter(t, MinterOptions{
		Validate: AllowGlobs([]string{"*.example"}),
		TTL:      time.Minute,
		Cap:      4,
	})
	ctx := context.Background()

	for i := range 12 {
		if _, err := m.GetCertificate(ctx, fmt.Sprintf("h%d.example", i)); err != nil {
			t.Fatalf("GetCertificate: %v", err)
		}
	}

	impl := m.(*minter)
	impl.mu.Lock()
	size := impl.cache.len()
	impl.mu.Unlock()

	if size > 4 {
		t.Errorf("cache holds %d entries, want at most the cap of 4", size)
	}
}

func TestMinterIsConcurrencySafe(t *testing.T) {
	m := testMinter(t, MinterOptions{TTL: time.Minute, Cap: 8})
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.GetCertificate(ctx, fmt.Sprintf("h%d.example", i%16)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent GetCertificate: %v", err)
	}
}

func TestAllowGlobs(t *testing.T) {
	validate := AllowGlobs([]string{"*.example.com", "exact.test"})

	allowed := []string{"a.example.com", "exact.test"}
	for _, host := range allowed {
		if err := validate(host); err != nil {
			t.Errorf("validate(%q) = %v, want nil", host, err)
		}
	}

	denied := []string{
		"",                   // empty
		"example.com",        // the bare domain is not covered by *.example.com
		"a.b.example.com",    // one label per star
		"evil.test",          // no pattern
		"*.example.com",      // a wildcard in the SNI itself
		"a.example.com/../x", // path separator smuggling
		"..example.com",      // empty label
		".example.com",       // leading dot
		"a.example.com.",     // trailing dot
		"a.example.com\nfoo", // embedded newline
	}
	for _, host := range denied {
		if err := validate(host); err == nil {
			t.Errorf("validate(%q) = nil, want an error", host)
		}
	}
}

// TestAllowAny pins both halves of "any": every name AllowGlobs would have
// turned away for being outside the patterns is now accepted, and every name
// it turned away for not being a hostname at all still is. The second half is
// the one worth a test -- an implementation that returned nil unconditionally
// would pass the first.
func TestAllowAny(t *testing.T) {
	validate := AllowAny()

	allowed := []string{
		"example.com",
		"a.example.com",
		"a.b.c.d.example.com", // any depth, unlike a "*" label
		"EXAMPLE.COM",         // case is not a hostname's business
		"anything.test",       // any TLD
		"localhost",           // a single label is still a name
		"xn--80ak6aa92e.com",  // punycode
	}
	for _, host := range allowed {
		if err := validate(host); err != nil {
			t.Errorf("validate(%q) = %v, want nil", host, err)
		}
	}

	denied := []string{
		"",                   // empty
		"*.example.com",      // a wildcard in the SNI itself
		"a.example.com/../x", // path separator smuggling
		"..example.com",      // empty label
		".example.com",       // leading dot
		"a.example.com.",     // trailing dot
		"a.example.com\nfoo", // embedded newline
		strings.Repeat("a.", 200) + "example.com", // over 253 bytes
	}
	for _, host := range denied {
		if err := validate(host); err == nil {
			t.Errorf("validate(%q) = nil, want an error", host)
		}
	}
}
