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

// Package sdsmint e2e-tests the egress gateway's certificate minter.
//
// sdsmintd is an SDS server that mints a leaf certificate on demand for the
// SNI Envoy was asked for. Its unit tests cover the SDS protocol against a fake
// Envoy; what they cannot cover is the part that has actually broken in
// practice -- whether the deployed pod's Envoy, CA pool secret, allowlist and
// unix socket line up. Every assertion here is made on a certificate that came
// off a real handshake through the real gateway.
//
// The gateway is dormant: nothing sets EgressGatewayAddress, so no actor is
// pointed at it. That is exactly why this suite exists. Without it the only
// signal that the gateway still works is that the pod is Running, and the pod
// is Running whether or not the allowlist, the CA or the SDS socket is intact.
package sdsmint

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/localca"
	"github.com/agent-substrate/substrate/internal/portforward"
)

const (
	// Where the gateway and its CA live. Both are fixed by
	// manifests/ate-install/atenet-egress.yaml and hack/install-ate.sh.
	egressNamespace = "ate-system"
	mitmCASecret    = "egress-mitm-ca-pool"
	mitmCASecretKey = "pool"
	mitmCAID        = "mitm"

	// leafTTL is --ttl on the sdsmintd sidecar, and leafSkew is the backdating
	// internal/sdsmint/ca.go applies to NotBefore. Their sum is the validity
	// span every leaf should carry. Keep both in step with the manifest: a leaf
	// that suddenly lasts hours is the failure this pair is here to catch.
	leafTTL  = 5 * time.Minute
	leafSkew = 1 * time.Minute

	probeName = "egressprobe"
)

// TestSdsmintMintsALeafPerSNI is the core functional assertion: two different
// SNIs through one gateway come back as two different certificates, each issued
// for the name that was asked for and each chaining to the MITM CA.
//
// The SNIs are derived from the test's own namespace so that repeated runs use
// names Envoy has never subscribed to. Reusing a name would be served from
// Envoy's live secret set (--idle is 30m) and the test would pass without
// sdsmintd having minted anything.
func TestSdsmintMintsALeafPerSNI(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)

	root := mitmRootCertificate(t, ctx)
	probe := startProbe(t, ctx, ns.Name)

	first := ns.Name + "-a.example.com"
	second := ns.Name + "-b.example.com"

	chains := map[string][]*x509.Certificate{}
	for _, sni := range []string{first, second} {
		result := probe.handshake(t, ctx, sni)
		if !result.OK {
			t.Fatalf("handshake for allowlisted SNI %q failed: %s", sni, result.Error)
		}
		chains[sni] = parseChain(t, sni, result.ChainPEM)
	}

	for sni, chain := range chains {
		leaf := chain[0]

		// Minted for the name that was asked for, and only that name. A leaf
		// carrying anything else means the SNI Envoy policed is not the name
		// the certificate authorizes.
		if got := leaf.DNSNames; len(got) != 1 || got[0] != sni {
			t.Errorf("leaf for %q has DNSNames %v, want exactly [%q]", sni, got, sni)
		}
		if leaf.Subject.CommonName != sni {
			t.Errorf("leaf for %q has CN %q, want %q", sni, leaf.Subject.CommonName, sni)
		}

		// Chains to the MITM root the installer created, verified for the SNI
		// itself so the root's dNSName constraint is applied too.
		opts := x509.VerifyOptions{
			DNSName:       sni,
			Roots:         certPool(root),
			Intermediates: certPool(chain[1:]...),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		if _, err := leaf.Verify(opts); err != nil {
			t.Errorf("leaf for %q does not verify against the %s/%s MITM root: %v", sni, egressNamespace, mitmCASecret, err)
		}

		// Short-lived, which is what bounds the damage from a leaked leaf key
		// and what makes the MITM CA tolerable at all. The window is generous
		// because the point is to catch a TTL that is wrong by an order of
		// magnitude, not clock skew.
		validity := leaf.NotAfter.Sub(leaf.NotBefore)
		if want := leafTTL + leafSkew; validity < want-time.Minute || validity > want+time.Minute {
			t.Errorf("leaf for %q is valid for %v, want about %v (--ttl in atenet-egress.yaml)", sni, validity, want)
		}
		if time.Now().After(leaf.NotAfter) {
			t.Errorf("leaf for %q was already expired when served (NotAfter %s)", sni, leaf.NotAfter)
		}

		// Signed by a delegated intermediate rather than by the root. This is
		// --ca-intermediate-ttl doing its job: the root signs a few times a
		// day instead of once per cache miss.
		if len(chain) < 2 {
			t.Errorf("chain for %q has only the leaf; expected a delegated intermediate", sni)
			continue
		}
		intermediate := chain[1]
		if intermediate.Equal(root) {
			t.Errorf("leaf for %q was signed by the root directly; --ca-intermediate-ttl is not in effect", sni)
		}
		// The backstop under the allowlist. The allowlist is what sdsmintd
		// agrees to mint; this is what the key is cryptographically able to
		// mint at all, and it is what --ca-allow-unconstrained would remove.
		if len(intermediate.PermittedDNSDomains) == 0 {
			t.Errorf("intermediate signing %q carries no dNSName constraint; the MITM CA can sign for any name", sni)
		}

		t.Logf("%s: served a %d-cert chain, leaf serial %s valid %v, issued by %q constrained to %v",
			sni, len(chain), leaf.SerialNumber.Text(16), validity, intermediate.Subject.CommonName, intermediate.PermittedDNSDomains)
	}

	if a, b := chains[first][0], chains[second][0]; a.SerialNumber.Cmp(b.SerialNumber) == 0 {
		t.Errorf("both SNIs were served the same certificate (serial %s); the gateway is not minting per name", a.SerialNumber.Text(16))
	}
}

// TestSdsmintRefusesSNIOutsideTheAllowlist is the security half. --allow is the
// entire egress policy, so an SNI outside it must not produce a certificate --
// sdsmintd withdraws the resource, Envoy has nothing to present, and the
// handshake fails. A pass here that used to fail means the gateway will sign
// for names it was never authorized for.
func TestSdsmintRefusesSNIOutsideTheAllowlist(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)

	probe := startProbe(t, ctx, ns.Name)

	// .invalid can never be delegated (RFC 6761), so this cannot collide with
	// a name someone later adds to the allowlist.
	sni := ns.Name + ".notallowed.invalid"
	result := probe.handshake(t, ctx, sni)
	if result.OK {
		chain := parseChain(t, sni, result.ChainPEM)
		t.Fatalf("gateway served a certificate for non-allowlisted SNI %q (leaf CN %q, DNSNames %v) -- the allowlist is not being enforced",
			sni, chain[0].Subject.CommonName, chain[0].DNSNames)
	}
	t.Logf("non-allowlisted SNI %q was refused as expected: %s", sni, result.Error)
}

// TestWorkloadDoesNotTrustTheMITMCA pins the gap this whole design exists to
// close: the gateway mints a chain that is valid in every respect except that
// nothing in the cluster has been told to trust its anchor, so an ordinary TLS
// client inside a workload rejects it. That is why
// demos/egress/github-poller.yaml.tmpl passes --insecure, and why
// docs/dev/egress-trust-anchor.md is a document rather than a paragraph.
//
// Asserting the negative is worth a test because it is the precondition for
// that work. When Stage 3 lands and actors are handed the anchor, this test
// starts failing, and the failure is the signal to replace it with its inverse
// rather than a regression to debug.
//
// Two things make the assertion mean something:
//
//   - The verification runs in the pod, against the image's own trust store.
//     Running it in the test process would check the CI runner's store, which
//     has never heard of this CA and never will -- it would pass today and keep
//     passing long after actors had been taught to trust the anchor.
//   - The rejection reason is checked, not just the boolean. "Verification
//     failed" is also what an expired leaf or a misissued name produces, and
//     either of those would let this test go green while the real property had
//     silently stopped being tested.
//
// The scope is a pod, not an actor: the probe is not sandboxed and the ko
// distroless base is not an arbitrary actor image. The property is the same one
// an actor sees -- no path exists to install the anchor into either -- but
// proving it for actor images under gVisor belongs to the Stage 3 e2e.
func TestWorkloadDoesNotTrustTheMITMCA(t *testing.T) {
	ctx := context.Background()
	ns := e2e.CreateNamespace(t)

	root := mitmRootCertificate(t, ctx)
	probe := startProbe(t, ctx, ns.Name)

	// Allowlisted, so the gateway mints and the handshake completes. Untrusted
	// has to be shown on a chain that was actually served; a refused SNI proves
	// nothing about trust.
	sni := ns.Name + "-untrusted.example.com"
	result := probe.handshake(t, ctx, sni)
	if !result.OK {
		t.Fatalf("handshake for allowlisted SNI %q failed: %s", sni, result.Error)
	}

	// The chain really is the MITM CA's, and really is well-formed for this
	// name. Without this the test could pass on a chain that was rejected for
	// being garbage.
	chain := parseChain(t, sni, result.ChainPEM)
	opts := x509.VerifyOptions{
		DNSName:       sni,
		Roots:         certPool(root),
		Intermediates: certPool(chain[1:]...),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if _, err := chain[0].Verify(opts); err != nil {
		t.Fatalf("leaf for %q does not verify even against the MITM root: %v", sni, err)
	}

	if result.SystemTrusted {
		t.Fatalf("the probe image trusts the MITM CA: chain for %q verified against the image's own trust store. "+
			"If actors were just taught the anchor (docs/dev/egress-trust-anchor.md), replace this test with its inverse; "+
			"otherwise something is installing the MITM root into workload images.", sni)
	}
	if got := result.SystemVerifyErrorKind; got != "unknown_authority" {
		t.Fatalf("chain for %q was rejected as %q, want %q -- the leaf is being refused for some reason other than an untrusted anchor, "+
			"so this test is no longer checking what it claims: %s", sni, got, "unknown_authority", result.SystemVerifyError)
	}

	t.Logf("%s: gateway served a valid %d-cert MITM chain that the probe image rejects: %s",
		sni, len(chain), result.SystemVerifyError)
}

// mitmRootCertificate reads the trust anchor sdsmintd signs under, straight
// from the secret the sidecar mounts, so the test is checking the chain against
// the CA that is actually deployed rather than one it was told about.
func mitmRootCertificate(t *testing.T, ctx context.Context) *x509.Certificate {
	t.Helper()
	secret, err := e2e.GetClients().K8s.CoreV1().Secrets(egressNamespace).Get(ctx, mitmCASecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading MITM CA pool secret %s/%s: %v", egressNamespace, mitmCASecret, err)
	}
	// This unmarshals the signing key along with the certificate. That is
	// unavoidable -- the pool is one blob -- and acceptable only because this
	// runs against a test cluster with a kubeconfig that could read the secret
	// anyway. Nothing below touches the key.
	pool, err := localca.Unmarshal(secret.Data[mitmCASecretKey])
	if err != nil {
		t.Fatalf("parsing MITM CA pool from %s/%s key %q: %v", egressNamespace, mitmCASecret, mitmCASecretKey, err)
	}
	for _, ca := range pool.CAs {
		if ca.ID == mitmCAID {
			return ca.RootCertificate
		}
	}
	t.Fatalf("MITM CA pool %s/%s has no CA with id %q", egressNamespace, mitmCASecret, mitmCAID)
	return nil
}

// probeClient talks to the in-cluster egress probe over a port-forward.
type probeClient struct {
	baseURL string
	http    *http.Client
}

// startProbe builds and deploys the probe into ns, waits for it to be ready,
// and returns a client for it. Teardown rides on the namespace.
func startProbe(t *testing.T, ctx context.Context, ns string) *probeClient {
	t.Helper()
	if _, err := e2e.CheckEnv("KO_DOCKER_REPO"); err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}
	root, err := e2e.FindRepoRoot()
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
	if e2e.KubeContext != "" {
		applyArgs = append(applyArgs, "--", "--context="+e2e.KubeContext)
	}
	e2e.RunCmdWithEnv(t, []string{"KO_CONFIG_PATH=" + root}, filepath.Join(root, "hack/run-tool.sh"), applyArgs...)

	waitForProbeReady(t, ctx, ns)

	config, err := ateclient.LoadConfig(e2e.KubeConfig, e2e.KubeContext)
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("creating k8s client for port-forward: %v", err)
	}
	localPort, stop, err := portforward.ServicePortForward(ctx, config, clientset, ns, probeName, 8080)
	if err != nil {
		t.Fatalf("port-forwarding %s/%s: %v", ns, probeName, err)
	}
	t.Cleanup(stop)

	return &probeClient{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", localPort),
		// Comfortably above the probe's own --handshake-timeout, so a gateway
		// that stalls is reported as the probe's tunnel error rather than as a
		// client timeout with nothing to point at.
		http: &http.Client{Timeout: 90 * time.Second},
	}
}

func waitForProbeReady(t *testing.T, ctx context.Context, ns string) {
	t.Helper()
	const timeout = 3 * time.Minute
	deadline := time.Now().Add(timeout)
	// Kept so the timeout can say why the pod never came up. Without it the
	// failure is just "timed out", and the pod is in a namespace the harness
	// retains but nobody thinks to look in.
	var lastState string
	for time.Now().Before(deadline) {
		pod, err := e2e.GetClients().K8s.CoreV1().Pods(ns).Get(ctx, probeName, metav1.GetOptions{})
		switch {
		case err != nil:
			lastState = err.Error()
		case portforward.IsPodReady(pod):
			t.Logf("probe pod %s/%s is ready", ns, probeName)
			return
		default:
			lastState = describeProbeState(pod)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out after %v waiting for probe pod %s/%s to become ready: %s", timeout, ns, probeName, lastState)
}

func describeProbeState(pod *corev1.Pod) string {
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

// handshake asks the probe to complete one inner TLS handshake for sni. A
// refused SNI comes back as a result with OK false, not as an error: refusal is
// one of the outcomes under test.
func (c *probeClient) handshake(t *testing.T, ctx context.Context, sni string) handshakeResult {
	t.Helper()
	endpoint := c.baseURL + "/handshake?sni=" + url.QueryEscape(sni)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("building probe request for %q: %v", sni, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("calling probe for %q: %v", sni, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		t.Fatalf("probe returned %d for %q: %s", resp.StatusCode, sni, strings.TrimSpace(string(body)))
	}
	var out handshakeResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding probe response for %q: %v", sni, err)
	}
	return out
}

// handshakeResult mirrors the probe's response body. It is duplicated rather
// than imported because the probe is package main.
type handshakeResult struct {
	SNI      string `json:"sni"`
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	ChainPEM string `json:"chain_pem"`

	// The verdict of verifying the served chain against the probe image's own
	// trust store, computed in the pod. Kind is one of "unknown_authority",
	// "hostname_mismatch", "invalid_reason", "other", or empty.
	SystemTrusted         bool   `json:"system_trusted"`
	SystemVerifyError     string `json:"system_verify_error"`
	SystemVerifyErrorKind string `json:"system_verify_error_kind"`
}

func parseChain(t *testing.T, sni, chainPEM string) []*x509.Certificate {
	t.Helper()
	var chain []*x509.Certificate
	rest := []byte(chainPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parsing certificate served for %q: %v", sni, err)
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		t.Fatalf("no certificates in the chain served for %q", sni)
	}
	return chain
}

func certPool(certs ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}
