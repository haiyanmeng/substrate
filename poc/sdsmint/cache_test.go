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
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// certFor builds a recognisable stand-in. The cache never looks inside a
// MintedCert, so the serial is enough to tell entries apart.
func certFor(host string) *MintedCert {
	return &MintedCert{Serial: host}
}

// checkInvariants asserts the three structures agree with each other. Any bug
// in the list or heap bookkeeping shows up here rather than as a mysterious
// eviction much later.
func checkInvariants(t *testing.T, c *certCache) {
	t.Helper()

	// Walking forward from the head must reach every entry exactly once and
	// arrive at the tail sentinel.
	seen := make(map[string]bool, len(c.byHost))
	n := 0
	for e := c.head.next; e != &c.tail; e = e.next {
		if e == nil {
			t.Fatalf("recency list ran off the end after %d entries", n)
		}
		if n++; n > len(c.byHost)+1 {
			t.Fatalf("recency list has a cycle or extra entries (walked %d, cache holds %d)", n, len(c.byHost))
		}
		if seen[e.host] {
			t.Fatalf("host %q appears twice in the recency list", e.host)
		}
		seen[e.host] = true
		if c.byHost[e.host] != e {
			t.Fatalf("host %q is in the recency list but byHost points elsewhere", e.host)
		}
		if e.next.prev != e {
			t.Fatalf("host %q: next.prev does not point back", e.host)
		}
	}
	if n != len(c.byHost) {
		t.Fatalf("recency list holds %d entries, byHost holds %d", n, len(c.byHost))
	}

	if len(c.expiry) != len(c.byHost) {
		t.Fatalf("expiry heap holds %d entries, byHost holds %d", len(c.expiry), len(c.byHost))
	}
	for i, e := range c.expiry {
		if e.heapIndex != i {
			t.Fatalf("host %q sits at heap index %d but records %d", e.host, i, e.heapIndex)
		}
		if c.byHost[e.host] != e {
			t.Fatalf("host %q is in the heap but byHost points elsewhere", e.host)
		}
		// Min-heap property.
		if i > 0 {
			parent := c.expiry[(i-1)/2]
			if e.reuseUntil.Before(parent.reuseUntil) {
				t.Fatalf("heap order violated at index %d: %v before parent %v", i, e.reuseUntil, parent.reuseUntil)
			}
		}
	}

	if len(c.byHost) > c.capacity {
		t.Fatalf("cache holds %d entries, over its capacity of %d", len(c.byHost), c.capacity)
	}
}

func TestCertCacheHitAndMiss(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	c.put("a.example", certFor("a"), now, now.Add(time.Minute))

	got, ok := c.get("a.example", now)
	if !ok || got.Serial != "a" {
		t.Fatalf("get(a.example) = %v, %v; want the cached cert", got, ok)
	}
	if _, ok := c.get("b.example", now); ok {
		t.Fatal("get(b.example) reported a hit for a host never put")
	}
	checkInvariants(t, c)
}

func TestCertCacheDropsEntriesPastTheirDeadline(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	c.put("a.example", certFor("a"), now, now.Add(time.Minute))

	// Exactly at the deadline counts as past it: reuseUntil is exclusive.
	if _, ok := c.get("a.example", now.Add(time.Minute)); ok {
		t.Fatal("a leaf was served at its reuse deadline")
	}
	if c.len() != 0 {
		t.Fatalf("cache holds %d entries after the dead one was read; want 0", c.len())
	}
	checkInvariants(t, c)
}

func TestCertCacheEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Now()
	c := newCertCache(3)
	for _, h := range []string{"a", "b", "c"} {
		c.put(h, certFor(h), now, now.Add(time.Minute))
	}

	// Touch "a" so "b" becomes the least recently used, then overflow.
	if _, ok := c.get("a", now); !ok {
		t.Fatal("a should still be cached")
	}
	c.put("d", certFor("d"), now, now.Add(time.Minute))

	if _, ok := c.get("b", now); ok {
		t.Error("b was the least recently used and should have been evicted")
	}
	for _, h := range []string{"a", "c", "d"} {
		if _, ok := c.get(h, now); !ok {
			t.Errorf("%s should have survived eviction", h)
		}
	}
	checkInvariants(t, c)
}

// A dead entry must be reclaimed before a live one is evicted to make room,
// or a burst of short-lived hosts would push out hosts that are still usable.
func TestCertCachePrefersReclaimingDeadEntries(t *testing.T) {
	now := time.Now()
	c := newCertCache(2)
	c.put("dead", certFor("dead"), now, now.Add(time.Second))
	c.put("live", certFor("live"), now, now.Add(time.Hour))

	later := now.Add(2 * time.Second)
	c.put("new", certFor("new"), later, later.Add(time.Hour))

	if _, ok := c.get("live", later); !ok {
		t.Error("live was evicted even though a dead entry was available to reclaim")
	}
	if _, ok := c.get("dead", later); ok {
		t.Error("dead is still cached")
	}
	checkInvariants(t, c)
}

func TestCertCacheReplacesInPlace(t *testing.T) {
	now := time.Now()
	c := newCertCache(2)
	c.put("a", certFor("first"), now, now.Add(time.Minute))
	c.put("b", certFor("b"), now, now.Add(time.Minute))

	// A re-mint for a host already held must overwrite rather than consume a
	// second slot -- this is the path the rotation ticker takes.
	c.put("a", certFor("second"), now, now.Add(time.Hour))

	if c.len() != 2 {
		t.Fatalf("cache holds %d entries after re-minting a held host; want 2", c.len())
	}
	got, ok := c.get("a", now)
	if !ok || got.Serial != "second" {
		t.Fatalf("get(a) = %v, %v; want the replacement", got, ok)
	}
	// The replacement carried a later deadline, so it must still be live well
	// past the original one.
	if _, ok := c.get("a", now.Add(30*time.Minute)); !ok {
		t.Error("the replacement's deadline was not applied")
	}
	checkInvariants(t, c)
}

func TestCertCachePurgeExpiredStopsAtTheFirstLiveEntry(t *testing.T) {
	now := time.Now()
	c := newCertCache(8)
	for i := 0; i < 4; i++ {
		c.put(fmt.Sprintf("dead%d", i), certFor("dead"), now, now.Add(time.Duration(i+1)*time.Second))
	}
	c.put("live", certFor("live"), now, now.Add(time.Hour))

	c.purgeExpired(now.Add(10 * time.Second))

	if c.len() != 1 {
		t.Fatalf("cache holds %d entries after purge; want just the live one", c.len())
	}
	if _, ok := c.get("live", now.Add(10*time.Second)); !ok {
		t.Error("purge took the live entry too")
	}
	checkInvariants(t, c)
}

// A put reclaims at most purgeBudget dead entries so it cannot stall behind a
// cache that expired all at once. The rest have to come back on later puts,
// and the cap must hold throughout.
func TestCertCachePurgeIsBoundedPerPutButEventuallyComplete(t *testing.T) {
	const capacity = purgeBudget * 4
	now := time.Now()
	c := newCertCache(capacity)
	for i := 0; i < capacity; i++ {
		c.put(fmt.Sprintf("dead%d", i), certFor("dead"), now, now.Add(time.Second))
	}

	later := now.Add(time.Minute)
	c.put("first", certFor("first"), later, later.Add(time.Hour))
	if got := c.len(); got <= capacity-purgeBudget-1 {
		t.Fatalf("one put reclaimed more than its budget: cache went to %d entries", got)
	}

	// Keep putting the same live host, which does not itself consume slots,
	// until the dead ones are gone.
	for i := 0; i < capacity; i++ {
		c.put("first", certFor("first"), later, later.Add(time.Hour))
		checkInvariants(t, c)
		if c.len() == 1 {
			return
		}
	}
	t.Fatalf("dead entries were never fully reclaimed; %d still cached", c.len())
}

// TestCertCacheRandomOps hammers every path in a mixed order and checks the
// invariants after each step. The list and the heap are maintained by hand, so
// the failure mode to guard against is not a wrong answer but a structure that
// quietly drifts out of agreement with the map.
func TestCertCacheRandomOps(t *testing.T) {
	const capacity = 16
	rng := rand.New(rand.NewSource(1))
	c := newCertCache(capacity)
	now := time.Now()

	for i := 0; i < 3000; i++ {
		host := fmt.Sprintf("h%d", rng.Intn(capacity*3))
		switch rng.Intn(3) {
		case 0:
			c.get(host, now)
		case 1:
			// Deadlines land on both sides of the clock, so entries die at
			// unpredictable points relative to each other.
			offset := time.Duration(rng.Intn(2000)-500) * time.Millisecond
			c.put(host, certFor(host), now, now.Add(offset))
		case 2:
			now = now.Add(time.Duration(rng.Intn(300)) * time.Millisecond)
			c.purgeExpired(now)
		}
		checkInvariants(t, c)
	}
}
