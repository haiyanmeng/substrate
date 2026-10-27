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

package cmd

import (
	"github.com/spf13/cobra"
)

var renderCmd = &cobra.Command{
	Annotations: map[string]string{clusterAnnotation: "no"},
	Use:         "render",
	Short:       "Write the manifests a deploy would apply, without a cluster",
}

// renderOutputDir is where render ate-system writes its files.
var renderOutputDir string

var renderAteSystemCmd = &cobra.Command{
	Use:   "ate-system",
	Short: "Write the manifests deploy ate-system would apply",
	Long: `Write the manifests deploy ate-system would apply to --output-dir, one
file per apply step. Applying the files in name order installs them in the
order deploy does.

Images are pinned to digests: built and pushed from this checkout, or looked up
under --image-repo. The settings ConfigMaps and the ate-api-server-secret-envvars
Secret are written from the configuration alone. render reads nothing from a
cluster, so the service account issuer is --ateapi-expected-jwt-issuer, the one
derived from the GKE cluster coordinates, or a default: kubeadm's under --kind,
https://kubernetes.default.svc elsewhere.

The output leaves out the key pools deploy generates on the cluster and the
bundled PostgreSQL identities. See docs/operator-install.md.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return env.RenderAteSystem(cmd.Context(), renderOutputDir)
	},
}

func init() {
	rootCmd.AddCommand(renderCmd)
	renderCmd.AddCommand(renderAteSystemCmd)
	renderAteSystemCmd.Flags().StringVar(&renderOutputDir, "output-dir", "",
		"Directory to write the manifests to. It must be empty or absent")
	_ = renderAteSystemCmd.MarkFlagRequired("output-dir")
}
