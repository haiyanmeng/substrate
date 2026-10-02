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

// Package fakeworker names the fake Workers of the benchmark fake data plane.
//
// fake-workersync creates the Workers and fake-atelet reports their capacity.
// Both derive the same names from the same inputs, so neither has to ask
// ate-api-server or the other fake which Workers exist.
package fakeworker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"

	"github.com/google/uuid"
)

// MaxRunLength bounds the run prefix so every generated name stays well inside
// the 63-character limit on Worker names.
const MaxRunLength = 8

// MaxWorkersPerNode bounds the per-node index to five digits, for the same
// reason.
const MaxWorkersPerNode = 99999

var runPattern = regexp.MustCompile(`^[a-z0-9]+$`)

// podUIDNamespace is the UUIDv5 namespace fake WorkerPodUids are derived in.
// Changing it renames every fake Worker's pod UID.
var podUIDNamespace = uuid.MustParse("6f1c2a5e-8b3d-4e7a-9c01-5d2b7e4f8a90")

// ValidateRun reports whether run is usable as a run prefix.
func ValidateRun(run string) error {
	if len(run) == 0 || len(run) > MaxRunLength || !runPattern.MatchString(run) {
		return fmt.Errorf("run prefix %q must be 1 to %d lowercase letters or digits", run, MaxRunLength)
	}
	return nil
}

// ValidateWorkersPerNode reports whether n is a usable per-node Worker count.
func ValidateWorkersPerNode(n int) error {
	if n < 1 || n > MaxWorkersPerNode {
		return fmt.Errorf("workers per node must be between 1 and %d, got %d", MaxWorkersPerNode, n)
	}
	return nil
}

// Name returns the name of the index-th fake Worker on node. The node name
// enters as a hash because node names alone can come close to 63 characters.
func Name(run, node string, index int) string {
	sum := sha256.Sum256([]byte(node))
	return "fake-" + run + "-" + hex.EncodeToString(sum[:])[:8] + "-" + strconv.Itoa(index)
}

// Names returns the names of the fake Workers on node, in index order.
func Names(run, node string, workersPerNode int) []string {
	names := make([]string, workersPerNode)
	for i := range names {
		names[i] = Name(run, node, i)
	}
	return names
}

// PodUID returns the WorkerPodUid recorded for the Worker named name.
func PodUID(name string) string {
	return uuid.NewSHA1(podUIDNamespace, []byte(name)).String()
}

// IP returns the address recorded for the index-th fake Worker on a node: a
// documentation-range address that nothing answers on. Addresses repeat past
// 254 Workers, which ate-api-server allows.
func IP(index int) string {
	return "192.0.2." + strconv.Itoa(index%254+1)
}
