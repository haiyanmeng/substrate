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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"go.ate.dev/substrate/cmd/ate-setup/internal/config"
	"go.ate.dev/substrate/cmd/ate-setup/internal/kube"
	"go.ate.dev/substrate/cmd/ate-setup/internal/kustomize"
	"go.ate.dev/substrate/cmd/ate-setup/internal/log"
)

// renderedFile is one file RenderAteSystem writes. Applying the files in name
// order applies the objects in the order deploy ate-system does.
type renderedFile struct {
	name string
	objs []*unstructured.Unstructured
}

// RenderAteSystem writes the manifests deploy ate-system would apply to dir,
// without contacting a cluster. dir must be empty or absent, so no file from
// an earlier render is left beside the new ones.
//
// The output leaves out what deploy generates or reads on the cluster: the CA
// and JWT signing pools, the egress MITM CA, and the identities deploy creates
// in the bundled PostgreSQL. Settings deploy would read from the cluster come
// from the configuration alone. docs/operator-install.md lists what is left out.
func (e *Env) RenderAteSystem(ctx context.Context, dir string) error {
	log.Step("render_ate_system")

	if err := e.RequireCanonicalNamespace("render ate-system"); err != nil {
		return err
	}
	provider, err := e.Cfg.CredentialProvider()
	if err != nil {
		return err
	}
	if err := e.Cfg.RequireIngressAllowedSPIFFEIDs(); err != nil {
		return err
	}
	if err := e.Cfg.RequireTransferServiceAccount(); err != nil {
		return err
	}
	snapshotLocation, err := clusterSnapshotLocation(e.Cfg.SnapshotLocation, e.Cfg.PreviewGates)
	if err != nil {
		return err
	}
	if dir == "" {
		return fmt.Errorf("--output-dir is empty")
	}
	if err := requireEmptyDir(dir); err != nil {
		return err
	}

	cloudsql := cloudSQLSettingsFrom(e.Cfg.CloudSQL, nil)
	if cloudsql.Instance != "" {
		cloudsql.GSA = e.Cfg.CloudSQL.GSA
	}

	settings, hash, err := e.apiServerSettingsObjects(cloudsql, snapshotLocation)
	if err != nil {
		return err
	}
	bundledPostgres := e.Cfg.PostgresReadWriteConnectionString == "" && cloudsql.Instance == ""

	var files []renderedFile
	for _, f := range []struct {
		name   string
		render func() ([]*unstructured.Unstructured, error)
	}{
		{"00-namespace.yaml", func() ([]*unstructured.Unstructured, error) {
			return kube.LoadPath(e.Cfg.Manifest("ate-system-namespace.yaml"))
		}},
		{"10-crds.yaml", func() ([]*unstructured.Unstructured, error) { return e.crdObjects(ctx) }},
		{"20-settings.yaml", func() ([]*unstructured.Unstructured, error) { return settings, nil }},
		{"30-podcertificate-controller.yaml", func() ([]*unstructured.Unstructured, error) {
			return e.podCertificateControllerObjects(ctx)
		}},
		{"40-sandboxconfig.yaml", e.sandboxConfigObjects},
		{"50-postgres.yaml", func() ([]*unstructured.Unstructured, error) {
			if !bundledPostgres {
				return nil, nil
			}
			return e.postgresObjects()
		}},
		{"60-control-plane.yaml", func() ([]*unstructured.Unstructured, error) {
			return e.controlPlaneObjects(ctx, hash, cloudsql)
		}},
		{"70-egress.yaml", func() ([]*unstructured.Unstructured, error) { return e.egressObjects(ctx, provider) }},
	} {
		objs, err := f.render()
		if err != nil {
			return fmt.Errorf("while rendering %s: %w", f.name, err)
		}
		if len(objs) > 0 {
			files = append(files, renderedFile{f.name, objs})
		}
	}
	return writeRenderedFiles(dir, files)
}

// requireEmptyDir fails unless dir is absent or an empty directory.
func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("output directory %s is not empty", dir)
	}
	return nil
}

// sandboxConfigObjects is what DeploySandboxConfig applies.
func (e *Env) sandboxConfigObjects() ([]*unstructured.Unstructured, error) {
	validation, err := kube.LoadPath(e.Cfg.Manifest("sandboxconfig-validation.yaml"))
	if err != nil {
		return nil, err
	}
	gvisor, err := kube.LoadPath(e.Cfg.Manifest("sandboxconfig-gvisor.yaml"))
	if err != nil {
		return nil, err
	}
	return append(validation, gvisor...), nil
}

// crdObjects is what DeployCRDs applies: the generated CRDs and RBAC.
func (e *Env) crdObjects(ctx context.Context) ([]*unstructured.Unstructured, error) {
	manifest, err := e.ResolveManifest(ctx, e.Cfg.Manifest("generated"))
	if err != nil {
		return nil, err
	}
	return kube.DecodeManifestBytes(manifest)
}

// apiServerSettingsObjects returns the objects EnsureAPIServerPrerequisites
// writes for ate-api-server's settings, built from the configuration alone,
// and the digest of ate-api-server's environment. The service account issuer
// defaults to kubeadm's on kind, and to the in-cluster one elsewhere.
func (e *Env) apiServerSettingsObjects(cloudsql cloudSQLSettings, snapshotLocation string) ([]*unstructured.Unstructured, string, error) {
	readWriteDSN := e.Cfg.PostgresReadWriteConnectionString
	ownerDSN := e.Cfg.PostgresOwnerConnectionString
	if readWriteDSN == "" {
		var err error
		if cloudsql.Instance != "" {
			readWriteDSN, err = cloudSQLDSN(cloudsql)
		} else {
			readWriteDSN, ownerDSN, err = e.postgresReadWriteConnectionStrings()
		}
		if err != nil {
			return nil, "", err
		}
	}
	if ownerDSN == "" {
		ownerDSN = readWriteDSN
	}
	configVars := e.apiServerConfigVars(cloudsql, e.Cfg.PostgresReadWriteRole, e.Cfg.PostgresOwnerRole,
		e.Cfg.PostgresPoolMaxConns, snapshotLocation)
	secretVars := buildAPIServerEnvVars(readWriteDSN, ownerDSN, e.Cfg.PostgresSchemaName())

	apiConfig, err := e.apiConfig()
	if err != nil {
		return nil, "", err
	}
	issuer := configuredJWTIssuer(e.Cfg)
	if issuer == "" && e.Cfg.Kind {
		issuer = kindIssuer
	} else if issuer == "" {
		issuer = inClusterIssuer
	}

	ns := e.Namespace()
	objs := []*unstructured.Unstructured{
		configMapObject(ns, ConfigMapAPIEnvVars, configVars),
		secretObject(ns, SecretAPIEnvVars, secretVars),
		configMapObject(ns, ConfigMapAPIConfig, map[string]string{"apiconfig.textproto": apiConfig}),
		configMapObject(ns, ConfigMapAPIAuthn, map[string]string{"authentication.yaml": buildAuthenticationConfig(issuer)}),
	}
	serverCA, err := e.postgresServerCA()
	if err != nil {
		return nil, "", err
	}
	if serverCA != nil {
		objs = append(objs, secretObject(ns, SecretPostgresServerCA, serverCA))
	}

	secretData := make(map[string][]byte, len(secretVars))
	for k, v := range secretVars {
		secretData[k] = []byte(v)
	}
	return objs, envHash(configVars, secretData), nil
}

// kindIssuer is the service account issuer kind clusters use, kubeadm's
// default.
const kindIssuer = inClusterIssuer + ".cluster.local"

// configMapObject builds a ConfigMap holding data.
func configMapObject(namespace, name string, data map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"data":       stringMap(data),
	}}
}

// secretObject builds an Opaque Secret holding data, as kube.ApplySecret does.
func secretObject(namespace, name string, data map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"type":       "Opaque",
		"stringData": stringMap(data),
	}}
}

// stringMap converts data to the map type unstructured objects hold.
func stringMap(data map[string]string) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	return out
}

// controlPlaneObjects is the bundle deploy ate-system applies, with the
// changes deploy makes to it on the cluster made to the objects instead: the
// environment digest, the telemetry endpoint and the Cloud SQL proxy sidecar.
func (e *Env) controlPlaneObjects(ctx context.Context, hash string, cloudsql cloudSQLSettings) ([]*unstructured.Unstructured, error) {
	manifest, err := e.renderSystemManifests(ctx)
	if err != nil {
		return nil, err
	}
	if cloudsql.Instance != "" {
		if manifest, err = e.addCloudSQLProxySidecar(manifest); err != nil {
			return nil, err
		}
	}
	objs, err := kube.DecodeManifestBytes(manifest)
	if err != nil {
		return nil, err
	}
	if err := e.finishAPIServer(objs, hash); err != nil {
		return nil, err
	}
	if cloudsql.GSA != "" {
		if err := setAnnotation(objs, "ServiceAccount", "ate-api-server", workloadIdentityAnnotation, cloudsql.GSA); err != nil {
			return nil, err
		}
	}
	if endpoint := e.Cfg.OtlpEndpoint; endpoint != "" {
		if err := setConfigMapKey(objs, otelConfigMap, otelEndpointKey, endpoint); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

// addCloudSQLProxySidecar applies the patch reconcileCloudSQLProxySidecar
// sends to the cluster to the ate-api-server Deployment in manifest.
func (e *Env) addCloudSQLProxySidecar(manifest []byte) ([]byte, error) {
	patch, err := os.ReadFile(e.Cfg.Manifest("cloudsql", "proxy-sidecar-patch.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading the Cloud SQL proxy sidecar patch: %w", err)
	}
	dir, err := os.MkdirTemp("", "ate-setup-kustomize-")
	if err != nil {
		return nil, fmt.Errorf("while creating a scratch kustomization: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// The patch file holds only the spec, so name the object it patches.
	header := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: ate-api-server\n  namespace: ate-system\n"
	for name, content := range map[string][]byte{
		"manifest.yaml": manifest,
		"patch.yaml":    append([]byte(header), patch...),
		"kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources:\n  - manifest.yaml\npatches:\n  - path: patch.yaml\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return nil, fmt.Errorf("while writing a scratch kustomization: %w", err)
		}
	}
	return kustomize.Build(dir)
}

// setAnnotation sets an annotation on the object of kind and name in objs.
func setAnnotation(objs []*unstructured.Unstructured, kind, name, key, value string) error {
	for _, obj := range objs {
		if obj.GetKind() != kind || obj.GetName() != name {
			continue
		}
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[key] = value
		obj.SetAnnotations(annotations)
		return nil
	}
	return fmt.Errorf("the manifest has no %s/%s to annotate with %s", strings.ToLower(kind), name, key)
}

// setConfigMapKey sets key in the data of the named ConfigMap in objs.
func setConfigMapKey(objs []*unstructured.Unstructured, name, key, value string) error {
	for _, obj := range objs {
		if obj.GetKind() != "ConfigMap" || obj.GetName() != name {
			continue
		}
		return unstructured.SetNestedField(obj.Object, value, "data", key)
	}
	return fmt.Errorf("the manifest has no configmap/%s to set %s in", name, key)
}

// egressObjects is what deploy ate-system applies for egress: the bundled
// credential provider when it is selected, and the egress gateway.
func (e *Env) egressObjects(ctx context.Context, provider config.CredentialProvider) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	if provider.Kubernetes() {
		policy, err := kube.LoadPath(e.k8sCredentialProviderPath(k8sCredentialProviderPolicyManifest))
		if err != nil {
			return nil, err
		}
		manifest, err := e.renderResolve(ctx, e.k8sCredentialProviderPath(k8sCredentialProviderManifest))
		if err != nil {
			return nil, err
		}
		providerObjs, err := kube.DecodeManifestBytes(manifest)
		if err != nil {
			return nil, err
		}
		objs = append(append(objs, policy...), providerObjs...)
	}
	manifest, err := e.renderAtenetEgressManifest(ctx, provider)
	if err != nil {
		return nil, err
	}
	egress, err := kube.DecodeManifestBytes(manifest)
	if err != nil {
		return nil, err
	}
	return append(objs, egress...), nil
}

// writeRenderedFiles writes files to dir. The files are private to the user,
// because the settings carry the PostgreSQL connection strings.
func writeRenderedFiles(dir string, files []renderedFile) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		var b bytes.Buffer
		for i, obj := range f.objs {
			if i > 0 {
				b.WriteString("---\n")
			}
			out, err := yaml.Marshal(obj.Object)
			if err != nil {
				return fmt.Errorf("while encoding %s in %s: %w", kube.Describe(obj), f.name, err)
			}
			b.Write(out)
		}
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
			return err
		}
		log.Infof("wrote %s (%d objects)", path, len(f.objs))
	}
	return nil
}
