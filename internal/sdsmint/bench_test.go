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

// Benchmarks for the two questions the design cannot answer on paper:
//
//	S0  where does the time in a mint actually go?
//	S1  does the cache get slower as it gets bigger?
//
// S1 exists because evictLocked walks the whole map on every miss, under the
// exclusive lock. If that dominates, raising --cache-cap to hold more hosts
// makes the server slower rather than faster, which is the opposite of what
// an operator would expect the flag to do.
//
//	go test ./internal/sdsmint/ -run XXX -bench . -benchmem

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func benchCA(b *testing.B) *CA {
	b.Helper()
	// Unconstrained and root-signing on purpose: this is the configuration the
	// numbers recorded in README.md were measured under, and a name constraint
	// or a delegated intermediate would change what is being compared.
	ca, _, err := GenerateCA("sdsmint bench CA", time.Hour, nil, Options{AllowUnconstrained: true})
	if err != nil {
		b.Fatalf("GenerateCA: %v", err)
	}
	return ca
}

func benchMinter(b *testing.B, opts MinterOptions) *minter {
	b.Helper()
	if opts.Validate == nil {
		opts.Validate = AllowGlobs([]string{"*.example"})
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	if opts.TTL == 0 {
		opts.TTL = time.Hour
	}
	m, err := NewMinter(benchCA(b), opts)
	if err != nil {
		b.Fatalf("NewMinter: %v", err)
	}
	return m.(*minter)
}

// ---------------------------------------------------------------------------
// S0: cost decomposition
//
// Sign is the sum of these parts. Benchmarking them separately says whether
// optimisation effort belongs in key generation (in which case: pre-generate a
// pool), in the signature (in which case: nothing to do, it is one P-256
// operation), or in the encoding (in which case: cache the PEM).
// ---------------------------------------------------------------------------

func BenchmarkSign(b *testing.B) {
	ca := benchCA(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ca.Sign("a.example", 5*time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSignKeygen isolates the fresh-keypair-per-leaf decision. A leaf key
// is generated for every mint so that no two hosts share one; if this is the
// bulk of Sign, a pre-generated key pool is the obvious lever.
func BenchmarkSignKeygen(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSignCreateCertificate is the CA signature itself, with the leaf key
// already in hand. This is the irreducible part.
func BenchmarkSignCreateCertificate(b *testing.B) {
	ca := benchCA(b)
	iss := ca.current.Load()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		DNSNames:              []string{"a.example"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(5 * time.Minute),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := x509.CreateCertificate(rand.Reader, tmpl, iss.cert, key.Public(), iss.key); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSignEncode covers everything Sign does after the signature: PKCS#8
// marshalling of the leaf key and PEM-encoding both halves. Cheap in theory,
// but it is per-mint and it allocates.
func BenchmarkSignEncode(b *testing.B) {
	ca := benchCA(b)
	iss := ca.current.Load()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial,
		DNSNames:     []string{"a.example"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(5 * time.Minute),
	}, iss.cert, key.Public(), iss.key)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
		for _, der := range iss.chainDER {
			chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			b.Fatal(err)
		}
		_ = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
}

// BenchmarkMinterCacheHit is the warm path: what a repeat request for a host
// already in cache costs. This is the number that matters for steady state,
// since on-demand SDS asks once per host and then holds the subscription.
func BenchmarkMinterCacheHit(b *testing.B) {
	m := benchMinter(b, MinterOptions{})
	ctx := context.Background()
	if _, err := m.GetCertificate(ctx, "a.example"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.GetCertificate(ctx, "a.example"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMinterCacheHitParallel runs the warm path across GOMAXPROCS
// goroutines against one host. Every hit takes the exclusive mutex to stamp
// lastUsed, so this shows what that costs when many workers share a hot name.
func BenchmarkMinterCacheHitParallel(b *testing.B) {
	m := benchMinter(b, MinterOptions{})
	ctx := context.Background()
	if _, err := m.GetCertificate(ctx, "a.example"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := m.GetCertificate(ctx, "a.example"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkAllowGlobs is the allowlist check, which runs before the cache
// lookup on every single request including hits. Split by outcome because a
// miss walks every pattern while a hit can stop early.
func BenchmarkAllowGlobs(b *testing.B) {
	patterns := make([]string, 0, 32)
	for i := 0; i < 31; i++ {
		patterns = append(patterns, fmt.Sprintf("*.tenant%d.example", i))
	}
	patterns = append(patterns, "*.match.example")
	validate := AllowGlobs(patterns)

	b.Run("match_last_of_32", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := validate("host.match.example"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("reject_after_32", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := validate("host.nomatch.example"); err == nil {
				b.Fatal("expected rejection")
			}
		}
	})
}

// BenchmarkPackSecret is the per-push serialisation cost: build the Secret
// proto, wrap it in an Any, and marshal. Rotation pays this once per live
// subscription per tick, so at a few thousand live secrets it is the shape of
// the rotation storm.
func BenchmarkPackSecret(b *testing.B) {
	ca := benchCA(b)
	cert, err := ca.Sign("a.example", 5*time.Minute)
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	var size int
	for i := 0; i < b.N; i++ {
		body, err := anypb.New(toSecret("a.example", cert))
		if err != nil {
			b.Fatal(err)
		}
		size = proto.Size(body)
	}
	b.StopTimer()
	// The absolute number is the interesting output: multiply by the live
	// secret count to find where a rotation response crosses Envoy's 4MB
	// default gRPC receive limit.
	b.ReportMetric(float64(size), "wire_bytes")
}

// ---------------------------------------------------------------------------
// S1: does a bigger cache cost more?
//
// It used to. The map-only cache scanned everything twice per miss with the
// lock held -- once for expired entries, once for the LRU victim -- so a miss
// cost 441us at cap 256 and 13.3ms at cap 100k, and concurrent misses stopped
// scaling by cap 10k. certCache replaced both scans with a recency list and an
// expiry heap. These benchmarks are what measured the problem; they stay as
// the regression guard, and the numbers across caps should now be flat.
// ---------------------------------------------------------------------------

var benchCaps = []int{256, 1000, 10_000, 100_000}

// fillCache seeds n live entries directly, skipping the signing that would
// otherwise dominate setup. The certificate is shared; only the shape of the
// cache matters for what eviction costs.
func fillCache(b *testing.B, m *minter, n int) {
	b.Helper()
	cert, err := m.ca.Sign("seed.example", time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		// Stagger the deadlines so the expiry heap is a real heap rather than
		// n identical keys, and insert oldest-first so recency order is not
		// degenerate either.
		m.cache.put(fmt.Sprintf("seed%d.example", i), cert, now,
			now.Add(time.Hour+time.Duration(i)*time.Millisecond))
	}
}

// BenchmarkMinterMissAtCap is the honest end-to-end number: a cache miss for a
// never-seen host with the cache already full. It includes the P-256 sign, so
// if the eviction scans are noise here they are noise in production too.
func BenchmarkMinterMissAtCap(b *testing.B) {
	ctx := context.Background()
	for _, capacity := range benchCaps {
		b.Run(fmt.Sprintf("cap=%d", capacity), func(b *testing.B) {
			m := benchMinter(b, MinterOptions{Cap: capacity})
			fillCache(b, m, capacity)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// A distinct host every iteration, so every one is a miss.
				if _, err := m.GetCertificate(ctx, fmt.Sprintf("miss%d.example", i)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCacheInsertAtCap strips the signing away and times only the work a
// miss does under the lock: evict a victim, insert a replacement, maintain
// both orderings. This is where an O(n) term shows up clearly -- the
// end-to-end benchmark above can hide one behind a fixed ~375us sign.
func BenchmarkCacheInsertAtCap(b *testing.B) {
	for _, capacity := range benchCaps {
		b.Run(fmt.Sprintf("cap=%d", capacity), func(b *testing.B) {
			m := benchMinter(b, MinterOptions{Cap: capacity})
			fillCache(b, m, capacity)
			cert, err := m.ca.Sign("probe.example", time.Hour)
			if err != nil {
				b.Fatal(err)
			}
			now := time.Now()
			// Pre-render the keys: fmt.Sprintf costs more than the insert
			// being measured once the insert is O(1).
			keys := make([]string, b.N)
			for i := range keys {
				keys[i] = fmt.Sprintf("insert%d.example", i)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// The cache is full, so every insert evicts. A distinct host
				// each time keeps it that way.
				m.mu.Lock()
				m.cache.put(keys[i], cert, now, now.Add(time.Hour))
				m.mu.Unlock()
			}
		})
	}
}

// BenchmarkCachePurgeExpired times giving memory back: a full cache whose
// entries have all passed their reuse deadline, emptied in one call. The heap
// makes this O(n log n) overall rather than O(n) per removal, and it is the
// path an idle sdsmintd takes after a traffic burst.
func BenchmarkCachePurgeExpired(b *testing.B) {
	for _, capacity := range benchCaps {
		b.Run(fmt.Sprintf("cap=%d", capacity), func(b *testing.B) {
			m := benchMinter(b, MinterOptions{Cap: capacity})
			cert, err := m.ca.Sign("probe.example", time.Hour)
			if err != nil {
				b.Fatal(err)
			}
			past := time.Now().Add(-time.Hour)
			keys := make([]string, capacity)
			for i := range keys {
				keys[i] = fmt.Sprintf("dead%d.example", i)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				for j, k := range keys {
					m.cache.put(k, cert, past, past.Add(time.Duration(j)*time.Millisecond))
				}
				b.StartTimer()

				m.cache.purgeExpired(time.Now())
				if m.cache.len() != 0 {
					b.Fatalf("cache still holds %d entries", m.cache.len())
				}
			}
			b.ReportMetric(float64(capacity), "entries/op")
		})
	}
}

// BenchmarkMinterMissParallel is the contention question S1 really cares
// about: concurrent handshakes for distinct cold hosts. Signing happens
// outside the lock, so the ceiling is set by how long each miss holds m.mu.
// That used to be the eviction scan, and cap=10000 barely scaled at all.
func BenchmarkMinterMissParallel(b *testing.B) {
	ctx := context.Background()
	for _, capacity := range []int{256, 10_000} {
		b.Run(fmt.Sprintf("cap=%d", capacity), func(b *testing.B) {
			m := benchMinter(b, MinterOptions{Cap: capacity})
			fillCache(b, m, capacity)

			var counter atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					host := fmt.Sprintf("miss%d.example", counter.Add(1))
					if _, err := m.GetCertificate(ctx, host); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
