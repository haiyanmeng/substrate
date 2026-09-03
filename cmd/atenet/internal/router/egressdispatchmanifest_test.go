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
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/egress"
	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/egressxds"
)

// The TLS-interception exemption path is split across a Go control plane and a
// hand-written Envoy bootstrap, and the two agree only by convention: a node
// ID, a listener name, a filter state key, a port. Each of these fails quietly
// when it drifts -- the gateway keeps tunneling and simply intercepts
// everything, which is the same thing it does when no actor has exemptions. The
// tests below are what notices.

// sdsmintManifest is the only egress variant that intercepts TLS, so the only
// one with a dispatch listener. atenet-egress.yaml tunnels everything untouched
// already, which is what an exemption asks for.
const sdsmintManifest = "../../../../manifests/ate-install/atenet-egress-with-sdsmint.yaml"

// dispatchBootstrap is the sliver of the Envoy bootstrap these tests read.
type dispatchBootstrap struct {
	Node struct {
		ID string `json:"id"`
	} `json:"node"`
	DynamicResources struct {
		LdsConfig struct {
			Ads *struct{} `json:"ads"`
		} `json:"lds_config"`
		AdsConfig struct {
			GrpcServices []struct {
				EnvoyGrpc struct {
					ClusterName string `json:"cluster_name"`
				} `json:"envoy_grpc"`
			} `json:"grpc_services"`
		} `json:"ads_config"`
	} `json:"dynamic_resources"`
	StaticResources struct {
		Listeners []dispatchListener `json:"listeners"`
		Clusters  []dispatchCluster  `json:"clusters"`
	} `json:"static_resources"`
}

type dispatchListener struct {
	Name         string `json:"name"`
	FilterChains []struct {
		Filters []struct {
			TypedConfig struct {
				HTTPFilters []struct {
					Name        string `json:"name"`
					TypedConfig struct {
						OnRequestHeaders []struct {
							ObjectKey          string `json:"object_key"`
							SharedWithUpstream string `json:"shared_with_upstream"`
						} `json:"on_request_headers"`
					} `json:"typed_config"`
				} `json:"http_filters"`
			} `json:"typed_config"`
		} `json:"filters"`
	} `json:"filter_chains"`
}

type dispatchCluster struct {
	Name           string `json:"name"`
	LoadAssignment struct {
		Endpoints []struct {
			LbEndpoints []struct {
				Endpoint dispatchEndpoint `json:"endpoint"`
			} `json:"lb_endpoints"`
		} `json:"endpoints"`
	} `json:"load_assignment"`
}

type dispatchEndpoint struct {
	Address struct {
		SocketAddress struct {
			Address   string `json:"address"`
			PortValue int    `json:"port_value"`
		} `json:"socket_address"`
		EnvoyInternalAddress struct {
			ServerListenerName string `json:"server_listener_name"`
		} `json:"envoy_internal_address"`
	} `json:"address"`
}

func dispatchConfig(t *testing.T) dispatchBootstrap {
	t.Helper()
	var bootstrap dispatchBootstrap
	if err := yaml.Unmarshal([]byte(envoyConfig(t, sdsmintManifest)), &bootstrap); err != nil {
		t.Fatalf("parsing envoy.yaml: %v", err)
	}
	return bootstrap
}

func (b dispatchBootstrap) cluster(t *testing.T, name string) dispatchCluster {
	t.Helper()
	for _, c := range b.StaticResources.Clusters {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the bootstrap has no cluster named %q", name)
	return dispatchCluster{}
}

// endpoint is the cluster's single static endpoint. These clusters all have
// exactly one; more than one would mean the manifest grew a shape this test no
// longer understands.
func (c dispatchCluster) endpoint(t *testing.T) dispatchEndpoint {
	t.Helper()
	if len(c.LoadAssignment.Endpoints) != 1 || len(c.LoadAssignment.Endpoints[0].LbEndpoints) != 1 {
		t.Fatalf("cluster %q does not have exactly one endpoint", c.Name)
	}
	return c.LoadAssignment.Endpoints[0].LbEndpoints[0].Endpoint
}

// The snapshot cache serves a node ID, not a connection. A gateway that
// identifies itself as anything else gets an empty configuration and never says
// so; it just waits out initial_fetch_timeout and starts with no listener.
func TestEgressGatewayNodeIDMatchesTheSnapshotCache(t *testing.T) {
	bootstrap := dispatchConfig(t)
	if got := bootstrap.Node.ID; got != egressxds.NodeID {
		t.Errorf("bootstrap node.id = %q, but atenet publishes snapshots for %q", got, egressxds.NodeID)
	}
	if bootstrap.DynamicResources.LdsConfig.Ads == nil {
		t.Error("lds_config does not use ads; the dispatch listener would never be fetched")
	}
	services := bootstrap.DynamicResources.AdsConfig.GrpcServices
	if len(services) != 1 {
		t.Fatalf("ads_config names %d gRPC services, want 1", len(services))
	}
	if got := services[0].EnvoyGrpc.ClusterName; got != "xds_server" {
		t.Errorf("ads_config points at cluster %q, want %q", got, "xds_server")
	}
}

// The CONNECT route is the whole reason the dispatch listener sees any traffic.
// Pointed back at mitm_internal it would still work, and would intercept every
// exempted destination.
func TestEgressConnectRoutesThroughTheDispatchListener(t *testing.T) {
	bootstrap := dispatchConfig(t)
	routes := connectRoutes(t, envoyConfig(t, sdsmintManifest))
	if len(routes) == 0 {
		t.Fatal("found no connect_matcher route; the manifest changed shape and this test is checking nothing")
	}
	for _, r := range routes {
		endpoint := bootstrap.cluster(t, r.Route.Cluster).endpoint(t)
		if got := endpoint.Address.EnvoyInternalAddress.ServerListenerName; got != egressxds.ListenerName {
			t.Errorf("the CONNECT route reaches internal listener %q via cluster %q, but atenet serves %q",
				got, r.Route.Cluster, egressxds.ListenerName)
		}
	}
}

// Envoy dials this port and atenet binds it; they are configured a thousand
// lines apart in the same file.
func TestEgressXdsPortMatchesTheExtProcFlag(t *testing.T) {
	endpoint := dispatchConfig(t).cluster(t, "xds_server").endpoint(t)
	address := endpoint.Address.SocketAddress
	// atenet binds loopback only, so a cluster pointing anywhere else connects
	// to nothing -- and would be reaching outside the pod to fetch the policy
	// decisions for traffic inside it.
	if address.Address != "127.0.0.1" {
		t.Errorf("xds_server dials %q, but atenet serves the dispatch listener on loopback", address.Address)
	}

	const flag = "--port-egress-xds="
	var want string
	for _, arg := range extProcArgs(t) {
		if value, found := strings.CutPrefix(arg, flag); found {
			want = value
		}
	}
	if want == "" {
		t.Fatalf("the ext-proc container passes no %s flag", strings.TrimSuffix(flag, "="))
	}
	if got := strconv.Itoa(address.PortValue); got != want {
		t.Errorf("xds_server dials port %s but ext-proc serves %s", got, want)
	}
}

// A CONNECT crosses two internal listeners before anything reads this state:
// egress_dispatch, then mitm_listener. ONCE propagates across one hop, so it
// would leave the exemption set -- and the actor identity, and the original
// destination -- unreadable exactly where they are needed.
func TestEgressFilterStateSurvivesBothInternalHops(t *testing.T) {
	var listener dispatchListener
	for _, l := range dispatchConfig(t).StaticResources.Listeners {
		if l.Name == "egress" {
			listener = l
		}
	}
	if listener.Name == "" {
		t.Fatal("the bootstrap has no listener named egress")
	}

	keys := map[string]string{}
	for _, chain := range listener.FilterChains {
		for _, filter := range chain.Filters {
			for _, httpFilter := range filter.TypedConfig.HTTPFilters {
				if httpFilter.Name != "envoy.filters.http.set_filter_state" {
					continue
				}
				for _, object := range httpFilter.TypedConfig.OnRequestHeaders {
					keys[object.ObjectKey] = object.SharedWithUpstream
				}
			}
		}
	}

	shared, ok := keys[egress.ExemptionFilterStateKey]
	if !ok {
		t.Fatalf("no set_filter_state object writes %q, so the dispatch listener has nothing to match on",
			egress.ExemptionFilterStateKey)
	}
	if shared != "TRANSITIVE" {
		t.Errorf("%s is shared %s, want TRANSITIVE", egress.ExemptionFilterStateKey, shared)
	}
	for key, shared := range keys {
		if shared != "TRANSITIVE" {
			t.Errorf("%s is shared %s; it will not reach past egress_dispatch", key, shared)
		}
	}
}

// extProcArgs is the command line of the ext-proc container in the
// atenet-egress Deployment.
func extProcArgs(t *testing.T) []string {
	t.Helper()
	manifest, err := os.ReadFile(sdsmintManifest)
	if err != nil {
		t.Fatalf("reading %s: %v", sdsmintManifest, err)
	}
	for _, doc := range strings.Split(string(manifest), "\n---\n") {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name string   `json:"name"`
							Args []string `json:"args"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parsing a document of %s: %v", sdsmintManifest, err)
		}
		if obj.Kind != "Deployment" || obj.Metadata.Name != "atenet-egress" {
			continue
		}
		for _, container := range obj.Spec.Template.Spec.Containers {
			if container.Name == "ext-proc" {
				return container.Args
			}
		}
		t.Fatalf("the atenet-egress Deployment has no ext-proc container")
	}
	t.Fatalf("%s has no Deployment named atenet-egress", sdsmintManifest)
	return nil
}
