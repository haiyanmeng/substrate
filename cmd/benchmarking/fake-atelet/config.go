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

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"k8s.io/apimachinery/pkg/api/resource"
)

// parseCapacity builds the WorkerResources every fake Worker reports, from an
// actor count and a "cpu=32,memory=128Gi" list, enforcing the rules
// ate-api-server applies to Limits: only cpu and memory, each at most once,
// each greater than zero, and cpu under 1000 cores.
func parseCapacity(actors int, list string) (*ateapipb.WorkerResources, error) {
	if actors < 1 {
		return nil, fmt.Errorf("actor capacity must be at least 1, got %d", actors)
	}
	out := &ateapipb.WorkerResources{Actors: int32(actors)}
	if list == "" {
		return out, nil
	}
	seen := map[string]bool{}
	var limits []*ateapipb.Limits
	for _, entry := range strings.Split(list, ",") {
		name, quantity, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			return nil, fmt.Errorf("resource %q is not name=quantity", entry)
		}
		if name != "cpu" && name != "memory" {
			return nil, fmt.Errorf("resource %q: only cpu and memory are supported", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("resource %q given more than once", name)
		}
		seen[name] = true
		q, err := resource.ParseQuantity(quantity)
		if err != nil {
			return nil, fmt.Errorf("resource %s: %w", name, err)
		}
		if q.Sign() <= 0 {
			return nil, fmt.Errorf("resource %s: quantity must be greater than zero, got %s", name, quantity)
		}
		// ate-api-server rejects a cpu capacity of 1000 cores or more.
		if name == "cpu" && q.Cmp(resource.MustParse("1000")) >= 0 {
			return nil, fmt.Errorf("resource cpu: must be less than 1000 cores, got %s", quantity)
		}
		limits = append(limits, &ateapipb.Limits{Name: name, Quantity: quantity})
	}
	out.Resources = &ateapipb.Resources{Limits: limits}
	return out, nil
}

// resolveDelays applies each per-call override over the default delay. A
// negative override means "use the default".
func resolveDelays(def, run, restore, checkpoint, uploadPausedCheckpoint, terminate time.Duration) (delays, error) {
	if def < 0 {
		return delays{}, fmt.Errorf("delay must not be negative, got %v", def)
	}
	pick := func(d time.Duration) time.Duration {
		if d < 0 {
			return def
		}
		return d
	}
	return delays{
		run:                    pick(run),
		restore:                pick(restore),
		checkpoint:             pick(checkpoint),
		uploadPausedCheckpoint: pick(uploadPausedCheckpoint),
		terminate:              pick(terminate),
	}, nil
}
