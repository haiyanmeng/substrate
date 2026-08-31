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
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const sdsmintContainerName = "sdsmint"

// egressManifestPath is the deployed egress gateway, read by every test here.
const egressManifestPath = "../../../../manifests/ate-install/atenet-egress-with-sdsmint.yaml"

// sniForwardProxyFilter resolves the upstream from the SNI the filter chain
// matched on. A bypass chain without it dials something else.
const sniForwardProxyFilter = "envoy.filters.network.sni_dynamic_forward_proxy"

// TestManifestKeepsTheCAOffTheDataPlane verifies that the MITM CA is only mounted
// on the `sdsmint` container in the `atenet-egress` Deployment.
func TestManifestKeepsTheCAOffTheDataPlane(t *testing.T) {
	pod := egressPodSpec(t)

	// Reached through sdsmint's own flag rather than by volume name, so
	// renaming the volume cannot quietly turn this into a test of nothing.
	caVolume := mountSupplying(t, pod, sdsmintContainerName, sdsmintCAPoolPath(t, pod)).Name

	for _, c := range allContainers(pod) {
		if c.Name == sdsmintContainerName {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.Name == caVolume {
				t.Errorf("container %q mounts the MITM CA volume %q at %s; only %s may see the signing key, and a mount that spreads gives that up without breaking anything else a test could notice",
					c.Name, caVolume, m.MountPath, sdsmintContainerName)
			}
		}
	}
}

// sdsmintCAPoolPath returns the value of the `--ca-pool-path` flag of
// the `sdsmint` container in the `atenet-egress` Deployment.
func sdsmintCAPoolPath(t *testing.T, pod *corev1.PodSpec) string {
	t.Helper()
	args := container(t, pod, sdsmintContainerName).Args
	cmd := NewSdsmintCmd()
	if len(args) == 0 || args[0] != cmd.Name() {
		t.Fatalf("the %s container's args are %v; they should invoke the %q subcommand", sdsmintContainerName, args, cmd.Name())
	}
	if err := cmd.ParseFlags(args[1:]); err != nil {
		t.Fatalf("the %s container's flags are not ones the binary accepts: %v", sdsmintContainerName, err)
	}
	path, err := cmd.Flags().GetString("ca-pool-path")
	if err != nil {
		t.Fatalf("reading --ca-pool-path from the manifest args: %v", err)
	}
	if path == "" {
		t.Fatal("the manifest passes no --ca-pool-path; sdsmint requires it")
	}
	return path
}

// TestBypassChainsResolveTheNameTheyMatched guards the one thing that turns a
// destination allowlist back into a global opt-out. A chain selected by SNI and
// pointed at an ORIGINAL_DST cluster matches on one actor-controlled value and
// dials another: the actor sends an allowlisted SNI, aims the socket at its own
// server, and reaches it with interception off. Resolving the matched name is
// what keeps the two from disagreeing, and nothing else in the config notices
// if it stops happening -- traffic still flows, to the wrong place.
func TestBypassChainsResolveTheNameTheyMatched(t *testing.T) {
	cfg := egressEnvoyConfig(t)

	originalDst := map[string]bool{}
	for _, c := range cfg.StaticResources.Clusters {
		if c.Type == "ORIGINAL_DST" {
			originalDst[c.Name] = true
		}
	}

	for _, listener := range cfg.StaticResources.Listeners {
		for i, chain := range listener.FilterChains {
			// A nil ServerNames is a chain that does not select on SNI at all.
			// An empty one is the shipped bypass chain, which selects on SNI and
			// currently names nothing -- still subject to this.
			if chain.FilterChainMatch.ServerNames == nil {
				continue
			}
			var sawSNIResolver bool
			for _, f := range chain.Filters {
				if f.Name == sniForwardProxyFilter {
					sawSNIResolver = true
				}
				cluster, _ := f.TypedConfig["cluster"].(string)
				if originalDst[cluster] {
					t.Errorf("listener %s filter chain %d selects on SNI %v but forwards to %q, an ORIGINAL_DST cluster; it would dial the address the actor's socket names rather than the name that was policed, so any actor could reach any destination with MITM off by sending an allowlisted SNI",
						listener.Name, i, chain.FilterChainMatch.ServerNames, cluster)
				}
			}
			if !sawSNIResolver {
				t.Errorf("listener %s filter chain %d selects on SNI %v but has no %s filter; without it the upstream comes from somewhere other than the name that was matched",
					listener.Name, i, chain.FilterChainMatch.ServerNames, sniForwardProxyFilter)
			}
		}
	}
}

// TestBypassChainShipsWithNoDestinations keeps the shipped default at "intercept
// everything". The bypass list is an operator's judgement about origins they
// cannot intercept; a name checked in here would silently exempt it for every
// deployment, and an exemption is invisible in traffic that otherwise looks fine.
func TestBypassChainShipsWithNoDestinations(t *testing.T) {
	cfg := egressEnvoyConfig(t)

	var found bool
	for _, listener := range cfg.StaticResources.Listeners {
		for i, chain := range listener.FilterChains {
			if chain.FilterChainMatch.ServerNames == nil {
				continue
			}
			found = true
			if len(chain.FilterChainMatch.ServerNames) != 0 {
				t.Errorf("listener %s filter chain %d ships with server_names %v; the bypass list must be empty in the repository and set per deployment",
					listener.Name, i, chain.FilterChainMatch.ServerNames)
			}
		}
	}
	if !found {
		t.Error("no filter chain selects on server_names; the MITM bypass chain is gone, and with it the only way to reach an origin whose certificate the actor pins")
	}
}

// egressEnvoyConfig is the Envoy bootstrap the egress gateway runs, read out of
// the ConfigMap that supplies it.
func egressEnvoyConfig(t *testing.T) *envoyBootstrap {
	t.Helper()
	for _, doc := range egressManifestDocs(t) {
		var cm corev1.ConfigMap
		if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
			continue // not a ConfigMap; egressManifestDocs covers the whole file
		}
		if cm.Kind != "ConfigMap" {
			continue
		}
		raw, ok := cm.Data["envoy.yaml"]
		if !ok {
			continue
		}
		var cfg envoyBootstrap
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("parsing envoy.yaml out of ConfigMap %s: %v", cm.Name, err)
		}
		return &cfg
	}
	t.Fatalf("%s has no ConfigMap carrying an envoy.yaml key", egressManifestPath)
	return nil
}

// envoyBootstrap is the slice of the bootstrap these tests assert on. Envoy's
// own Go types would pull the whole go-control-plane API in to read four fields,
// and would reject the file over any unrelated field this package does not
// otherwise care about.
type envoyBootstrap struct {
	StaticResources struct {
		Listeners []struct {
			Name         string `json:"name"`
			FilterChains []struct {
				FilterChainMatch struct {
					TransportProtocol string   `json:"transport_protocol"`
					ServerNames       []string `json:"server_names"`
				} `json:"filter_chain_match"`
				Filters []struct {
					Name        string         `json:"name"`
					TypedConfig map[string]any `json:"typed_config"`
				} `json:"filters"`
			} `json:"filter_chains"`
		} `json:"listeners"`
		Clusters []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"clusters"`
	} `json:"static_resources"`
}

// egressManifestDocs is the manifest split into its YAML documents.
func egressManifestDocs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(egressManifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", egressManifestPath, err)
	}
	return strings.Split(string(raw), "\n---\n")
}

func egressPodSpec(t *testing.T) *corev1.PodSpec {
	t.Helper()
	raw, err := os.ReadFile(egressManifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", egressManifestPath, err)
	}
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("parsing a document of %s: %v", egressManifestPath, err)
		}
		if head.Kind != "Deployment" || head.Metadata.Name != "atenet-egress" {
			continue
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &deployment); err != nil {
			t.Fatalf("decoding the atenet-egress Deployment from %s: %v", egressManifestPath, err)
		}
		return &deployment.Spec.Template.Spec
	}
	t.Fatalf("%s has no Deployment named atenet-egress", egressManifestPath)
	return nil
}

// allContainers is every container in the pod.
func allContainers(pod *corev1.PodSpec) []corev1.Container {
	return append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...)
}

func container(t *testing.T, pod *corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range allContainers(pod) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the egress pod has no container named %q", name)
	return corev1.Container{}
}

// mountSupplying returns the mount a container reads file through.
func mountSupplying(t *testing.T, pod *corev1.PodSpec, containerName, file string) corev1.VolumeMount {
	t.Helper()
	for _, m := range container(t, pod, containerName).VolumeMounts {
		if strings.HasPrefix(file, strings.TrimRight(m.MountPath, "/")+"/") {
			return m
		}
	}
	t.Fatalf("container %q mounts nothing that would supply %s", containerName, file)
	return corev1.VolumeMount{}
}
