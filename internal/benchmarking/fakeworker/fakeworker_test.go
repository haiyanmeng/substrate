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

package fakeworker

import (
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The two fakes run as separate binaries and must agree on every name, so
// the output is pinned: a change here renames every fake Worker and must be
// made to both fakes at once.
func TestNamePinned(t *testing.T) {
	const node = "gke-bench-default-pool-1a2b3c4d-xyz1"
	if got, want := Name("r1", node, 0), "fake-r1-163cdf46-0"; got != want {
		t.Errorf("Name(r1, %s, 0) = %q, want %q", node, got, want)
	}
	if got, want := PodUID("fake-r1-163cdf46-0"), "61ab5d90-c44c-54aa-bc46-5194ceebaa46"; got != want {
		t.Errorf("PodUID = %q, want %q", got, want)
	}
}

// k8sShortName is the k8s-short-name format Worker names are validated
// against.
var k8sShortName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestNameIsAShortNameAtTheBounds(t *testing.T) {
	name := Name(strings.Repeat("z", MaxRunLength), strings.Repeat("n", 253), MaxWorkersPerNode)
	if len(name) > 63 || !k8sShortName.MatchString(name) {
		t.Errorf("Name at the bounds = %q (%d chars), want a short name of at most 63", name, len(name))
	}
}

func TestNamesDistinctAcrossNodes(t *testing.T) {
	seen := map[string]bool{}
	for _, node := range []string{"node-a", "node-b"} {
		for _, name := range Names("r1", node, 3) {
			if seen[name] {
				t.Fatalf("name %q generated twice", name)
			}
			seen[name] = true
		}
	}
}

func TestPodUIDIsAStableUUID(t *testing.T) {
	a, b := PodUID("fake-r1-00000000-0"), PodUID("fake-r1-00000000-0")
	if a != b {
		t.Errorf("PodUID is not deterministic: %q then %q", a, b)
	}
	if _, err := uuid.Parse(a); err != nil {
		t.Errorf("PodUID = %q, not a UUID: %v", a, err)
	}
	if PodUID("fake-r1-00000000-1") == a {
		t.Error("two names share a pod UID")
	}
}

func TestIPInDocumentationRange(t *testing.T) {
	doc := netip.MustParsePrefix("192.0.2.0/24")
	for _, i := range []int{0, 253, 254, 1000} {
		addr, err := netip.ParseAddr(IP(i))
		if err != nil || !doc.Contains(addr) || addr.As4()[3] == 0 || addr.As4()[3] == 255 {
			t.Errorf("IP(%d) = %q, want a host address in %s", i, IP(i), doc)
		}
	}
	if IP(0) != IP(254) {
		t.Errorf("IP(0) = %q, IP(254) = %q; addresses should repeat every 254", IP(0), IP(254))
	}
}

func TestValidateRun(t *testing.T) {
	for _, run := range []string{"r", "abc12345"} {
		if err := ValidateRun(run); err != nil {
			t.Errorf("ValidateRun(%q) = %v, want nil", run, err)
		}
	}
	for _, run := range []string{"", "abc123456", "Run", "a-b", "a_b"} {
		if err := ValidateRun(run); err == nil {
			t.Errorf("ValidateRun(%q) = nil, want an error", run)
		}
	}
}

func TestValidateWorkersPerNode(t *testing.T) {
	for _, n := range []int{1, MaxWorkersPerNode} {
		if err := ValidateWorkersPerNode(n); err != nil {
			t.Errorf("ValidateWorkersPerNode(%d) = %v, want nil", n, err)
		}
	}
	for _, n := range []int{0, -1, MaxWorkersPerNode + 1} {
		if err := ValidateWorkersPerNode(n); err == nil {
			t.Errorf("ValidateWorkersPerNode(%d) = nil, want an error", n)
		}
	}
}
