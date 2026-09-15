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

// The egress protocol matrix: one actor, one origin, and every protocol shape
// an actor's outbound traffic can take -- plain HTTP, a tunneling CONNECT of
// the workload's own, a WebSocket, a QUIC datagram, a DNS query -- each over
// HTTP/1.1 and HTTP/2, in the clear and inside TLS, under four EgressPolicies.
//
// The other egress suites ask whether a policy allows or denies one request.
// This one asks what the egress path does to a protocol at all, and what the
// actor is told when the answer is no: the status code and body of a denial,
// or the error a dropped datagram turns into. Every case reports rather than
// only asserting, so a run is the evidence behind docs/egress-protocol-matrix.md.
package egressmatrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/internal/e2e/fixtures/testserver/matrixapi"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// matrixAtespace holds the probe actor and its template.
	matrixAtespace = "egress-matrix-e2e"
	// testserverImportPath backs both ends: the origin runs its matrixtarget
	// subcommand, the actor its matrixprobe one.
	testserverImportPath = "github.com/agent-substrate/substrate/internal/e2e/fixtures/testserver"
	// targetListenPort is what the origin binds. Above 1024, because the pod
	// runs as uid 65532 with every capability dropped; the Services in front
	// publish 80 and 443 down to it.
	targetListenPort = 8443
	// policySettleDelay outwaits the egress router's policy cache
	// (DefaultPolicyCacheTTL, 10s) so a phase measures the policy it set and
	// not the one before it.
	policySettleDelay = 13 * time.Second
	// caseTimeoutSeconds bounds a case whose answer is silence -- a dropped
	// datagram, a tunnel that is neither accepted nor refused. Short, because
	// the route in front of the probe has a timeout of its own: a case that
	// outlasts it is answered by the ingress with a 504 and says nothing about
	// egress. Every answer the matrix does get back arrives in milliseconds.
	caseTimeoutSeconds = 4
	// probeTimeout bounds one case end to end, as seen from the test. Longer
	// than the probe's own timeout so a dropped datagram is reported by the
	// probe rather than cut off here.
	probeTimeout = 45 * time.Second
)

// matrixCase is one cell: a protocol shape, and the name it is reported under.
type matrixCase struct {
	// name is the protocol shape in the terms the report uses.
	name string
	// request is what the probe is asked to do.
	request matrixapi.ProbeRequest
}

// TestEgressProtocolMatrix runs every case under every policy and reports what
// came back. Read the MATRIX lines in the output: one per cell, with the status
// and body of whatever denied it.
func TestEgressProtocolMatrix(t *testing.T) {
	ctx := context.Background()
	clients := e2e.GetClients()

	// Stand the origins up first: a fixture failure should cost no actor.
	// Two Services in front of the same sniffed port, one on 80 and one on
	// 443, so a case can ask whether the port or the content decides how the
	// gateway handles a connection.
	plain := e2e.DeployServerPod(t, ctx, matrixTarget("matrixplain", 80, ""))
	secure := e2e.DeployServerPod(t, ctx, matrixTarget("matrixsecure", 443, plain.Namespace))
	plainName := serviceDomainName("matrixplain", plain.Namespace)
	secureName := serviceDomainName("matrixsecure", secure.Namespace)
	t.Logf("origins: %s (%s) and %s (%s)", plainName, plain.Address(), secureName, secure.Address())

	actorRef, apiRef := deployProbeActor(t, ctx, clients)
	router := mustRouterClient(t, ctx)

	// Bound the gateway-log dump at the end to lines these phases produced.
	since := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	// Every phase runs the same cases; what changes is the policy and, for the
	// hostname phase, whether the origin is dialed by name or by address. A
	// hostname rule has nothing to match when the actor dials an address: the
	// authority the gateway sees is then the address too.
	phases := []struct {
		name  string
		rules []*ateapipb.EgressRule
		cases []matrixCase
	}{
		{
			name:  "allow-all",
			rules: []*ateapipb.EgressRule{e2e.EgressAllowAll()},
			cases: protocolCases(plain.Address(), secure.Address()),
		},
		{
			name:  "hostnames-only",
			rules: []*ateapipb.EgressRule{e2e.EgressAllowHostnames(plainName, secureName)},
			cases: protocolCases(hostPort(plainName, 80), hostPort(secureName, 443)),
		},
		{
			// The same hostname rule, aimed at an origin it does not name. This
			// is the phase that produces a per-request denial: the tunnel opens
			// because no address rule has anything to say, and the gateway then
			// refuses the requests inside it one at a time.
			name:  "hostname-not-allowed",
			rules: []*ateapipb.EgressRule{e2e.EgressAllowHostnames(secureName)},
			cases: protocolCases(hostPort(plainName, 80), hostPort(secureName, 443)),
		},
		{
			name:  "cidrs-only",
			rules: []*ateapipb.EgressRule{e2e.EgressAllowCIDRs(hostCIDR(t, plain.ClusterIP), hostCIDR(t, secure.ClusterIP))},
			cases: protocolCases(plain.Address(), secure.Address()),
		},
		{
			name:  "no-policy",
			rules: nil,
			cases: protocolCases(plain.Address(), secure.Address()),
		},
	}

	for _, phase := range phases {
		t.Run(phase.name, func(t *testing.T) {
			setPolicy(t, ctx, clients, apiRef, phase.rules)
			for _, c := range phase.cases {
				result := runCase(t, ctx, router, actorRef, c)
				t.Logf("MATRIX|%s|%s|ok=%v|status=%d|proto=%s|body=%q|detail=%q|error=%q|errorType=%s|elapsedMs=%d",
					phase.name, c.name, result.OK, result.Status, result.Proto,
					result.Body, result.Detail, result.Error, result.ErrorType, result.ElapsedMs)
			}
		})
	}

	// The baseline phase is the one with a right answer that does not depend on
	// the gateway's protocol support: an allow-all policy has to let the
	// simplest request through, or every denial below is just a broken fixture.
	t.Run("baseline", func(t *testing.T) {
		setPolicy(t, ctx, clients, apiRef, []*ateapipb.EgressRule{e2e.EgressAllowAll()})
		baseline := matrixCase{
			name:    "HTTP/1.1 cleartext",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindHTTP, Target: plain.Address(), HTTP: "1.1"},
		}
		result := runCase(t, ctx, router, actorRef, baseline)
		if !result.OK {
			t.Fatalf("the allow-all baseline failed: status=%d body=%q error=%q", result.Status, result.Body, result.Error)
		}
	})

	dumpGatewayLogs(t, ctx, since)
}

// protocolCases is the matrix itself, for an origin reachable at plainAddr in
// the clear and at secureAddr under TLS. The UDP and DNS cases name their own
// destinations: nothing in the actor's netns forwards a datagram to a
// ClusterIP that publishes no UDP port, and what they are asking is which
// datagrams leave at all.
func protocolCases(plainAddr, secureAddr string) []matrixCase {
	cases := []matrixCase{
		{
			name:    "HTTP/1.1 cleartext",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindHTTP, Target: plainAddr, HTTP: "1.1"},
		},
		{
			name:    "HTTP/2 cleartext (h2c, prior knowledge)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindHTTP, Target: plainAddr, HTTP: "2"},
		},
		{
			name:    "HTTP/1.1 over TLS",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindHTTP, Target: secureAddr, HTTP: "1.1", TLS: true},
		},
		{
			name:    "HTTP/2 over TLS (ALPN h2)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindHTTP, Target: secureAddr, HTTP: "2", TLS: true},
		},
		{
			name:    "CONNECT over HTTP/1.1 cleartext",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindConnect, Target: plainAddr, HTTP: "1.1"},
		},
		{
			name:    "CONNECT over HTTP/2 cleartext (h2c)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindConnect, Target: plainAddr, HTTP: "2"},
		},
		{
			name:    "CONNECT over HTTP/1.1 inside TLS",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindConnect, Target: secureAddr, HTTP: "1.1", TLS: true},
		},
		{
			name:    "CONNECT over HTTP/2 inside TLS",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindConnect, Target: secureAddr, HTTP: "2", TLS: true},
		},
		{
			name:    "WebSocket over HTTP/1.1 cleartext (ws://)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindWebsocket, Target: plainAddr, HTTP: "1.1"},
		},
		{
			name:    "WebSocket over HTTP/2 cleartext (extended CONNECT)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindWebsocket, Target: plainAddr, HTTP: "2"},
		},
		{
			name:    "WebSocket over HTTP/1.1 inside TLS (wss://)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindWebsocket, Target: secureAddr, HTTP: "1.1", TLS: true},
		},
		{
			name:    "WebSocket over HTTP/2 inside TLS (extended CONNECT)",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindWebsocket, Target: secureAddr, HTTP: "2", TLS: true},
		},
		{
			name:    "QUIC Initial datagram to UDP 443",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindUDP, Target: "1.1.1.1:443"},
		},
		{
			name:    "DNS over UDP to 1.1.1.1:53",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindDNS, Target: "1.1.1.1:53", Network: "udp", Name: "example.com"},
		},
		{
			name:    "DNS over UDP to the actor's own resolver",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindDNS, Network: "udp", Name: "example.com"},
		},
		{
			name:    "DNS over TCP to 1.1.1.1:53",
			request: matrixapi.ProbeRequest{Kind: matrixapi.KindDNS, Target: "1.1.1.1:53", Network: "tcp", Name: "example.com"},
		},
	}
	for i := range cases {
		cases[i].request.TimeoutSeconds = caseTimeoutSeconds
	}
	return cases
}

// runCase asks the probe for one case and returns what it reported. A transport
// failure inside the actor is a result, not a test failure: a refused tunnel is
// exactly what several cells are measuring.
func runCase(t *testing.T, ctx context.Context, router *e2e.RouterClient, actorRef resources.ActorRef, c matrixCase) matrixapi.ProbeResult {
	t.Helper()
	payload, err := json.Marshal(c.request)
	if err != nil {
		t.Fatalf("marshaling the probe request for %q: %v", c.name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	response, err := router.PostJSON(ctx, actorRef, "/probe", payload)
	if err != nil {
		t.Fatalf("POST /probe for %q: %v", c.name, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("reading the probe response for %q (HTTP %d): %v", c.name, response.StatusCode, err)
	}
	if response.StatusCode != http.StatusOK {
		// The ingress path answered instead of the probe -- a 504 when a case
		// outlasts the route's timeout. Reported rather than fatal, so one slow
		// cell does not cost the rest of the phase.
		return matrixapi.ProbeResult{
			Error:     fmt.Sprintf("ingress answered HTTP %d before the probe did: %s", response.StatusCode, strings.TrimSpace(string(body))),
			ErrorType: "ingress",
		}
	}
	var result matrixapi.ProbeResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decoding the probe result for %q: %v; body: %s", c.name, err, body)
	}
	return result
}

// setPolicy replaces the actor's EgressPolicy with rules, or removes it when
// rules is nil, and waits out the router's policy cache.
func setPolicy(t *testing.T, ctx context.Context, clients *e2e.Clients, actor *ateapipb.ObjectRef, rules []*ateapipb.EgressRule) {
	t.Helper()
	if rules == nil {
		if _, err := clients.SubstrateAPI.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: actor}); err != nil {
			t.Fatalf("DeleteActorEgressPolicy: %v", err)
		}
	} else {
		e2e.EnsureEgressPolicy(t, ctx, clients, actor, rules...)
	}
	time.Sleep(policySettleDelay)
}

// matrixTarget is the origin, published on port and listening on
// targetListenPort. GODEBUG=http2xconnect=1 is what makes the HTTP/2 server
// advertise SETTINGS_ENABLE_CONNECT_PROTOCOL; without it the two extended
// CONNECT cases would fail at the origin and say nothing about the egress path.
func matrixTarget(name string, port int, namespace string) e2e.ServerPod {
	return e2e.ServerPod{
		Name:       name,
		Namespace:  namespace,
		ImportPath: testserverImportPath,
		Args:       []string{"matrixtarget"},
		Port:       port,
		TargetPort: targetListenPort,
		Env:        []corev1.EnvVar{{Name: "GODEBUG", Value: "http2xconnect=1"}},
	}
}

// deployProbeActor creates the probe's ActorTemplate -- the egress demo's
// runtime with the testserver image in place of its container -- then an actor
// from it, and resumes it. It returns the actor in both the shapes its two
// callers need: the router's and the API's.
func deployProbeActor(t *testing.T, ctx context.Context, clients *e2e.Clients) (resources.ActorRef, *ateapipb.ObjectRef) {
	t.Helper()
	image := e2e.KoBuild(t, testserverImportPath)
	namespace := e2e.CreateNamespace(t).Name

	template := e2e.CreateSubstrateTemplateFrom(ctx, t, clients, namespace, egressSource(), e2e.SubstrateTemplateOptions{
		Atespace:     matrixAtespace,
		Name:         e2e.FixtureName("matrixprobe"),
		PoolName:     "matrixprobe",
		PoolReplicas: 1,
		Labels:       map[string]string{"workload": "egress-matrix"},
		Modify: func(tmpl *ateapipb.ActorTemplate) {
			tmpl.Containers = []*ateapipb.Container{{
				Name:    "matrixprobe",
				Image:   image,
				Command: []string{"/ko-app/testserver"},
				Args:    []string{"matrixprobe", "--listen=:80"},
				Readyz: &ateapipb.ContainerReadyz{
					HttpGet: &ateapipb.HTTPGetAction{Path: "/readyz", Port: 80},
				},
			}}
		},
	})

	actorName := fmt.Sprintf("matrixprobe-%d", time.Now().UnixNano())
	apiRef := &ateapipb.ObjectRef{Atespace: matrixAtespace, Name: actorName}
	actor := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: matrixAtespace, Name: actorName},
		ActorTemplate: e2e.TemplateRef(template),
	}
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: actor}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	t.Cleanup(func() {
		_, _ = clients.SubstrateAPI.SuspendActor(context.Background(), &ateapipb.SuspendActorRequest{Actor: apiRef})
		_, _ = clients.SubstrateAPI.DeleteActor(context.Background(), &ateapipb.DeleteActorRequest{Actor: apiRef})
	})
	// The first policy goes in before the resume so the actor's first outbound
	// connection already finds one.
	e2e.EnsureEgressPolicy(t, ctx, clients, apiRef, e2e.EgressAllowAll())
	if _, err := e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapipb.ResumeActorRequest{Actor: apiRef}); err != nil {
		t.Fatalf("ResumeActor: %v", err)
	}
	t.Logf("resumed probe actor %s/%s", matrixAtespace, actorName)

	actorRef := resources.ActorRef{Atespace: matrixAtespace, Name: actorName}
	waitForActorRoute(t, ctx, mustRouterClient(t, ctx), actorRef)
	return actorRef, apiRef
}

// egressSource is the installed demo the probe's template copies its runtime
// from: the one whose workers already have an egress path set up.
func egressSource() e2e.SubstrateFixture {
	fixture := e2e.EgressFixture()
	return e2e.SubstrateFixture{
		Atespace:      fixture.Namespace,
		Name:          fixture.Name,
		PoolNamespace: fixture.Namespace,
		PoolName:      fixture.Name,
		DeployWith:    fixture.DeployWith,
	}
}

// dumpGatewayLogs writes the egress gateway's access log and its policy
// router's complaints into the test output, which is where a denial's
// gateway-side half is recorded: the actor only ever sees the status code.
func dumpGatewayLogs(t *testing.T, ctx context.Context, since metav1.Time) {
	t.Helper()
	const namespace = "ate-system"
	clients := e2e.GetClients()
	pods, err := clients.K8s.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=atenet-egress"})
	if err != nil {
		t.Logf("listing the egress gateway pods: %v", err)
		return
	}
	for _, pod := range pods.Items {
		for _, container := range []string{"envoy", "ext-proc"} {
			logs, err := clients.K8s.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: container,
				SinceTime: &since,
			}).DoRaw(ctx)
			if err != nil {
				t.Logf("reading %s/%s logs of container %s: %v", namespace, pod.Name, container, err)
				continue
			}
			for line := range strings.SplitSeq(string(logs), "\n") {
				if line != "" {
					t.Logf("GATEWAY|%s|%s", container, line)
				}
			}
		}
	}
}

// serviceDomainName is the cluster DNS name of a Service, which is what a
// hostname rule has to match.
func serviceDomainName(name, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)
}

func hostPort(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// hostCIDR is the single-address prefix covering ip, which is how a CIDR rule
// names one origin.
func hostCIDR(t *testing.T, ip string) string {
	t.Helper()
	address, err := netip.ParseAddr(ip)
	if err != nil {
		t.Fatalf("parsing the origin's address %q: %v", ip, err)
	}
	return netip.PrefixFrom(address, address.BitLen()).String()
}

func mustRouterClient(t *testing.T, ctx context.Context) *e2e.RouterClient {
	t.Helper()
	router, err := e2e.NewRouterClient(ctx)
	if err != nil {
		t.Fatalf("NewRouterClient: %v", err)
	}
	t.Cleanup(func() { router.Close() })
	return router
}

// waitForActorRoute polls the probe's readiness endpoint through the router
// until it answers, which is when the actor's route has reached the ingress
// dataplane. Without it the first case would see a transient 503 from the
// router and report it as if the egress path had produced it.
func waitForActorRoute(t *testing.T, ctx context.Context, router *e2e.RouterClient, actorRef resources.ActorRef) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		response, err := router.Get(ctx, actorRef, "/readyz")
		if err != nil {
			t.Fatalf("GET /readyz on the probe actor through ingress: %v", err)
		}
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe actor's route did not come up: GET /readyz returned HTTP %d", response.StatusCode)
		}
		time.Sleep(time.Second)
	}
}
