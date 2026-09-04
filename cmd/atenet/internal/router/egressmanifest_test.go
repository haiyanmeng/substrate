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
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// sdsmintEgressManifest is the variant that terminates and re-originates the
// tunneled TLS. It is the only one with chains to choose between, so the
// chain-selection test below reads it alone.
const sdsmintEgressManifest = "../../../../manifests/ate-install/atenet-egress-with-sdsmint.yaml"

// egressManifests are the two variants ate-setup installs; the plain one is the
// default path, the sdsmint one terminates and re-originates the tunneled TLS.
// They are siblings, and the timeout below was set on one and not the other.
var egressManifests = []string{
	"../../../../manifests/ate-install/atenet-egress.yaml",
	sdsmintEgressManifest,
}

// TestEgressManifestsDisableTheConnectTimeout is the static-config half of
// TestBuildConnectRoutes_DisablesTimeout: Envoy applies a route's timeout to a
// CONNECT tunnel's whole lifetime rather than to its headers, so a route left
// at Envoy's 15s default caps how long any actor's outbound connection may
// exist -- streaming responses, long downloads and SSH sessions all die
// mid-stream at 15s. These manifests are what the gateway actually runs, and
// nothing else in the tree notices when one of them loses the line.
func TestEgressManifestsDisableTheConnectTimeout(t *testing.T) {
	for _, path := range egressManifests {
		t.Run(path, func(t *testing.T) {
			routes := connectRoutes(parseBootstrap(t, envoyConfig(t, path)))
			if len(routes) == 0 {
				t.Fatal("found no connect_matcher route; the manifest changed shape and this test is checking nothing")
			}
			for _, r := range routes {
				if r.Route.Timeout == nil {
					t.Errorf("connect_matcher route to cluster %q sets no timeout, so it falls back to Envoy's 15s default and cuts every tunnel after 15s", r.Route.Cluster)
					continue
				}
				d, err := time.ParseDuration(*r.Route.Timeout)
				if err != nil {
					t.Errorf("connect_matcher route to cluster %q has timeout %q, which is not a duration: %v", r.Route.Cluster, *r.Route.Timeout, err)
					continue
				}
				if d != 0 {
					t.Errorf("connect_matcher route to cluster %q has timeout %s; it must be 0 to disable it", r.Route.Cluster, d)
				}
			}
		})
	}
}

// relayCluster is the ORIGINAL_DST cluster that dials the origin without
// inspecting anything. Every other path out of this gateway classifies the
// payload first, so reaching this one is what "uninspected" means.
const relayCluster = "egress_tcp_passthrough"

// TestEgressMITMManifestRelaysOnlySSH checks that the opaque relay is reachable
// only by dialing port 22, and only from the CONNECT leg where the destination
// comes from atunnel's SO_ORIGINAL_DST readout rather than from the actor.
//
// The rule this pins is not "SSH is allowed" but "chain selection is not the
// actor's to make". A chain that relays and is reachable by payload is a chain
// an actor selects for itself: listener filters peek with MSG_PEEK rather than
// consuming, so a junk first byte gets a connection classified as neither TLS
// nor HTTP and still arrives at the origin intact. Restoring a relay as
// mitm_listener's raw_buffer fallback -- which is what it was, and the obvious
// way to make a stuck non-HTTP workload work again -- reopens exactly that, and
// nothing else in the tree notices.
func TestEgressMITMManifestRelaysOnlySSH(t *testing.T) {
	bootstrap := parseBootstrap(t, envoyConfig(t, sdsmintEgressManifest))

	var toRelay []envoyRoute
	for _, r := range connectRoutes(bootstrap) {
		if r.Route.Cluster == relayCluster {
			toRelay = append(toRelay, r)
		}
	}
	if len(toRelay) != 1 {
		t.Fatalf("found %d routes to %s, want exactly 1; every route to it is a port that leaves uninspected", len(toRelay), relayCluster)
	}
	if headers := toRelay[0].Match.Headers; len(headers) != 1 ||
		headers[0].Name != ":authority" || headers[0].StringMatch.Suffix != ":22" {
		t.Errorf("the route to %s matches %+v, want a single :authority suffix match on \":22\"; an unqualified match relays every port", relayCluster, headers)
	}

	// That route is the only way in. A network filter reaching the relay is one
	// selected by a filter chain, which is to say by the payload -- the input
	// this whole arrangement exists to stop deciding on.
	for _, l := range bootstrap.StaticResources.Listeners {
		for _, fc := range l.FilterChains {
			for _, f := range fc.Filters {
				if f.TypedConfig.Cluster == relayCluster {
					t.Errorf("listener %q proxies to %s from a filter chain; the relay may only be reached by the CONNECT route above, where the destination comes from the kernel rather than from the bytes", l.Name, relayCluster)
				}
			}
		}
	}

	// mitm_listener's fallback, where a connection lands when neither inspector
	// could claim it and where the sniff timeout resolves.
	mitm := listener(t, bootstrap, "mitm_listener")
	if !mitm.ContinueOnListenerFiltersTimeout {
		t.Error("mitm_listener does not set continue_on_listener_filters_timeout; a client that sends nothing is then refused by the listener rather than by the deny chain, so the refusal has no access log naming the destination")
	}
	fallback := mitm.FilterChains[len(mitm.FilterChains)-1]
	if fallback.FilterChainMatch.TransportProtocol != "raw_buffer" || len(fallback.FilterChainMatch.ApplicationProtocols) != 0 {
		t.Fatalf("mitm_listener's last filter chain matches %+v, want an unqualified raw_buffer match; this test is reading the wrong chain", fallback.FilterChainMatch)
	}
	for _, f := range fallback.Filters {
		if f.TypedConfig.Cluster == "" {
			continue
		}
		if endpoints := clusterEndpoints(t, bootstrap, f.TypedConfig.Cluster); endpoints != 0 {
			t.Errorf("mitm_listener's fallback chain proxies to %s, which has %d endpoints; the fallback must deny, and it denies by having nothing to select", f.TypedConfig.Cluster, endpoints)
		}
	}
}

// envoyRoute is the sliver of an Envoy bootstrap these tests read. Unnamed
// fields are dropped by the decoder, so the manifests stay free to grow.
type envoyRoute struct {
	Match struct {
		ConnectMatcher *struct{} `json:"connect_matcher"`
		Headers        []struct {
			Name        string `json:"name"`
			StringMatch struct {
				Suffix string `json:"suffix"`
			} `json:"string_match"`
		} `json:"headers"`
	} `json:"match"`
	Route struct {
		Cluster string  `json:"cluster"`
		Timeout *string `json:"timeout"`
	} `json:"route"`
}

type envoyListener struct {
	Name string `json:"name"`
	// Envoy's own default is false, and so is the zero value here.
	ContinueOnListenerFiltersTimeout bool `json:"continue_on_listener_filters_timeout"`
	FilterChains                     []struct {
		FilterChainMatch struct {
			TransportProtocol    string   `json:"transport_protocol"`
			ApplicationProtocols []string `json:"application_protocols"`
		} `json:"filter_chain_match"`
		Filters []struct {
			TypedConfig struct {
				// Set by tcp_proxy; an HCM leaves it empty and routes instead.
				Cluster     string `json:"cluster"`
				RouteConfig struct {
					VirtualHosts []struct {
						Routes []envoyRoute `json:"routes"`
					} `json:"virtual_hosts"`
				} `json:"route_config"`
			} `json:"typed_config"`
		} `json:"filters"`
	} `json:"filter_chains"`
}

type envoyBootstrap struct {
	StaticResources struct {
		Listeners []envoyListener `json:"listeners"`
		Clusters  []struct {
			Name           string `json:"name"`
			LoadAssignment struct {
				Endpoints []struct {
					LbEndpoints []struct{} `json:"lb_endpoints"`
				} `json:"endpoints"`
			} `json:"load_assignment"`
		} `json:"clusters"`
	} `json:"static_resources"`
}

func parseBootstrap(t *testing.T, raw string) envoyBootstrap {
	t.Helper()
	var bootstrap envoyBootstrap
	if err := yaml.Unmarshal([]byte(raw), &bootstrap); err != nil {
		t.Fatalf("parsing envoy.yaml: %v", err)
	}
	return bootstrap
}

func listener(t *testing.T, bootstrap envoyBootstrap, name string) envoyListener {
	t.Helper()
	for _, l := range bootstrap.StaticResources.Listeners {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("the bootstrap has no listener named %q", name)
	return envoyListener{}
}

// clusterEndpoints is how many endpoints name is configured with. A cluster the
// manifest never defines is a fatal error rather than zero endpoints: Envoy
// refuses to start on it, so reading it as "denies" would pass a test on a
// config that never runs.
func clusterEndpoints(t *testing.T, bootstrap envoyBootstrap, name string) int {
	t.Helper()
	for _, c := range bootstrap.StaticResources.Clusters {
		if c.Name != name {
			continue
		}
		total := 0
		for _, e := range c.LoadAssignment.Endpoints {
			total += len(e.LbEndpoints)
		}
		return total
	}
	t.Fatalf("the bootstrap has no cluster named %q", name)
	return 0
}

// connectRoutes is every route in the bootstrap matched by connect_matcher.
func connectRoutes(bootstrap envoyBootstrap) []envoyRoute {
	var out []envoyRoute
	for _, l := range bootstrap.StaticResources.Listeners {
		for _, fc := range l.FilterChains {
			for _, f := range fc.Filters {
				for _, vh := range f.TypedConfig.RouteConfig.VirtualHosts {
					for _, r := range vh.Routes {
						if r.Match.ConnectMatcher != nil {
							out = append(out, r)
						}
					}
				}
			}
		}
	}
	return out
}

// envoyConfig is the envoy.yaml the atenet-egress ConfigMap in path ships.
func envoyConfig(t *testing.T, path string) string {
	t.Helper()
	manifest, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, doc := range strings.Split(string(manifest), "\n---\n") {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parsing a document of %s: %v", path, err)
		}
		if obj.Kind != "ConfigMap" || obj.Metadata.Name != "atenet-egress" {
			continue
		}
		envoyYaml, ok := obj.Data["envoy.yaml"]
		if !ok {
			t.Fatalf("the atenet-egress ConfigMap in %s has no envoy.yaml key", path)
		}
		return envoyYaml
	}
	t.Fatalf("%s has no ConfigMap named atenet-egress", path)
	return ""
}
