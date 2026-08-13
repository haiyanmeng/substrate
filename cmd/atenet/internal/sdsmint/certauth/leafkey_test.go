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

package certauth

import (
	"sync"
	"testing"
	"time"
)

func TestLeafKeysHoldsOneKeyUntilItRotates(t *testing.T) {
	start := time.Now()
	keys, err := newLeafKeys(time.Hour, start)
	if err != nil {
		t.Fatalf("newLeafKeys: %v", err)
	}

	first, err := keys.at(start)
	if err != nil {
		t.Fatalf("at(start): %v", err)
	}
	// Just short of the period, and at several points inside it: the whole
	// point of the shared key is that these are all the same keypair.
	for _, elapsed := range []time.Duration{0, time.Second, 30 * time.Minute, time.Hour - time.Nanosecond} {
		got, err := keys.at(start.Add(elapsed))
		if err != nil {
			t.Fatalf("at(start+%v): %v", elapsed, err)
		}
		if got != first {
			t.Errorf("at(start+%v) returned a new key before the rotation period elapsed", elapsed)
		}
	}
}

func TestLeafKeysRotatesAtThePeriod(t *testing.T) {
	start := time.Now()
	keys, err := newLeafKeys(time.Hour, start)
	if err != nil {
		t.Fatalf("newLeafKeys: %v", err)
	}

	first, err := keys.at(start)
	if err != nil {
		t.Fatalf("at(start): %v", err)
	}
	// Exactly at the period, not past it: at uses a strict <, so the boundary
	// itself rotates. Pinned because "expires after an hour" and "expires on
	// the hour" differ by a whole period if the comparison drifts.
	second, err := keys.at(start.Add(time.Hour))
	if err != nil {
		t.Fatalf("at(start+1h): %v", err)
	}
	if second == first {
		t.Fatal("at did not rotate the key at the rotation period")
	}
	if string(second.pem) == string(first.pem) {
		t.Error("the rotated key serializes identically to the one it replaced")
	}

	// And the clock restarts from the rotation, rather than from construction.
	third, err := keys.at(start.Add(time.Hour + 30*time.Minute))
	if err != nil {
		t.Fatalf("at(start+1h30m): %v", err)
	}
	if third != second {
		t.Error("at rotated again half a period after the previous rotation")
	}
}

func TestNewLeafKeysRejectsANonPositivePeriod(t *testing.T) {
	for _, period := range []time.Duration{0, -time.Second} {
		if _, err := newLeafKeys(period, time.Now()); err == nil {
			t.Errorf("newLeafKeys(%v) succeeded, want an error", period)
		}
	}
}

func TestLeafKeysIsSafeForConcurrentUse(t *testing.T) {
	start := time.Now()
	keys, err := newLeafKeys(time.Hour, start)
	if err != nil {
		t.Fatalf("newLeafKeys: %v", err)
	}

	// Sign is called from every handshake at once, so at is too. Meaningful
	// under -race; a plain run only checks that nothing panics.
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half of these land past the rotation period, so the racing
			// callers are contending over a replacement, not just a read.
			if _, err := keys.at(start.Add(time.Duration(i) * 4 * time.Minute)); err != nil {
				t.Errorf("at: %v", err)
			}
		}(i)
	}
	wg.Wait()
}
