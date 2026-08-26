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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

type healthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f healthRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type healthControlClient struct {
	ateapipb.ControlClient
	listActorsFn func(context.Context, *ateapipb.ListActorsRequest, ...grpc.CallOption) (*ateapipb.ListActorsResponse, error)
}

func (c *healthControlClient) ListActors(
	ctx context.Context,
	req *ateapipb.ListActorsRequest,
	opts ...grpc.CallOption,
) (*ateapipb.ListActorsResponse, error) {
	return c.listActorsFn(ctx, req, opts...)
}

func newHealthTestClientset(t *testing.T, server *httptest.Server) kubernetes.Interface {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("creating Kubernetes client: %v", err)
	}
	return clientset
}

func setHealthyDataplaneClient(rh *routerHealth) {
	rh.dataplaneClient = &http.Client{Transport: healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("LIVE")),
		}, nil
	})}
}

// writeServingBundle drops a credential bundle whose leaf expires at notAfter
// into a temp dir, standing in for the projected podCertificate volume.
func writeServingBundle(t *testing.T, notAfter time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential-bundle.pem")
	if err := os.WriteFile(path, makeServingBundleExpiring(t, "atenet-egress", notAfter), 0o600); err != nil {
		t.Fatalf("write credential bundle: %v", err)
	}
	return path
}

// newServingCertHealth builds a routerHealth whose dataplane admin API reports
// loading certPath with the given expiry. A zero loadedExpiry reports a
// dataplane that has no certificate at all.
func newServingCertHealth(t *testing.T, certPath string, loadedExpiry time.Time) *routerHealth {
	t.Helper()
	rh := newRouterHealth(time.Second, nil, nil, routerConfig{
		Mode:           ModeEgress,
		EnvoyCertPath:  certPath,
		EnvoyAdminAddr: "localhost:15000",
	})
	body := `{"certificates":[]}`
	if !loadedExpiry.IsZero() {
		body = fmt.Sprintf(`{"certificates":[{"ca_cert":[],"cert_chain":[{"path":%q,"expiration_time":%q}]}]}`,
			certPath, loadedExpiry.UTC().Format(time.RFC3339))
	}
	rh.dataplaneClient = &http.Client{Transport: healthRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/certs" {
			t.Errorf("serving certificate check requested %s, want /certs", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return rh
}

// TestCheckServingCert covers the divergence the SDS delivery in sds.go exists
// to prevent: the dataplane holding a leaf older than the one on disk, which
// nothing else reports until that leaf expires and every handshake fails.
func TestCheckServingCert(t *testing.T) {
	onDisk := time.Now().Add(24 * time.Hour).Truncate(time.Second)

	t.Run("in sync", func(t *testing.T) {
		certPath := writeServingBundle(t, onDisk)
		healthy, msg := newServingCertHealth(t, certPath, onDisk).checkServingCert(context.Background())
		if !healthy {
			t.Errorf("checkServingCert() = (false, %q), want healthy when the loaded leaf matches the bundle", msg)
		}
	})

	t.Run("rotation not picked up", func(t *testing.T) {
		// The shape of the outage this check is for: kubelet has written the
		// replacement, and the dataplane is still on the leaf it booted with.
		certPath := writeServingBundle(t, onDisk)
		stale := onDisk.Add(-23 * time.Hour)
		healthy, msg := newServingCertHealth(t, certPath, stale).checkServingCert(context.Background())
		if healthy {
			t.Fatal("checkServingCert() = healthy while the dataplane served a leaf a day behind the bundle on disk")
		}
		for _, want := range []string{stale.UTC().Format(time.RFC3339), onDisk.UTC().Format(time.RFC3339), certPath} {
			if !strings.Contains(msg, want) {
				t.Errorf("checkServingCert() message = %q, want it to name %q", msg, want)
			}
		}
	})

	t.Run("within the reload grace", func(t *testing.T) {
		// Reading the file and reading the admin API are two instants with the
		// reload in between, so a lag under the grace is that race, not a fault.
		certPath := writeServingBundle(t, onDisk)
		lagging := onDisk.Add(-(servingCertReloadGrace / 2))
		healthy, msg := newServingCertHealth(t, certPath, lagging).checkServingCert(context.Background())
		if !healthy {
			t.Errorf("checkServingCert() = (false, %q), want the reload race tolerated", msg)
		}
	})

	t.Run("nothing loaded", func(t *testing.T) {
		certPath := writeServingBundle(t, onDisk)
		healthy, msg := newServingCertHealth(t, certPath, time.Time{}).checkServingCert(context.Background())
		if healthy {
			t.Fatal("checkServingCert() = healthy while the dataplane had no certificate loaded")
		}
		if !strings.Contains(msg, certPath) {
			t.Errorf("checkServingCert() message = %q, want it to name %q", msg, certPath)
		}
	})
}

// A router with no dataplane certificate to speak of — an ext_proc-only
// deployment, or agentgateway — must not report a permanently degraded
// dependency, nor dial an admin API that is not there.
func TestCheckServingCertSkippedWithoutCertPath(t *testing.T) {
	rh := newRouterHealth(time.Second, nil, nil, routerConfig{EnvoyAdminAddr: "localhost:9901"})
	rh.dataplaneClient = &http.Client{Transport: healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("serving certificate check dialed the dataplane admin API with no certificate configured")
		return nil, errors.New("unexpected request")
	})}

	healthy, msg := rh.checkServingCert(context.Background())
	if !healthy {
		t.Errorf("checkServingCert() healthy = false, want true (skipped)")
	}
	if !strings.Contains(msg, "Skipped") {
		t.Errorf("checkServingCert() message = %q, want it to report the check was skipped", msg)
	}
}

func TestCheckDataplane(t *testing.T) {
	tests := []struct {
		name        string
		router      atenetRouter
		wantURL     string
		response    string
		wantMessage string
	}{
		{
			name:        "envoy",
			router:      atenetRouterEnvoy,
			wantURL:     "http://localhost:9901/ready",
			response:    "LIVE",
			wantMessage: "LIVE",
		},
		{
			name:        "agentgateway",
			router:      atenetRouterAgentgateway,
			wantURL:     "http://127.0.0.1:15021/healthz/ready",
			response:    "ready\n",
			wantMessage: "ready",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rh := newRouterHealth(time.Second, nil, nil, routerConfig{AtenetRouter: string(tc.router)})
			rh.dataplaneClient = &http.Client{Transport: healthRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != tc.wantURL {
					t.Errorf("health URL = %q, want %q", req.URL.String(), tc.wantURL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(tc.response)),
				}, nil
			})}

			healthy, message := rh.checkDataplane(context.Background())
			if !healthy || message != tc.wantMessage {
				t.Errorf("checkDataplane() = (%v, %q), want (true, %q)", healthy, message, tc.wantMessage)
			}
		})
	}
}

// An egress-only router has no ingress dataplane beside it, so probing the
// ingress proxy's admin port would report a dependency that is permanently
// down.
func TestCheckDataplaneSkippedInEgressMode(t *testing.T) {
	rh := newRouterHealth(time.Second, nil, nil, routerConfig{Mode: ModeEgress})
	rh.dataplaneClient = &http.Client{Transport: healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("checkDataplane dialed the ingress dataplane admin port in egress mode")
		return nil, errors.New("unexpected request")
	})}

	healthy, msg := rh.checkDataplane(context.Background())
	if !healthy {
		t.Errorf("checkDataplane healthy = false, want true (skipped)")
	}
	if !strings.Contains(msg, "Skipped") {
		t.Errorf("checkDataplane message = %q, want it to report the check was skipped", msg)
	}
}

func TestCheckK8sTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	defer server.Close()

	rh := newRouterHealth(time.Second, newHealthTestClientset(t, server), nil, routerConfig{})
	startedAt := time.Now()
	healthy, msg := rh.checkK8s(context.Background())
	elapsed := time.Since(startedAt)

	if healthy {
		t.Fatal("checkK8s returned healthy for a stalled request")
	}
	if !strings.Contains(msg, context.DeadlineExceeded.Error()) {
		t.Fatalf("checkK8s message = %q, want context deadline exceeded", msg)
	}
	if elapsed > 3*dependencyHealthCheckTimeout {
		t.Fatalf("checkK8s took %v, want a bounded timeout near %v", elapsed, dependencyHealthCheckTimeout)
	}
}

func TestCheckK8sWithoutRESTClient(t *testing.T) {
	rh := newRouterHealth(time.Second, kubernetesfake.NewSimpleClientset(), nil, routerConfig{})
	healthy, msg := rh.checkK8s(context.Background())
	if healthy {
		t.Fatal("checkK8s returned healthy without a discovery REST client")
	}
	if msg != "Kubernetes discovery REST client is unavailable" {
		t.Fatalf("checkK8s message = %q, want unavailable REST client", msg)
	}
}

func TestHealthCheckDoesNotBlockReportOrStatusz(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequest)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(started)
		select {
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"gitVersion":"v1.36.1"}`)
		case <-req.Context().Done():
		}
	}))
	defer server.Close()

	rh := newRouterHealth(time.Second, newHealthTestClientset(t, server), nil, routerConfig{})
	setHealthyDataplaneClient(rh)
	checkDone := make(chan struct{})
	go func() {
		rh.check(context.Background())
		close(checkDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Kubernetes health request did not start")
	}

	reportDone := make(chan struct{})
	go func() {
		_ = rh.Report()
		close(reportDone)
	}()
	select {
	case <-reportDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Report blocked while a dependency health request was in flight")
	}

	statusServer := httptest.NewServer(http.HandlerFunc((&RouterServer{
		cfg:    routerConfig{},
		health: rh,
	}).handleStatusz))
	defer statusServer.Close()
	statusClient := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := statusClient.Get(statusServer.URL + "/statusz?format=json")
	if err != nil {
		t.Fatalf("GET /statusz while health check was in flight: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /statusz status = %s, want 200 OK", resp.Status)
	}

	releaseRequest()
	select {
	case <-checkDone:
	case <-time.After(time.Second):
		t.Fatal("health check did not finish after the Kubernetes response was released")
	}

	report := rh.Report()
	if !report.Dataplane.Healthy || report.Dataplane.SuccessCount != 1 || report.Dataplane.LastSuccess.IsZero() {
		t.Errorf("dataplane health = %+v, want one successful check", report.Dataplane)
	}
	if !report.K8sAPI.Healthy || report.K8sAPI.SuccessCount != 1 || report.K8sAPI.LastSuccess.IsZero() {
		t.Errorf("Kubernetes health = %+v, want one successful check", report.K8sAPI)
	}
	if report.K8sAPI.Message != "Version: v1.36.1" {
		t.Errorf("Kubernetes health message = %q, want %q", report.K8sAPI.Message, "Version: v1.36.1")
	}
	if report.AteAPI.Healthy || report.AteAPI.FailureCount != 1 || report.AteAPI.LastFailure.IsZero() {
		t.Errorf("ATE API health = %+v, want one failed check", report.AteAPI)
	}
}

func TestHealthChecksRunConcurrently(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseChecks := func() { releaseOnce.Do(func() { close(release) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		started <- "k8s"
		select {
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"gitVersion":"v1.36.1"}`)
		case <-req.Context().Done():
		}
	}))
	defer server.Close()

	apiClient := &healthControlClient{
		listActorsFn: func(ctx context.Context, _ *ateapipb.ListActorsRequest, _ ...grpc.CallOption) (*ateapipb.ListActorsResponse, error) {
			started <- "ateapi"
			select {
			case <-release:
				return &ateapipb.ListActorsResponse{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	rh := newRouterHealth(time.Second, newHealthTestClientset(t, server), apiClient, routerConfig{})
	rh.dataplaneClient = &http.Client{Transport: healthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		started <- "dataplane"
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("LIVE")),
		}, nil
	})}

	checkDone := make(chan struct{})
	go func() {
		rh.check(context.Background())
		close(checkDone)
	}()
	defer func() {
		releaseChecks()
		<-checkDone
	}()

	seen := make(map[string]bool, 3)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for len(seen) < 3 {
		select {
		case dependency := <-started:
			seen[dependency] = true
		case <-timer.C:
			t.Fatalf("started health checks = %v, want dataplane, k8s, and ateapi before any check finishes", seen)
		}
	}

	releaseChecks()
	select {
	case <-checkDone:
	case <-time.After(time.Second):
		t.Fatal("health check did not finish after dependencies were released")
	}

	report := rh.Report()
	if !report.Dataplane.Healthy || !report.K8sAPI.Healthy || !report.AteAPI.Healthy {
		t.Errorf("health report = %+v, want all dependencies healthy", report)
	}
}

func TestHealthStartStopsWhenK8sCheckIsCanceled(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
	}))
	defer server.Close()

	rh := newRouterHealth(time.Hour, newHealthTestClientset(t, server), nil, routerConfig{})
	setHealthyDataplaneClient(rh)
	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})
	go func() {
		rh.Start(ctx)
		close(startDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Kubernetes health request did not start")
	}
	cancel()
	select {
	case <-startDone:
	case <-time.After(time.Second):
		t.Fatal("router health loop did not stop after cancellation")
	}
}
