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

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/e2e/fixtures/egressprobe/probeapi"
	"github.com/agent-substrate/substrate/internal/portforward"
)

// egressProbeName is the Pod, Service and container name in the probe's
// manifest template.
const egressProbeName = "egressprobe"

// EgressProbe drives the in-cluster egress probe over a port-forward. It is the
// only way a test process can reach the egress gateway's front door, which
// requires a client certificate that only pods are issued.
type EgressProbe struct {
	baseURL string
	http    *http.Client
}

// StartEgressProbe builds and deploys the egress probe into ns, waits for it to
// be ready, and returns a client for it. Teardown rides on the namespace.
func StartEgressProbe(t *testing.T, ctx context.Context, ns string) *EgressProbe {
	t.Helper()
	if _, err := CheckEnv("KO_DOCKER_REPO"); err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}
	root, err := FindRepoRoot()
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}

	tmpl, err := os.ReadFile(filepath.Join(root, "internal/e2e/fixtures/egressprobe/egressprobe.yaml.tmpl"))
	if err != nil {
		t.Fatalf("reading egressprobe manifest template: %v", err)
	}
	manifest := filepath.Join(t.TempDir(), "egressprobe.yaml")
	rendered := strings.ReplaceAll(string(tmpl), "${NAMESPACE}", ns)
	if err := os.WriteFile(manifest, []byte(rendered), 0o644); err != nil {
		t.Fatalf("writing rendered egressprobe manifest: %v", err)
	}

	// Same invocation as the identity suite's probe: the repo's pinned ko via
	// hack/run-tool.sh, with KO_CONFIG_PATH pointing at the repo root because
	// ko resolves .ko.yaml relative to its working directory, which here is the
	// test's package dir. Without it the build loses defaultPlatforms.
	applyArgs := []string{"ko", "apply", "-f", manifest}
	if KubeContext != "" {
		applyArgs = append(applyArgs, "--", "--context="+KubeContext)
	}
	RunCmdWithEnv(t, []string{"KO_CONFIG_PATH=" + root}, filepath.Join(root, "hack/run-tool.sh"), applyArgs...)

	waitForEgressProbeReady(t, ctx, ns)

	config, err := ateclient.LoadConfig(KubeConfig, KubeContext)
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("creating k8s client for port-forward: %v", err)
	}
	localPort, stop, err := portforward.ServicePortForward(ctx, config, clientset, ns, egressProbeName, 8080)
	if err != nil {
		t.Fatalf("port-forwarding %s/%s: %v", ns, egressProbeName, err)
	}
	t.Cleanup(stop)

	return &EgressProbe{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", localPort),
		// Comfortably above the probe's own --handshake-timeout, so a gateway
		// that stalls is reported as the probe's tunnel error rather than as a
		// client timeout with nothing to point at.
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

// Probe asks the probe for one trip through the gateway. A denied CONNECT or a
// refused SNI comes back as a Result, not as an error: refusal is one of the
// outcomes under test. Only a probe that could not be reached or understood
// fails the test here.
func (p *EgressProbe) Probe(t *testing.T, ctx context.Context, req probeapi.Request) probeapi.Result {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encoding probe request: %v", err)
	}
	// Logged without the credential, which is several kilobytes of base64 and
	// says nothing a failure needs; the identity it carries is named by whatever
	// minted it.
	t.Logf("probe: destination=%q sni=%q credential=%t", req.Destination, req.SNI, req.ClientCredentialPEM != "")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/probe", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building probe request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.http.Do(httpReq)
	if err != nil {
		t.Fatalf("calling probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		t.Fatalf("probe returned %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out probeapi.Result
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding probe response: %v", err)
	}
	return out
}

func waitForEgressProbeReady(t *testing.T, ctx context.Context, ns string) {
	t.Helper()
	const timeout = 3 * time.Minute
	deadline := time.Now().Add(timeout)
	// Kept so the timeout can say why the pod never came up. Without it the
	// failure is just "timed out", and the pod is in a namespace the harness
	// retains but nobody thinks to look in.
	var lastState string
	for time.Now().Before(deadline) {
		pod, err := GetClients().K8s.CoreV1().Pods(ns).Get(ctx, egressProbeName, metav1.GetOptions{})
		switch {
		case err != nil:
			lastState = err.Error()
		case portforward.IsPodReady(pod):
			t.Logf("probe pod %s/%s is ready", ns, egressProbeName)
			return
		default:
			lastState = describeEgressProbeState(pod)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %v waiting for probe pod %s/%s to become ready: %s", timeout, ns, egressProbeName, lastState)
}

func describeEgressProbeState(pod *corev1.Pod) string {
	parts := []string{"phase=" + string(pod.Status.Phase)}
	for _, cs := range pod.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil:
			parts = append(parts, fmt.Sprintf("%s waiting: %s: %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
		case cs.State.Terminated != nil:
			parts = append(parts, fmt.Sprintf("%s terminated: %s: %s", cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.Message))
		default:
			parts = append(parts, fmt.Sprintf("%s running, ready=%t", cs.Name, cs.Ready))
		}
	}
	return strings.Join(parts, "; ")
}
