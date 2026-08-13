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
	"sync"
	"time"

	"k8s.io/utils/lru"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

// certCache is a bounded LRU of minted leaves, each carrying the deadline past
// which it may no longer be served. Recency and capacity are k8s.io/utils/lru's
// problem; the deadline is this type's only addition, and it is enforced on
// read rather than swept, so an entry past its window is never handed out no
// matter how long it has sat there.
//
// certCache is safe for concurrent use. Minting, the one slow step, happens in
// the caller between a get that missed and a put, with no lock held.
type certCache struct {
	// mu makes the read-then-write pairs below atomic. lru.Cache locks each of
	// its own calls, which is not enough for get's "drop it if the deadline has
	// passed" or forget's "report whether anything was there".
	mu    sync.Mutex
	certs *lru.Cache
}

type cacheEntry struct {
	cert       *certauth.MintedCert
	reuseUntil time.Time
}

func newCertCache(capacity int) *certCache {
	// lru.New reads a non-positive size as "no limit", which would turn a
	// misconfiguration into an unbounded cache. newMinter does not allow one,
	// so this only has to fail safe rather than pick a sensible number.
	if capacity < 1 {
		capacity = 1
	}
	return &certCache{certs: lru.New(capacity)}
}

func (c *certCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.certs.Len()
}

// get returns the cached certificate for host if there is one that is still
// inside its reuse window, promoting it to most-recently-used.
//
// An entry found past its deadline is dropped rather than left in place: the
// caller is about to mint a replacement, and holding the dead one until then
// only costs a slot.
func (c *certCache) get(host string, now time.Time) (*certauth.MintedCert, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	v, ok := c.certs.Get(host)
	if !ok {
		return nil, false
	}
	e := v.(*cacheEntry)
	if !now.Before(e.reuseUntil) {
		c.certs.Remove(host)
		return nil, false
	}
	return e.cert, true
}

// put stores cert for host until reuseUntil, evicting the least recently used
// entry if that is what it takes to make room. A host already held is replaced
// in place, which is the path the rotation ticker takes.
func (c *certCache) put(host string, cert *certauth.MintedCert, reuseUntil time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.certs.Add(host, &cacheEntry{cert: cert, reuseUntil: reuseUntil})
}

// forget drops the entry for host if there is one. Unlike eviction it is not
// driven by pressure: the SDS server calls it when it withdraws a name from the
// data plane and when a stream closes, so the leaf stops being held on both
// sides at once.
func (c *certCache) forget(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.certs.Get(host); !ok {
		return false
	}
	c.certs.Remove(host)
	return true
}
