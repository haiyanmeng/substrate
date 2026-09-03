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
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	fileaccesslogv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/file/v3"
	tlsinspectorv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	networkinputsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/matching/common_inputs/network/v3"

	xdscorev3 "github.com/cncf/xds/go/xds/core/v3"
	matcherv3 "github.com/cncf/xds/go/xds/type/matcher/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/egress"
)

// The names below are the contract between this builder and the egress
// gateway's static bootstrap (manifests/ate-install/atenet-egress*.yaml).
// Changing one without changing the other leaves the listener referencing a
// cluster that does not exist, which Envoy rejects the whole snapshot for.
const (
	// ListenerName is the internal listener this package serves over LDS. The
	// CONNECT leg's route targets it, and it is the only dynamic resource in an
	// otherwise static bootstrap.
	ListenerName = "egress_dispatch"
	// mitmClusterName reaches the static MITM listener, which terminates the
	// tunneled TLS and re-originates it.
	mitmClusterName = "mitm_internal"
	// passthroughClusterName is the ORIGINAL_DST cluster that dials the address
	// the actor's kernel recorded, taken from the
	// envoy.network.transport_socket.original_dst_address filter state the
	// CONNECT leg set.
	passthroughClusterName = "egress_tcp_passthrough"

	// mitmChainName and passthroughChainName are what the matcher's actions
	// resolve to. They are filter chain names, so they only have to be unique
	// within this listener.
	mitmChainName        = "mitm"
	passthroughChainName = "passthrough"
)

// buildListener renders the dispatch listener for the given exemption sets.
//
// Every connection out of the CONNECT leg lands here. The listener's whole job
// is a two-level decision: which exemption set applies (from filter state, set
// per connection by ext_proc) and whether this connection's SNI is in it. A
// connection that answers yes to both is tunneled to the origin untouched;
// everything else goes to the MITM listener.
//
// The indirection through a set ID exists because Envoy's matchers compare a
// runtime input only against configured literals — there is no way to ask
// whether the SNI appears in a list carried on the connection. So each distinct
// set is compiled into its own subtree here, and the connection carries only
// the name of the subtree to use.
func buildListener(sets []egress.ExemptionSet) (*listenerv3.Listener, error) {
	matcher, err := buildMatcher(sets)
	if err != nil {
		return nil, err
	}

	tlsInspector, err := anypb.New(&tlsinspectorv3.TlsInspector{})
	if err != nil {
		return nil, fmt.Errorf("marshaling the TLS inspector config: %w", err)
	}

	mitm, err := buildChain(mitmChainName, mitmClusterName)
	if err != nil {
		return nil, err
	}
	passthrough, err := buildChain(passthroughChainName, passthroughClusterName)
	if err != nil {
		return nil, err
	}

	return &listenerv3.Listener{
		Name:              ListenerName,
		StatPrefix:        "egress_dispatch",
		ListenerSpecifier: &listenerv3.Listener_InternalListener{InternalListener: &listenerv3.Listener_InternalListenerConfig{}},
		// The SNI is the only thing this listener matches on, and the TLS
		// inspector is what recovers it. A connection that carries no
		// ClientHello — a raw stream, or a server-speaks-first protocol whose
		// client is waiting on the origin — has no SNI to be exempt on, so the
		// inspector timing out is a normal outcome and must not stall the
		// connection: it falls through to the MITM listener, which classifies
		// it properly on its own chains. The 1s budget mirrors that listener's.
		ListenerFiltersTimeout:           durationpb.New(tlsInspectorTimeout),
		ContinueOnListenerFiltersTimeout: true,
		ListenerFilters: []*listenerv3.ListenerFilter{{
			Name:       "envoy.filters.listener.tls_inspector",
			ConfigType: &listenerv3.ListenerFilter_TypedConfig{TypedConfig: tlsInspector},
		}},
		FilterChainMatcher: matcher,
		FilterChains:       []*listenerv3.FilterChain{mitm, passthrough},
	}, nil
}

// tlsInspectorTimeout bounds how long the listener waits for a ClientHello
// before giving up and treating the connection as having no SNI.
const tlsInspectorTimeout = time.Second

// buildMatcher renders the exemption sets into the listener's filter chain
// matcher: an exact match on the set ID in filter state, then, within the
// matched set, a match on the SNI.
//
// Both levels fail to MITM. That is the safe direction — the cost of getting it
// wrong is that a connection which could have been passed through is inspected
// instead — and it is also what makes the empty set free: an actor with no
// exemptions carries no filter state, matches no ID, and is intercepted without
// any of its own configuration existing here.
func buildMatcher(sets []egress.ExemptionSet) (*matcherv3.Matcher, error) {
	toMITM, err := chainAction(mitmChainName)
	if err != nil {
		return nil, err
	}

	setInput, err := anypb.New(&networkinputsv3.FilterStateInput{Key: egress.ExemptionFilterStateKey})
	if err != nil {
		return nil, fmt.Errorf("marshaling the exemption set matching input: %w", err)
	}

	byID := make(map[string]*matcherv3.Matcher_OnMatch, len(sets))
	for _, set := range sets {
		if set.IsEmpty() {
			continue
		}
		sub, err := buildSNIMatcher(set, toMITM)
		if err != nil {
			return nil, fmt.Errorf("exemption set %s: %w", set.ID(), err)
		}
		byID[set.ID()] = &matcherv3.Matcher_OnMatch{OnMatch: &matcherv3.Matcher_OnMatch_Matcher{Matcher: sub}}
	}

	// An exact_match_map has to have at least one entry, and until an actor with
	// exemptions connects there are none. A matcher that only ever reaches its
	// on_no_match is the honest encoding of "nothing is exempt yet".
	if len(byID) == 0 {
		return &matcherv3.Matcher{OnNoMatch: toMITM}, nil
	}

	return &matcherv3.Matcher{
		MatcherType: &matcherv3.Matcher_MatcherTree_{MatcherTree: &matcherv3.Matcher_MatcherTree{
			Input: &xdscorev3.TypedExtensionConfig{Name: "exemption-set", TypedConfig: setInput},
			TreeType: &matcherv3.Matcher_MatcherTree_ExactMatchMap{
				ExactMatchMap: &matcherv3.Matcher_MatcherTree_MatchMap{Map: byID},
			},
		}},
		OnNoMatch: toMITM,
	}, nil
}

// buildSNIMatcher renders one set's patterns as an ordered list of SNI
// predicates. A list rather than a map because the grammar allows a wildcard,
// which an exact_match_map cannot express.
func buildSNIMatcher(set egress.ExemptionSet, toMITM *matcherv3.Matcher_OnMatch) (*matcherv3.Matcher, error) {
	toPassthrough, err := chainAction(passthroughChainName)
	if err != nil {
		return nil, err
	}
	sniInput, err := anypb.New(&networkinputsv3.ServerNameInput{})
	if err != nil {
		return nil, fmt.Errorf("marshaling the SNI matching input: %w", err)
	}

	patterns := set.Patterns()
	matchers := make([]*matcherv3.Matcher_MatcherList_FieldMatcher, 0, len(patterns))
	for _, pattern := range patterns {
		value, err := patternMatcher(pattern)
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, &matcherv3.Matcher_MatcherList_FieldMatcher{
			Predicate: &matcherv3.Matcher_MatcherList_Predicate{
				MatchType: &matcherv3.Matcher_MatcherList_Predicate_SinglePredicate_{
					SinglePredicate: &matcherv3.Matcher_MatcherList_Predicate_SinglePredicate{
						Input:   &xdscorev3.TypedExtensionConfig{Name: "sni", TypedConfig: sniInput},
						Matcher: &matcherv3.Matcher_MatcherList_Predicate_SinglePredicate_ValueMatch{ValueMatch: value},
					},
				},
			},
			OnMatch: toPassthrough,
		})
	}

	return &matcherv3.Matcher{
		MatcherType: &matcherv3.Matcher_MatcherList_{
			MatcherList: &matcherv3.Matcher_MatcherList{Matchers: matchers},
		},
		// Stated rather than inherited from the enclosing matcher: an SNI this
		// set does not exempt must be intercepted, and that is too important to
		// rest on how Envoy propagates a nested no-match.
		OnNoMatch: toMITM,
	}, nil
}

// patternMatcher turns one exemption pattern into the string matcher Envoy
// compares the SNI against.
func patternMatcher(pattern string) (*matcherv3.StringMatcher, error) {
	// SNI case is the client's choice, and both forms name the same host.
	if suffix, found := strings.CutPrefix(pattern, "*."); found {
		if suffix == "" {
			return nil, fmt.Errorf("exemption pattern %q wildcards the whole name", pattern)
		}
		// A leftmost-label wildcard covers exactly one label, so "*.example.com"
		// must not match "a.b.example.com". Envoy's prefix and suffix matchers
		// cannot say "one label"; a regex can.
		return &matcherv3.StringMatcher{
			MatchPattern: &matcherv3.StringMatcher_SafeRegex{SafeRegex: &matcherv3.RegexMatcher{
				Regex: `(?i)^[^.]+\.` + regexp.QuoteMeta(suffix) + `$`,
			}},
		}, nil
	}
	return &matcherv3.StringMatcher{
		MatchPattern: &matcherv3.StringMatcher_Exact{Exact: pattern},
		IgnoreCase:   true,
	}, nil
}

// chainAction builds the matcher action that selects a filter chain by name.
func chainAction(chain string) (*matcherv3.Matcher_OnMatch, error) {
	name, err := anypb.New(wrapperspb.String(chain))
	if err != nil {
		return nil, fmt.Errorf("marshaling the %q filter chain action: %w", chain, err)
	}
	return &matcherv3.Matcher_OnMatch{
		OnMatch: &matcherv3.Matcher_OnMatch_Action{
			Action: &xdscorev3.TypedExtensionConfig{Name: chain, TypedConfig: name},
		},
	}, nil
}

// buildChain builds one of the listener's two filter chains: an opaque TCP
// relay into the cluster the decision picked. Nothing is parsed here — the
// dispatch listener only routes; the leg it routes to does the work.
func buildChain(name, cluster string) (*listenerv3.FilterChain, error) {
	log, err := anypb.New(&fileaccesslogv3.FileAccessLog{
		Path: "/dev/stdout",
		AccessLogFormat: &fileaccesslogv3.FileAccessLog_LogFormat{LogFormat: &corev3.SubstitutionFormatString{
			Format: &corev3.SubstitutionFormatString_TextFormatSource{TextFormatSource: &corev3.DataSource{
				Specifier: &corev3.DataSource_InlineString{InlineString: "[dispatch] decision=" + name +
					" sni=%REQUESTED_SERVER_NAME%" +
					" exempt_set=%FILTER_STATE(" + egress.ExemptionFilterStateKey + ":PLAIN)%" +
					" actor=%FILTER_STATE(dev.ate.actor.identity:PLAIN)%" +
					" flags=%RESPONSE_FLAGS% duration_ms=%DURATION%\n"},
			}},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling the %q access log config: %w", name, err)
	}

	proxy, err := anypb.New(&tcpproxyv3.TcpProxy{
		StatPrefix:       "egress_dispatch_" + name,
		ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: cluster},
		AccessLog: []*accesslogv3.AccessLog{{
			Name:       "envoy.access_loggers.file",
			ConfigType: &accesslogv3.AccessLog_TypedConfig{TypedConfig: log},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling the %q tcp_proxy config: %w", name, err)
	}

	return &listenerv3.FilterChain{
		Name: name,
		Filters: []*listenerv3.Filter{{
			Name:       "envoy.filters.network.tcp_proxy",
			ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: proxy},
		}},
	}, nil
}

// sortedSets returns the sets in ID order, so that the same set of exemptions
// always renders byte-identical config and an unchanged snapshot stays
// unchanged.
func sortedSets(sets map[string]egress.ExemptionSet) []egress.ExemptionSet {
	ordered := make([]egress.ExemptionSet, 0, len(sets))
	for _, set := range sets {
		ordered = append(ordered, set)
	}
	slices.SortFunc(ordered, func(a, b egress.ExemptionSet) int { return strings.Compare(a.ID(), b.ID()) })
	return ordered
}
