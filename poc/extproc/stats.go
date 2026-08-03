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

package extproc

import (
	"maps"
	"sync"
)

// Stats is a counter map the harness asserts against, so PoC results are
// measured rather than inferred from "the request succeeded".
//
// A mutex is sufficient here: the map is written once per ext_proc call, which
// is orders of magnitude cheaper than the gRPC round trip that carries it.
type Stats struct {
	mu sync.Mutex
	c  map[string]int64
}

func NewStats() *Stats { return &Stats{c: make(map[string]int64)} }

// Inc bumps one counter, ignoring nil so callers need no guard.
func (s *Stats) Inc(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c[name]++
}

// Get reads one counter.
func (s *Stats) Get(name string) int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c[name]
}

// Snapshot copies every counter.
func (s *Stats) Snapshot() map[string]int64 {
	if s == nil {
		return map[string]int64{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.c)
}
