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

package steps

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.ate.dev/substrate/cmd/ate-setup/internal/config"
	"go.ate.dev/substrate/cmd/ate-setup/internal/images"
	"go.ate.dev/substrate/cmd/ate-setup/internal/kube"
)

// imageLine matches a container image field in a rendered manifest.
var imageLine = regexp.MustCompile(`(?m)^\s*(?:- )?image: "?([^"\s]+)`)

func TestRenderAteSystem(t *testing.T) {
	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	src := images.Source{Repo: "example.com/substrate", Tag: "v1.2.3"}
	all := []string{
		"00-namespace.yaml", "10-crds.yaml", "20-settings.yaml", "30-podcertificate-controller.yaml",
		"40-sandboxconfig.yaml", "50-postgres.yaml", "60-control-plane.yaml", "70-egress.yaml",
	}
	withoutPostgres := slices.DeleteFunc(slices.Clone(all), func(f string) bool { return f == "50-postgres.yaml" })

	for _, tc := range []struct {
		name      string
		cfg       func(*config.Config)
		wantFiles []string
		check     func(t *testing.T, dir string)
	}{
		{
			name:      "defaults",
			cfg:       func(*config.Config) {},
			wantFiles: all,
			check: func(t *testing.T, dir string) {
				authn := renderedObject(t, dir, "20-settings.yaml", "ConfigMap", ConfigMapAPIAuthn)
				if got, _, _ := unstructured.NestedString(authn.Object, "data", "authentication.yaml"); got != buildAuthenticationConfig(inClusterIssuer) {
					t.Errorf("authentication.yaml = %q, want the in-cluster issuer", got)
				}
			},
		},
		{
			name:      "kind with agentgateway",
			cfg:       func(c *config.Config) { c.Kind, c.Router = true, config.RouterAgentgateway },
			wantFiles: all,
			check: func(t *testing.T, dir string) {
				authn := renderedObject(t, dir, "20-settings.yaml", "ConfigMap", ConfigMapAPIAuthn)
				if got, _, _ := unstructured.NestedString(authn.Object, "data", "authentication.yaml"); got != buildAuthenticationConfig(kindIssuer) {
					t.Errorf("authentication.yaml = %q, want the kind issuer", got)
				}
			},
		},
		{
			name: "external database",
			cfg: func(c *config.Config) {
				c.PostgresReadWriteConnectionString = "postgresql://rw@db.example.com/atepg"
				c.ExpectedJWTIssuer = "https://issuer.example.com"
				c.CloudSQL.GSA = "ateapi@p.iam.gserviceaccount.com"
			},
			wantFiles: withoutPostgres,
			check: func(t *testing.T, dir string) {
				secret := renderedObject(t, dir, "20-settings.yaml", "Secret", SecretAPIEnvVars)
				if got, _, _ := unstructured.NestedString(secret.Object, "stringData", "ATE_API_POSTGRES_OWNER_CONNECTION_STRING"); got != "postgresql://rw@db.example.com/atepg" {
					t.Errorf("owner connection string = %q, want the read-write one", got)
				}
				authn := renderedObject(t, dir, "20-settings.yaml", "ConfigMap", ConfigMapAPIAuthn)
				if got, _, _ := unstructured.NestedString(authn.Object, "data", "authentication.yaml"); !strings.Contains(got, "issuer: https://issuer.example.com\n") {
					t.Errorf("authentication.yaml = %q, want the configured issuer", got)
				}
				sa := renderedObject(t, dir, "60-control-plane.yaml", "ServiceAccount", "ate-api-server")
				if got, ok := sa.GetAnnotations()[workloadIdentityAnnotation]; ok {
					t.Errorf("ate-api-server ServiceAccount %s = %q without Cloud SQL, want none", workloadIdentityAnnotation, got)
				}
			},
		},
		{
			name: "cloud sql",
			cfg: func(c *config.Config) {
				c.CloudSQL = config.CloudSQLConfig{Instance: "p:r:i", InstanceSet: true, GSA: "ateapi@p.iam.gserviceaccount.com"}
			},
			wantFiles: withoutPostgres,
			check: func(t *testing.T, dir string) {
				dep := renderedObject(t, dir, "60-control-plane.yaml", "Deployment", "ate-api-server")
				initContainers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
				if !slices.ContainsFunc(initContainers, func(c any) bool {
					return c.(map[string]any)["name"] == cloudSQLProxyContainer
				}) {
					t.Errorf("ate-api-server has no %s sidecar", cloudSQLProxyContainer)
				}
				sa := renderedObject(t, dir, "60-control-plane.yaml", "ServiceAccount", "ate-api-server")
				if got := sa.GetAnnotations()[workloadIdentityAnnotation]; got != "ateapi@p.iam.gserviceaccount.com" {
					t.Errorf("ate-api-server ServiceAccount %s = %q, want the GSA", workloadIdentityAnnotation, got)
				}
			},
		},
		{
			name:      "component",
			cfg:       func(c *config.Config) { c.Components = []string{writeComponent(t, "provider", "")} },
			wantFiles: all,
			check: func(t *testing.T, dir string) {
				for _, w := range []struct{ file, kind, name string }{
					{"30-podcertificate-controller.yaml", "Deployment", "podcertificate-controller"},
					{"50-postgres.yaml", "StatefulSet", "postgres"},
					{"60-control-plane.yaml", "Deployment", "ate-api-server"},
					{"70-egress.yaml", "Deployment", "atenet-egress"},
				} {
					if got := renderedObject(t, dir, w.file, w.kind, w.name).GetLabels()["test.ate.dev/component"]; got != "provider" {
						t.Errorf("%s %s/%s has component label %q, want %q", w.file, w.kind, w.name, got, "provider")
					}
				}
			},
		},
		{
			name:      "telemetry endpoint",
			cfg:       func(c *config.Config) { c.OtlpEndpoint = "http://collector.example.com:4317" },
			wantFiles: all,
			check: func(t *testing.T, dir string) {
				cm := renderedObject(t, dir, "60-control-plane.yaml", "ConfigMap", otelConfigMap)
				if got, _, _ := unstructured.NestedString(cm.Object, "data", otelEndpointKey); got != "http://collector.example.com:4317" {
					t.Errorf("%s = %q, want the configured endpoint", otelEndpointKey, got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Root:                    repoRoot(t),
				Router:                  config.RouterEnvoy,
				Images:                  src,
				IngressAllowedSPIFFEIDs: []string{"spiffe://cluster.local/ns/a/sa/a"},
				SnapshotLocation:        "gs://bucket",
				TransferGSAName:         "transfer",
				ProjectID:               "p",
				ActorJWTIssuer:          "https://idp.ate-system.svc",
				PostgresReadWriteRole:   config.DefaultPostgresReadWriteRole,
				PostgresOwnerRole:       config.DefaultPostgresOwnerRole,
				CredentialProviderJSON:  `{"name":"k8s.io"}`,
			}
			tc.cfg(&cfg)
			e := &Env{Cfg: &cfg, resolver: images.NewPrebuilt(src, func(context.Context, string) (string, error) {
				return digest, nil
			})}
			dir := filepath.Join(t.TempDir(), "out")
			if err := e.RenderAteSystem(t.Context(), dir); err != nil {
				t.Fatalf("RenderAteSystem() error = %v", err)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, entry := range entries {
				files = append(files, entry.Name())
				out, err := os.ReadFile(filepath.Join(dir, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				for _, leftover := range []string{"ko://", "${ENVOY_DATAPLANE_IMAGE}"} {
					if strings.Contains(string(out), leftover) {
						t.Errorf("%s still contains %q", entry.Name(), leftover)
					}
				}
				for _, m := range imageLine.FindAllStringSubmatch(string(out), -1) {
					if !strings.Contains(m[1], "@sha256:") {
						t.Errorf("%s installs %s, which is not pinned to a digest", entry.Name(), m[1])
					}
				}
			}
			if !slices.Equal(files, tc.wantFiles) {
				t.Errorf("files = %q, want %q", files, tc.wantFiles)
			}

			// The settings are the ones deploy writes, and the digest on the
			// Deployment is the one deploy computes from them, so applying the
			// render and then deploying rolls nothing.
			cm := renderedObject(t, dir, "20-settings.yaml", "ConfigMap", ConfigMapAPIEnvVars)
			secret := renderedObject(t, dir, "20-settings.yaml", "Secret", SecretAPIEnvVars)
			cmData, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			secretStrings, _, _ := unstructured.NestedStringMap(secret.Object, "stringData")
			deployed := &Env{Cfg: &cfg, Kube: fakeKube(t,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: NamespaceAteSystem}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapAPIEnvVars, Namespace: NamespaceAteSystem}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretAPIEnvVars, Namespace: NamespaceAteSystem}},
			)}
			if err := deployed.CreateAPIServerEnvVars(t.Context()); err != nil {
				t.Fatalf("CreateAPIServerEnvVars() error = %v", err)
			}
			deployedCM, err := deployed.Kube.GetConfigMap(t.Context(), NamespaceAteSystem, ConfigMapAPIEnvVars)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(cmData, deployedCM.Data) {
				t.Errorf("rendered %s = %v, deploy writes %v", ConfigMapAPIEnvVars, cmData, deployedCM.Data)
			}
			deployedSecret, err := deployed.Kube.GetSecret(t.Context(), NamespaceAteSystem, SecretAPIEnvVars)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(secretStrings, deployedSecret.StringData) {
				t.Errorf("rendered %s = %v, deploy writes %v", SecretAPIEnvVars, secretStrings, deployedSecret.StringData)
			}
			secretData := map[string][]byte{}
			for k, v := range secretStrings {
				secretData[k] = []byte(v)
			}
			dep := renderedObject(t, dir, "60-control-plane.yaml", "Deployment", "ate-api-server")
			if got, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", envHashAnnotation); got != envHash(cmData, secretData) {
				t.Errorf("ate-api-server %s = %q, want the digest of the rendered settings", envHashAnnotation, got)
			}

			if tc.check != nil {
				tc.check(t, dir)
			}
		})
	}
}

// RenderAteSystem refuses a directory that holds files, which would otherwise
// be applied beside the new render.
func TestRenderAteSystemRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireEmptyDir(dir); err == nil {
		t.Error("requireEmptyDir() = nil for a directory with a file, want an error")
	}
	if err := requireEmptyDir(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("requireEmptyDir() = %v for an absent directory, want nil", err)
	}
}

// renderedObject returns the object of kind and name in the rendered file.
func renderedObject(t *testing.T, dir, file, kind, name string) *unstructured.Unstructured {
	t.Helper()
	objs, err := kube.LoadPath(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	obj := findObject(objs, kind, name)
	if obj == nil {
		t.Fatalf("%s has no %s/%s", file, kind, name)
	}
	return obj
}
