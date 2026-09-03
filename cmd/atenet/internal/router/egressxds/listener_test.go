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

package egressxds

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	networkinputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"

	matcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/egress"
)

// newSet is shorthand for the exemption sets these tests render.
func newSet(patterns ...string) egress.ExemptionSet { return egress.NewExemptionSet(patterns) }

// chainFor returns the filter chain the matcher's action names.
func chainFor(t *testing.T, match *matcherv3.Matcher_OnMatch) string {
	t.Helper()
	action, ok := match.GetOnMatch().(*matcherv3.Matcher_OnMatch_Action)
	if !ok {
		t.Fatalf("on_match is %T, want an action", match.GetOnMatch())
	}
	var name wrapperspb.StringValue
	if err := action.Action.GetTypedConfig().UnmarshalTo(&name); err != nil {
		t.Fatalf("unmarshaling the filter chain action: %v", err)
	}
	return name.GetValue()
}

// The two chains the matcher can name have to exist, or Envoy rejects the whole
// listener.
func TestBuildListenerChains(t *testing.T) {
	listener, err := buildListener([]egress.ExemptionSet{newSet("api.example.com")})
	if err != nil {
		t.Fatalf("buildListener() error = %v", err)
	}
	if listener.GetInternalListener() == nil {
		t.Error("the dispatch listener has no internal_listener; it must not own a socket")
	}

	want := map[string]string{
		mitmChainName:        mitmClusterName,
		passthroughChainName: passthroughClusterName,
	}
	if got := len(listener.GetFilterChains()); got != len(want) {
		t.Fatalf("filter chains = %d, want %d", got, len(want))
	}
	for _, chain := range listener.GetFilterChains() {
		cluster, ok := want[chain.GetName()]
		if !ok {
			t.Errorf("unexpected filter chain %q", chain.GetName())
			continue
		}
		var proxy tcpproxyv3.TcpProxy
		if err := chain.GetFilters()[0].GetTypedConfig().UnmarshalTo(&proxy); err != nil {
			t.Fatalf("unmarshaling the %q tcp_proxy: %v", chain.GetName(), err)
		}
		if got := proxy.GetCluster(); got != cluster {
			t.Errorf("chain %q proxies to %q, want %q", chain.GetName(), got, cluster)
		}
	}
}

// Nothing is exempt until an actor with exemptions connects, and the listener
// still has to be valid in the meantime.
func TestBuildListenerWithNoSetsInterceptsEverything(t *testing.T) {
	listener, err := buildListener(nil)
	if err != nil {
		t.Fatalf("buildListener() error = %v", err)
	}
	matcher := listener.GetFilterChainMatcher()
	if matcher.GetMatcherTree() != nil {
		t.Error("matcher has a tree with no exemption sets to key it on")
	}
	if got := chainFor(t, matcher.GetOnNoMatch()); got != mitmChainName {
		t.Errorf("on_no_match selects %q, want %q", got, mitmChainName)
	}
}

// The outer level keys on the filter state ext_proc writes; every set gets its
// own subtree, and anything unrecognized is intercepted.
func TestBuildMatcherKeysOnTheExemptionSet(t *testing.T) {
	first := newSet("api.example.com")
	second := newSet("api.example.com", "cdn.example.com")

	matcher, err := buildMatcher([]egress.ExemptionSet{first, second})
	if err != nil {
		t.Fatalf("buildMatcher() error = %v", err)
	}

	var input networkinputsv3.FilterStateInput
	if err := matcher.GetMatcherTree().GetInput().GetTypedConfig().UnmarshalTo(&input); err != nil {
		t.Fatalf("unmarshaling the tree input: %v", err)
	}
	if got := input.GetKey(); got != egress.ExemptionFilterStateKey {
		t.Errorf("tree keys on filter state %q, want %q", got, egress.ExemptionFilterStateKey)
	}

	entries := matcher.GetMatcherTree().GetExactMatchMap().GetMap()
	got := slices.Sorted(maps.Keys(entries))
	want := slices.Sorted(slices.Values([]string{first.ID(), second.ID()}))
	if !slices.Equal(got, want) {
		t.Errorf("exact_match_map keys = %v, want %v", got, want)
	}
	if got := chainFor(t, matcher.GetOnNoMatch()); got != mitmChainName {
		t.Errorf("an unknown exemption set selects %q, want %q", got, mitmChainName)
	}
}

// An empty set has no ID and needs no configuration; rendering one would put an
// unreachable entry keyed on "" into the map.
func TestBuildMatcherSkipsEmptySets(t *testing.T) {
	matcher, err := buildMatcher([]egress.ExemptionSet{egress.NewExemptionSet(nil), newSet("api.example.com")})
	if err != nil {
		t.Fatalf("buildMatcher() error = %v", err)
	}
	if got := len(matcher.GetMatcherTree().GetExactMatchMap().GetMap()); got != 1 {
		t.Errorf("exact_match_map has %d entries, want 1", got)
	}
}

// sniDecisions replays a set's subtree against a list of SNIs the way Envoy
// would, and reports which chain each one lands on.
func sniDecisions(t *testing.T, set egress.ExemptionSet, names []string) map[string]string {
	t.Helper()
	matcher, err := buildMatcher([]egress.ExemptionSet{set})
	if err != nil {
		t.Fatalf("buildMatcher() error = %v", err)
	}
	sub := matcher.GetMatcherTree().GetExactMatchMap().GetMap()[set.ID()].GetMatcher()

	decisions := make(map[string]string, len(names))
	for _, name := range names {
		decisions[name] = chainFor(t, sub.GetOnNoMatch())
		for _, field := range sub.GetMatcherList().GetMatchers() {
			if matchesSNI(t, field.GetPredicate().GetSinglePredicate().GetValueMatch(), name) {
				decisions[name] = chainFor(t, field.GetOnMatch())
				break
			}
		}
	}
	return decisions
}

func matchesSNI(t *testing.T, value *matcherv3.StringMatcher, sni string) bool {
	t.Helper()
	switch pattern := value.GetMatchPattern().(type) {
	case *matcherv3.StringMatcher_Exact:
		if value.GetIgnoreCase() {
			return strings.EqualFold(pattern.Exact, sni)
		}
		return pattern.Exact == sni
	case *matcherv3.StringMatcher_SafeRegex:
		re, err := regexp.Compile(pattern.SafeRegex.GetRegex())
		if err != nil {
			t.Fatalf("compiling %q: %v", pattern.SafeRegex.GetRegex(), err)
		}
		return re.MatchString(sni)
	default:
		t.Fatalf("unexpected string matcher %T", pattern)
		return false
	}
}

func TestSNIMatching(t *testing.T) {
	set := newSet("api.example.com", "*.cdn.example.com")
	names := []string{
		"api.example.com",
		"API.Example.com",
		"assets.cdn.example.com",
		// A leftmost-label wildcard covers one label, not a whole subtree.
		"a.b.cdn.example.com",
		// The wildcard must not match the bare parent.
		"cdn.example.com",
		"api.example.com.evil.test",
		"evil.test",
		// No SNI at all — a raw stream, or a ClientHello that never arrived.
		"",
	}
	want := map[string]string{
		"api.example.com":           passthroughChainName,
		"API.Example.com":           passthroughChainName,
		"assets.cdn.example.com":    passthroughChainName,
		"a.b.cdn.example.com":       mitmChainName,
		"cdn.example.com":           mitmChainName,
		"api.example.com.evil.test": mitmChainName,
		"evil.test":                 mitmChainName,
		"":                          mitmChainName,
	}

	got := sniDecisions(t, set, names)
	for _, name := range names {
		if got[name] != want[name] {
			t.Errorf("SNI %q selects %q, want %q", name, got[name], want[name])
		}
	}
}

// Two calls with the same sets must produce the same config, or every push
// looks like a change.
func TestBuildListenerIsDeterministic(t *testing.T) {
	sets := []egress.ExemptionSet{newSet("b.example.com"), newSet("a.example.com")}

	first, err := buildListener(sets)
	if err != nil {
		t.Fatalf("buildListener() error = %v", err)
	}
	second, err := buildListener(sets)
	if err != nil {
		t.Fatalf("buildListener() error = %v", err)
	}
	if !proto.Equal(first, second) {
		t.Error("buildListener() is not deterministic for the same sets")
	}
}
