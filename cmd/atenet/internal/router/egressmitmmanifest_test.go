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

package router

import (
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
)

// sdsmintManifest is the MITM egress gateway. It is the only variant with a
// decrypted leg, and so the only one that can enforce an EgressPolicy.
const sdsmintManifest = "../../../../manifests/ate-install/atenet-egress-with-sdsmint.yaml"

// TestEgressMITMFilterChainsSelectTheirHandler is the config half of the
// ext_proc mux's dispatch. The mux picks a handler from the filter chain name
// Envoy asserts, and the two sides agree only by these string constants: a
// chain renamed here, or one that loses its name entirely, is classified as
// ingress and refused with a 404 by the --mode=egress instance in front of it.
// Nothing else in the tree notices.
func TestEgressMITMFilterChainsSelectTheirHandler(t *testing.T) {
	listeners := egressListeners(t, envoyConfig(t, sdsmintManifest))

	for _, want := range []struct {
		listener, chain string
	}{
		{"egress", extproc.EgressFilterChainName},
		{"mitm_listener", extproc.EgressMITMFilterChainName},
		{"mitm_listener", extproc.EgressMITMCleartextFilterChainName},
	} {
		if findChain(listeners, want.listener, want.chain) == nil {
			t.Errorf("listener %q has no filter chain named %q, so ext_proc cannot dispatch it to the handler that serves it",
				want.listener, want.chain)
		}
	}
}

// TestEgressMITMChainsEnforceEgressPolicy checks that both HTTP chains of the
// MITM leg actually run the policy ext_proc, and run it with everything the
// handler needs. Each request_attribute is a separate failure mode that no
// other test would catch: an attribute Envoy is not asked for arrives as
// nothing at all, which reads exactly like one that was never set, and the
// handler fails closed on it — so a dropped line here turns into every actor's
// egress being denied, in production only.
func TestEgressMITMChainsEnforceEgressPolicy(t *testing.T) {
	listeners := egressListeners(t, envoyConfig(t, sdsmintManifest))

	for _, name := range []string{extproc.EgressMITMFilterChainName, extproc.EgressMITMCleartextFilterChainName} {
		t.Run(name, func(t *testing.T) {
			chain := findChain(listeners, "mitm_listener", name)
			if chain == nil {
				t.Fatalf("mitm_listener has no filter chain named %q", name)
			}
			filters := chain.httpFilters()

			i := slices.IndexFunc(filters, func(f envoyHTTPFilter) bool {
				return f.Name == "envoy.filters.http.ext_proc"
			})
			if i < 0 {
				t.Fatal("chain runs no ext_proc filter, so requests out of the tunnel reach their destination unauthorized")
			}
			extProc := filters[i]

			// A denial has to be answered before the gateway resolves or dials
			// the destination it is denying.
			forwardProxy := slices.IndexFunc(filters, func(f envoyHTTPFilter) bool {
				return f.Name == "envoy.filters.http.dynamic_forward_proxy"
			})
			if forwardProxy >= 0 && forwardProxy < i {
				t.Error("ext_proc runs after dynamic_forward_proxy, so the gateway resolves the destination before deciding whether it is allowed")
			}

			if got := extProc.TypedConfig.GrpcService.EnvoyGrpc.ClusterName; got != "ext_proc_server" {
				t.Errorf("ext_proc dials cluster %q, want the co-located router at %q", got, "ext_proc_server")
			}
			if extProc.TypedConfig.FailureModeAllow {
				t.Error("failure_mode_allow is true, so an unreachable router lets every destination through unauthorized")
			}
			if got := extProc.TypedConfig.ProcessingMode.RequestHeaderMode; got != "SEND" {
				t.Errorf("request_header_mode is %q, want SEND; the handler never runs otherwise", got)
			}

			for _, attr := range []string{
				extproc.FilterChainNameAttribute,
				extproc.ActorIdentityFilterStateAttribute,
				extproc.EgressDestinationFilterStateAttribute,
			} {
				if !slices.Contains(extProc.TypedConfig.RequestAttributes, attr) {
					t.Errorf("ext_proc does not request the %s attribute, so it arrives at the handler as nothing at all", attr)
				}
			}
		})
	}
}

// TestEgressConnectLegPublishesTheDestination checks the one value the MITM leg
// cannot recover for itself. atunnel takes the CONNECT authority from
// SO_ORIGINAL_DST, so it is the only record of the destination IP an
// EgressPolicy IPBlockRule is written against; once the tunnel is open the
// request inside it carries a hostname and nothing else. Filter state shared
// with the upstream is what carries it across the internal-listener hop.
func TestEgressConnectLegPublishesTheDestination(t *testing.T) {
	listeners := egressListeners(t, envoyConfig(t, sdsmintManifest))
	chain := findChain(listeners, "egress", extproc.EgressFilterChainName)
	if chain == nil {
		t.Fatalf("the egress listener has no filter chain named %q", extproc.EgressFilterChainName)
	}

	for _, f := range chain.httpFilters() {
		if f.Name != "envoy.filters.http.set_filter_state" {
			continue
		}
		for _, v := range f.TypedConfig.OnRequestHeaders {
			if v.ObjectKey != extproc.EgressDestinationFilterStateKey {
				continue
			}
			if v.SharedWithUpstream != "ONCE" {
				t.Errorf("%s is set with shared_with_upstream %q, so it does not survive the hop to mitm_listener; want ONCE",
					v.ObjectKey, v.SharedWithUpstream)
			}
			return
		}
	}
	t.Errorf("the CONNECT leg never sets the %s filter state, so no IPBlockRule can ever match on the MITM leg",
		extproc.EgressDestinationFilterStateKey)
}

// The sliver of an Envoy bootstrap these tests read. Unnamed fields are dropped
// by the decoder, so the manifest stays free to grow.
type envoyListener struct {
	Name         string             `json:"name"`
	FilterChains []envoyFilterChain `json:"filter_chains"`
}

type envoyFilterChain struct {
	Name    string `json:"name"`
	Filters []struct {
		TypedConfig struct {
			HTTPFilters []envoyHTTPFilter `json:"http_filters"`
		} `json:"typed_config"`
	} `json:"filters"`
}

type envoyHTTPFilter struct {
	Name        string `json:"name"`
	TypedConfig struct {
		// ext_proc
		GrpcService struct {
			EnvoyGrpc struct {
				ClusterName string `json:"cluster_name"`
			} `json:"envoy_grpc"`
		} `json:"grpc_service"`
		FailureModeAllow  bool     `json:"failure_mode_allow"`
		RequestAttributes []string `json:"request_attributes"`
		ProcessingMode    struct {
			RequestHeaderMode string `json:"request_header_mode"`
		} `json:"processing_mode"`

		// set_filter_state
		OnRequestHeaders []struct {
			ObjectKey          string `json:"object_key"`
			SharedWithUpstream string `json:"shared_with_upstream"`
		} `json:"on_request_headers"`
	} `json:"typed_config"`
}

// httpFilters is every HTTP filter in the chain's connection managers, in order.
func (c envoyFilterChain) httpFilters() []envoyHTTPFilter {
	var out []envoyHTTPFilter
	for _, f := range c.Filters {
		out = append(out, f.TypedConfig.HTTPFilters...)
	}
	return out
}

func egressListeners(t *testing.T, raw string) []envoyListener {
	t.Helper()
	var bootstrap struct {
		StaticResources struct {
			Listeners []envoyListener `json:"listeners"`
		} `json:"static_resources"`
	}
	if err := yaml.Unmarshal([]byte(raw), &bootstrap); err != nil {
		t.Fatalf("parsing envoy.yaml: %v", err)
	}
	if len(bootstrap.StaticResources.Listeners) == 0 {
		t.Fatal("found no listeners; the manifest changed shape and these tests are checking nothing")
	}
	return bootstrap.StaticResources.Listeners
}

// findChain returns the named filter chain of the named listener, or nil.
func findChain(listeners []envoyListener, listener, chain string) *envoyFilterChain {
	for _, l := range listeners {
		if l.Name != listener {
			continue
		}
		for i, c := range l.FilterChains {
			if c.Name == chain {
				return &l.FilterChains[i]
			}
		}
	}
	return nil
}
