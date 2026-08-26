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
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
)

const dependencyHealthCheckTimeout = 500 * time.Millisecond

type ComponentHealth struct {
	Healthy      bool      `json:"healthy"`
	Message      string    `json:"message,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	LastFailure  time.Time `json:"last_failure,omitempty"`
	SuccessCount int64     `json:"success_count"`
	FailureCount int64     `json:"failure_count"`
}

type RouterHealthReport struct {
	Dataplane   ComponentHealth `json:"dataplane"`
	ServingCert ComponentHealth `json:"serving_cert"`
	K8sAPI      ComponentHealth `json:"k8s_api"`
	AteAPI      ComponentHealth `json:"ate_api"`
}

type componentHealthCheckResult struct {
	healthy   bool
	message   string
	checkedAt time.Time
}

// routerHealth periodically checks the dependent services of router to track health
// status of this component.
type routerHealth struct {
	mu sync.RWMutex

	report RouterHealthReport

	interval        time.Duration
	clientset       kubernetes.Interface
	apiClient       ateapipb.ControlClient
	cfg             routerConfig
	dataplaneClient *http.Client
}

func newRouterHealth(interval time.Duration, clientset kubernetes.Interface, apiClient ateapipb.ControlClient, cfg routerConfig) *routerHealth {
	if interval <= 0 {
		interval = time.Second
	}
	return &routerHealth{
		interval:        interval,
		clientset:       clientset,
		apiClient:       apiClient,
		cfg:             cfg,
		dataplaneClient: &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
	}
}

func (rh *routerHealth) Start(ctx context.Context) {
	ticker := time.NewTicker(rh.interval)
	defer ticker.Stop()

	// Trigger immediate check
	rh.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rh.check(ctx)
		}
	}
}

func (rh *routerHealth) check(ctx context.Context) {
	slog.InfoContext(ctx, "Checking health")

	// Run network checks concurrently and without holding the report mutex, so
	// the cycle is bounded by the slowest dependency and status requests can
	// continue serving the last completed report.
	var dataplaneResult, servingCertResult, k8sResult, ateResult componentHealthCheckResult
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		dataplaneResult = runComponentHealthCheck(ctx, "Router dataplane health check failed", rh.checkDataplane)
	}()
	go func() {
		defer wg.Done()
		servingCertResult = runComponentHealthCheck(ctx, "Dataplane serving certificate health check failed", rh.checkServingCert)
	}()
	go func() {
		defer wg.Done()
		k8sResult = runComponentHealthCheck(ctx, "Kubernetes API health check failed", rh.checkK8s)
	}()
	go func() {
		defer wg.Done()
		ateResult = runComponentHealthCheck(ctx, "ATE API gRPC health check failed", rh.checkAteAPI)
	}()
	wg.Wait()

	rh.mu.Lock()
	defer rh.mu.Unlock()
	updateComponentHealth(&rh.report.Dataplane, dataplaneResult.healthy, dataplaneResult.message, dataplaneResult.checkedAt)
	updateComponentHealth(&rh.report.ServingCert, servingCertResult.healthy, servingCertResult.message, servingCertResult.checkedAt)
	updateComponentHealth(&rh.report.K8sAPI, k8sResult.healthy, k8sResult.message, k8sResult.checkedAt)
	updateComponentHealth(&rh.report.AteAPI, ateResult.healthy, ateResult.message, ateResult.checkedAt)
}

func runComponentHealthCheck(
	ctx context.Context,
	failureMessage string,
	check func(context.Context) (bool, string),
) componentHealthCheckResult {
	healthy, message := check(ctx)
	if !healthy {
		slog.ErrorContext(ctx, failureMessage, slog.String("msg", message))
	}
	return componentHealthCheckResult{
		healthy:   healthy,
		message:   message,
		checkedAt: time.Now(),
	}
}

func updateComponentHealth(health *ComponentHealth, healthy bool, msg string, checkedAt time.Time) {
	health.Healthy = healthy
	health.Message = msg
	if healthy {
		health.LastSuccess = checkedAt
		health.SuccessCount++
	} else {
		health.LastFailure = checkedAt
		health.FailureCount++
	}
}

func (rh *routerHealth) checkDataplane(ctx context.Context) (bool, string) {
	// The dataplane this polls is the *ingress* proxy sharing the router's pod.
	// The egress gateway is a separate, statically configured proxy on its own
	// admin port; an egress-only router has none beside it, and probing this
	// address would report a permanently unhealthy dependency.
	if !rh.cfg.Mode.ServesIngress() {
		return true, "Skipped (egress mode)"
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, dependencyHealthCheckTimeout)
	defer cancel()

	check := rh.cfg.atenetRouter().healthCheck()
	req, err := http.NewRequestWithContext(timeoutCtx, "GET", check.url, nil)
	if err != nil {
		return false, err.Error()
	}

	resp, err := rh.dataplaneClient.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("unexpected status code %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err.Error()
	}

	bodyStr := strings.TrimSpace(string(bodyBytes))
	if bodyStr != check.expectedBody {
		return false, fmt.Sprintf("expected %s but got %q", check.expectedBody, bodyStr)
	}

	return true, check.expectedBody
}

// servingCertReloadGrace is how far the dataplane's loaded certificate may lag
// the bundle on disk before the check calls it stuck. A rotation is two
// separate reads from outside — the file, then the admin endpoint — with the
// proxy's own reload somewhere in between, so a small lag is that race and not
// a fault. It is minutes against a certificate lifetime of a day, so a real
// stall is still caught with hours to spare.
const servingCertReloadGrace = 2 * time.Minute

// envoyCertsResponse is the part of Envoy's admin /certs output this check
// reads: for each TLS context, the chain it currently has loaded.
type envoyCertsResponse struct {
	Certificates []struct {
		CertChain []struct {
			Path           string `json:"path"`
			ExpirationTime string `json:"expiration_time"`
		} `json:"cert_chain"`
	} `json:"certificates"`
}

// checkServingCert compares the certificate the dataplane has loaded against
// the credential bundle on disk.
//
// It exists because that divergence is invisible from everywhere else. A proxy
// holding a certificate it has stopped refreshing stays live, ready, and
// serving — /ready says nothing about leaf validity — right up to the moment
// the leaf expires, when every handshake resets at once and stays broken until
// someone restarts the pod. Kubelet rotates a podCertificate with only hours
// of overlap, so the window between "the reload silently did not happen" and
// "the gateway is down" is short and entirely unlit.
func (rh *routerHealth) checkServingCert(ctx context.Context) (bool, string) {
	if rh.cfg.EnvoyCertPath == "" || rh.cfg.EnvoyAdminAddr == "" || rh.cfg.atenetRouter() != atenetRouterEnvoy {
		return true, "Skipped (no Envoy dataplane serving certificate configured)"
	}

	onDisk, err := credentialBundleNotAfter(rh.cfg.EnvoyCertPath)
	if err != nil {
		return false, err.Error()
	}
	loaded, err := rh.loadedCertNotAfter(ctx, rh.cfg.EnvoyCertPath)
	if err != nil {
		return false, err.Error()
	}

	if onDisk.Sub(loaded) > servingCertReloadGrace {
		return false, fmt.Sprintf("the dataplane is serving a leaf that expires at %s while %s already holds one valid to %s: the rotation was not picked up, and every TLS handshake fails from the earlier time onwards",
			loaded.Format(time.RFC3339), rh.cfg.EnvoyCertPath, onDisk.Format(time.RFC3339))
	}
	return true, fmt.Sprintf("Serving a leaf valid to %s", loaded.Format(time.RFC3339))
}

// credentialBundleNotAfter reads the leaf expiry out of a projected
// podCertificate credential bundle. The leaf is the first certificate in the
// PEM; the private key and any intermediates share the file.
func credentialBundleNotAfter(path string) (time.Time, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading credential bundle: %w", err)
	}
	for rest := pemBytes; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, fmt.Errorf("parsing leaf from %s: %w", path, err)
		}
		return leaf.NotAfter, nil
	}
	return time.Time{}, fmt.Errorf("no certificate in %s", path)
}

// loadedCertNotAfter asks Envoy's admin API when the certificate it loaded
// from certPath expires. Envoy reports one entry per TLS context, and this
// takes the latest: contexts reload independently, and treating the first one
// to catch up as the answer would flap on every rotation.
func (rh *routerHealth) loadedCertNotAfter(ctx context.Context, certPath string) (time.Time, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, dependencyHealthCheckTimeout)
	defer cancel()

	url := "http://" + rh.cfg.EnvoyAdminAddr + "/certs"
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, url, nil)
	if err != nil {
		return time.Time{}, err
	}
	resp, err := rh.dataplaneClient.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("%s returned status %d", url, resp.StatusCode)
	}

	certs := &envoyCertsResponse{}
	if err := json.NewDecoder(resp.Body).Decode(certs); err != nil {
		return time.Time{}, fmt.Errorf("decoding %s: %w", url, err)
	}

	var latest time.Time
	for _, c := range certs.Certificates {
		for _, chain := range c.CertChain {
			if chain.Path != certPath {
				continue
			}
			expiry, err := time.Parse(time.RFC3339, chain.ExpirationTime)
			if err != nil {
				return time.Time{}, fmt.Errorf("parsing expiration_time %q from %s: %w", chain.ExpirationTime, url, err)
			}
			if expiry.After(latest) {
				latest = expiry
			}
		}
	}
	if latest.IsZero() {
		// Also the startup state, until the dataplane finishes warming its
		// listeners. Reported as a failure regardless: a dataplane that never
		// loads the certificate is exactly as broken as one that loaded it and
		// then went stale.
		return time.Time{}, fmt.Errorf("the dataplane has no certificate loaded from %s", certPath)
	}
	return latest, nil
}

func (rh *routerHealth) checkK8s(ctx context.Context) (bool, string) {
	if rh.clientset == nil {
		return true, "Skipped (no Kubernetes client)"
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, dependencyHealthCheckTimeout)
	defer cancel()

	restClient := rh.clientset.Discovery().RESTClient()
	if restClient == nil {
		return false, "Kubernetes discovery REST client is unavailable"
	}
	body, err := restClient.Get().AbsPath("/version").Do(timeoutCtx).Raw()
	if err != nil {
		return false, err.Error()
	}
	ver := &version.Info{}
	if err := json.Unmarshal(body, ver); err != nil {
		return false, fmt.Sprintf("decoding Kubernetes version: %v", err)
	}

	return true, fmt.Sprintf("Version: %s", ver.GitVersion)
}

func (rh *routerHealth) checkAteAPI(ctx context.Context) (bool, string) {
	if rh.apiClient == nil {
		return false, "No client"
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, dependencyHealthCheckTimeout)
	defer cancel()

	_, err := rh.apiClient.ListActors(timeoutCtx, &ateapipb.ListActorsRequest{PageSize: 1})
	if err != nil {
		return false, err.Error()
	}

	return true, "Connected"
}

func (rh *routerHealth) Report() RouterHealthReport {
	if rh == nil {
		return RouterHealthReport{}
	}
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.report
}
