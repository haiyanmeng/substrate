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
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

// certFor builds a recognizable stand-in. The cache never looks inside a
// MintedCert, so the serial is enough to tell entries apart.
func certFor(host string) *certauth.MintedCert {
	return &certauth.MintedCert{Serial: host}
}

func TestCertCacheHitAndMiss(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	c.put("a.example", certFor("a"), now.Add(time.Minute))

	got, ok := c.get("a.example", now)
	if !ok || got.Serial != "a" {
		t.Fatalf("get(a.example) = %v, %v; want the cached cert", got, ok)
	}
	if _, ok := c.get("b.example", now); ok {
		t.Fatal("get(b.example) reported a hit for a host never put")
	}
}

func TestCertCacheDropsEntriesPastTheirDeadline(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	c.put("a.example", certFor("a"), now.Add(time.Minute))

	// Exactly at the deadline counts as past it: reuseUntil is exclusive.
	if _, ok := c.get("a.example", now.Add(time.Minute)); ok {
		t.Fatal("a leaf was served at its reuse deadline")
	}
	if c.len() != 0 {
		t.Fatalf("cache holds %d entries after the dead one was read; want 0", c.len())
	}
}

// A dead entry is only reclaimed when something reads it, so one that is never
// asked for again occupies a slot until eviction. That is tolerable precisely
// because it can never be served, which is what this pins.
func TestCertCacheNeverServesAnEntryPastItsDeadline(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	c.put("a.example", certFor("a"), now.Add(time.Minute))

	for _, elapsed := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour} {
		if _, ok := c.get("a.example", now.Add(elapsed)); ok {
			t.Errorf("a leaf was served %v past its reuse deadline", elapsed)
		}
	}
}

func TestCertCacheEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Now()
	c := newCertCache(3)
	for _, h := range []string{"a", "b", "c"} {
		c.put(h, certFor(h), now.Add(time.Minute))
	}

	// Touch "a" so "b" becomes the least recently used, then overflow.
	if _, ok := c.get("a", now); !ok {
		t.Fatal("a should still be cached")
	}
	c.put("d", certFor("d"), now.Add(time.Minute))

	if _, ok := c.get("b", now); ok {
		t.Error("b was the least recently used and should have been evicted")
	}
	for _, h := range []string{"a", "c", "d"} {
		if _, ok := c.get(h, now); !ok {
			t.Errorf("%s should have survived eviction", h)
		}
	}
}

func TestCertCacheHoldsToItsCapacity(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	for i := range 100 {
		c.put(fmt.Sprintf("h%d.example", i), certFor("h"), now.Add(time.Minute))
		if got := c.len(); got > 4 {
			t.Fatalf("cache grew to %d entries after %d puts; the cap is 4", got, i+1)
		}
	}
	if got := c.len(); got != 4 {
		t.Errorf("cache holds %d entries after overflowing; want it full at 4", got)
	}
}

func TestCertCacheReplacesInPlace(t *testing.T) {
	now := time.Now()
	c := newCertCache(2)
	c.put("a", certFor("first"), now.Add(time.Minute))
	c.put("b", certFor("b"), now.Add(time.Minute))

	// A re-mint for a host already held must overwrite rather than consume a
	// second slot -- this is the path the rotation ticker takes.
	c.put("a", certFor("second"), now.Add(time.Hour))

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
	// And b must not have been pushed out to make room for a host already held.
	if _, ok := c.get("b", now); !ok {
		t.Error("b was evicted by a replacement that needed no new slot")
	}
}

func TestCertCacheForget(t *testing.T) {
	now := time.Now()
	c := newCertCache(4)
	c.put("a", certFor("a"), now.Add(time.Minute))

	if !c.forget("a") {
		t.Error("forget reported nothing held for a host that was just put")
	}
	if c.forget("a") {
		t.Error("forget reported a hit on a host it had already released")
	}
	if _, ok := c.get("a", now); ok {
		t.Error("a is still cached after forget")
	}
}
