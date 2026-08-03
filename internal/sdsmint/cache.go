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
	"container/heap"
	"time"
)

// certCache is a bounded LRU of minted leaves with a reuse deadline per entry.
//
// It exists because the obvious map-only version is O(size) per insert: one
// scan to drop entries past their deadline, another to find the least-recently
// used victim. That runs with the minter's lock held, which made a large
// --cache-cap actively harmful -- a miss cost 441us at cap 256 and 13.3ms at
// cap 100k, and concurrent misses stopped scaling entirely by cap 10k because
// signing happens outside the lock but the scan does not.
//
// Every operation here is O(1) plus at most O(log n) of heap work:
//
//	byHost   locates an entry
//	the list orders entries by recency, so the victim is just the tail
//	expiry   orders entries by deadline, so dead entries are found at the root
//
// certCache is not safe for concurrent use; the minter serializes access.
type certCache struct {
	capacity int
	byHost   map[string]*cacheEntry

	// Sentinel ends of the recency list. head.next is the most recently used
	// entry, tail.prev the least. Sentinels mean insert and unlink never have
	// to special-case an empty list or the ends.
	head, tail cacheEntry

	// expiry is a min-heap on reuseUntil. Its only job is to let an idle
	// cache give memory back: entries past their deadline can never be
	// returned by get, and without this they would sit there holding a
	// certificate each until enough new hosts arrived to push them out.
	expiry expiryHeap
}

type cacheEntry struct {
	host       string
	cert       *MintedCert
	reuseUntil time.Time

	// Recency list links. Valid only while the entry is in byHost.
	prev, next *cacheEntry

	// Position in expiry, maintained by the heap interface below so an entry
	// can be removed from the middle in O(log n) when it is evicted or
	// replaced. -1 once the entry has left the heap.
	heapIndex int
}

func newCertCache(capacity int) *certCache {
	c := &certCache{
		capacity: capacity,
		byHost:   make(map[string]*cacheEntry),
	}
	c.head.next = &c.tail
	c.tail.prev = &c.head
	return c
}

func (c *certCache) len() int { return len(c.byHost) }

// get returns the cached certificate for host if there is one that is still
// inside its reuse window, promoting it to most-recently-used.
//
// An entry found past its deadline is dropped rather than left in place: the
// caller is about to mint a replacement, and holding the dead one until then
// only costs a slot.
func (c *certCache) get(host string, now time.Time) (*MintedCert, bool) {
	e, ok := c.byHost[host]
	if !ok {
		return nil, false
	}
	if !now.Before(e.reuseUntil) {
		c.remove(e)
		return nil, false
	}
	c.unlink(e)
	c.pushFront(e)
	return e.cert, true
}

// purgeBudget bounds how many dead entries one put reclaims.
//
// Reclaiming is O(log n) per entry but memory-bound in practice -- measured at
// roughly 0.9us each once the cache is large enough to miss cache on every
// heap hop. Without a bound, a cache where everything expired at once would
// hand the next put an 86ms pause with the minter's lock held (measured at cap
// 100k). Anything not reclaimed now is reclaimed by the following puts, and
// making room never depends on it: eviction by recency is O(1) and always
// available.
const purgeBudget = 64

// put stores cert for host, first discarding anything already past its
// deadline and then making room by evicting the least recently used entry.
func (c *certCache) put(host string, cert *MintedCert, now, reuseUntil time.Time) {
	c.purgeExpiredN(now, purgeBudget)

	if e, ok := c.byHost[host]; ok {
		// A re-mint for a host we already hold. Reuse the node so the
		// replacement keeps its identity in both structures.
		e.cert = cert
		e.reuseUntil = reuseUntil
		heap.Fix(&c.expiry, e.heapIndex)
		c.unlink(e)
		c.pushFront(e)
		return
	}

	for len(c.byHost) >= c.capacity {
		lru := c.tail.prev
		if lru == &c.head {
			// capacity is < 1, which NewMinter does not allow. Nothing to
			// evict, so refuse to grow rather than looping forever.
			return
		}
		c.remove(lru)
	}

	e := &cacheEntry{host: host, cert: cert, reuseUntil: reuseUntil, heapIndex: -1}
	c.byHost[host] = e
	c.pushFront(e)
	heap.Push(&c.expiry, e)
}

// forget drops the entry for host if there is one. Unlike eviction it is not
// driven by pressure: the SDS server calls it when it withdraws a name from
// the data plane, so the leaf stops being held on both sides at once.
func (c *certCache) forget(host string) bool {
	e, ok := c.byHost[host]
	if !ok {
		return false
	}
	c.remove(e)
	return true
}

// purgeExpired drops every entry whose reuse window has closed. The heap root
// is always the nearest deadline, so this stops at the first live entry
// instead of walking the whole cache.
func (c *certCache) purgeExpired(now time.Time) { c.purgeExpiredN(now, -1) }

// purgeExpiredN is purgeExpired stopping after limit removals. A negative
// limit means no limit.
func (c *certCache) purgeExpiredN(now time.Time, limit int) {
	for n := 0; len(c.expiry) > 0 && !now.Before(c.expiry[0].reuseUntil); n++ {
		if limit >= 0 && n >= limit {
			return
		}
		c.remove(c.expiry[0])
	}
}

func (c *certCache) remove(e *cacheEntry) {
	delete(c.byHost, e.host)
	c.unlink(e)
	if e.heapIndex >= 0 {
		heap.Remove(&c.expiry, e.heapIndex)
		e.heapIndex = -1
	}
}

func (c *certCache) unlink(e *cacheEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	e.prev, e.next = nil, nil
}

func (c *certCache) pushFront(e *cacheEntry) {
	e.prev = &c.head
	e.next = c.head.next
	c.head.next.prev = e
	c.head.next = e
}

// expiryHeap is a min-heap of entries by reuseUntil. It writes heapIndex back
// on every move, which is what makes removal from the middle possible.
type expiryHeap []*cacheEntry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].reuseUntil.Before(h[j].reuseUntil) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].heapIndex = i; h[j].heapIndex = j }
func (h *expiryHeap) Push(x any)        { e := x.(*cacheEntry); e.heapIndex = len(*h); *h = append(*h, e) }
func (h *expiryHeap) Pop() (x any) {
	old := *h
	n := len(old)
	x = old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}
