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

// Command egressprobe drives the egress gateway from inside the cluster and
// reports what happened, so an e2e suite can assert on the gateway's real
// behaviour rather than on a simulation of it.
//
// It serves two suites, which need different halves of the same trip:
//
//   - internal/e2e/suites/egressauthz asks whether the CONNECT was authorized.
//     extprocd decides that from the client certificate, so the probe presents
//     whichever credential the caller supplies.
//   - internal/e2e/suites/sdsmint asks what certificate the MITM leg served for
//     a given SNI, which needs the CONNECT to be allowed first.
//
// It has to run in a pod rather than in the test process because the gateway's
// front door requires a client certificate, and the probe's own credential is
// only issued to pods. Everything after the front door is the same path a real
// actor takes: this uses internal/atunnel, the same client cmd/ateom-gvisor
// builds, so a change that breaks actor egress breaks this too.
//
// The probe is not an actor and cannot become one -- an actor certificate comes
// from atelet, which mints only for a worker's real assignment. A suite that
// needs the gateway to see an actor signs a certificate itself and posts it, so
// the identity the gateway authenticates is the suite's choice. That is a test
// affordance and depends on the suite holding the actor CA, which is true only
// against a test cluster.
//
// The probe deliberately does NOT verify the certificate it is served. The
// chain goes back to the test verbatim, because verifying here would collapse
// "the leaf was minted for the wrong name", "it does not chain to the MITM CA"
// and "it has already expired" into one indistinguishable handshake error.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/e2e/fixtures/egressprobe/probeapi"
)

var (
	listenAddress = flag.String("listen", ":8080", "Address the probe's HTTP API listens on.")
	// The gateway Service, and the SAN its servicedns serving certificate
	// carries. cmd/ateom-gvisor derives ServerName the same way -- the host
	// half of the gateway address -- so keep these consistent.
	gatewayAddress       = flag.String("gateway-address", "atenet-egress.ate-system.svc:443", "host:port of the egress gateway's CONNECT front door.")
	credentialBundlePath = flag.String("credential-bundle", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "PEM credential bundle presented as the client certificate to the gateway when a request supplies none.")
	trustBundlePath      = flag.String("trust-bundle", "/run/servicedns.podcert.ate.dev/trust-bundle.pem", "PEM trust bundle used to verify the gateway's serving certificate.")
	handshakeTimeout     = flag.Duration("handshake-timeout", 20*time.Second, "Budget for one CONNECT plus inner handshake.")
	// maxRequestBytes bounds the posted credential. A client certificate chain
	// is a few kilobytes; this is generous enough not to matter and small enough
	// that a runaway body cannot exhaust the probe.
	maxRequestBytes = flag.Int64("max-request-bytes", 256<<10, "Maximum accepted request body size.")
)

// probe runs one Request and reports the outcome. Every refusal -- a denied
// CONNECT, a refused SNI -- is a normal result, not an HTTP error: which of them
// happened is the thing under test. Only a malformed request is a 4xx.
func probe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a probeapi.Request", http.StatusMethodNotAllowed)
		return
	}
	var req probeapi.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, *maxRequestBytes)).Decode(&req); err != nil {
		http.Error(w, "decoding request: "+err.Error(), http.StatusBadRequest)
		return
	}

	destination := req.Destination
	if destination == "" {
		destination = probeapi.DefaultDestination
	}
	result := probeapi.Result{
		Destination: destination,
		SNI:         req.SNI,
		Identity:    probeapi.IdentityPod,
	}
	if req.ClientCredentialPEM != "" {
		result.Identity = probeapi.IdentitySupplied
	}

	getClientCertificate, err := clientCertificateSource(req.ClientCredentialPEM)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), *handshakeTimeout)
	defer cancel()

	conn, err := dialTunnel(ctx, destination, getClientCertificate)
	if err != nil {
		result.ConnectError = err.Error()
		writeJSON(w, result)
		return
	}
	defer conn.Close()
	result.Connected = true

	if req.SNI != "" {
		chain, err := innerHandshake(ctx, conn, req.SNI)
		if err != nil {
			result.HandshakeError = err.Error()
		} else {
			result.HandshakeOK = true
			result.ChainPEM = chain
		}
	}
	writeJSON(w, result)
}

// clientCertificateSource picks the credential this request presents at the
// gateway's front door.
//
// The supplied credential is parsed here rather than inside the callback so a
// malformed PEM comes back as a 400 naming the problem, instead of surfacing
// later as an opaque handshake failure that reads like a gateway fault.
func clientCertificateSource(credentialPEM string) (func(*tls.CertificateRequestInfo) (*tls.Certificate, error), error) {
	if credentialPEM == "" {
		return podIdentityCertificate, nil
	}
	cert, err := tls.X509KeyPair([]byte(credentialPEM), []byte(credentialPEM))
	if err != nil {
		return nil, fmt.Errorf("parsing supplied client credential: %w", err)
	}
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }, nil
}

// podIdentityCertificate reads the probe's own Pod certificate off disk for
// each handshake.
//
// This says "a substrate workload", not which actor: it carries a PodIdentity
// and no ActorIdentity at all. A real actor gets here differently --
// atunnel.BrokerCertificateSource asks atelet to mint an actor certificate, so
// the gateway learns which actor a connection came from -- which is why the
// egress PEP denies this credential, and why a suite that needs to be an actor
// supplies its own.
//
// Read per handshake, not cached: the bundle is rotated on disk under the
// probe, and a cached copy would start failing the front door partway through
// a long suite.
func podIdentityCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	pemBytes, err := os.ReadFile(*credentialBundlePath)
	if err != nil {
		return nil, fmt.Errorf("reading credential bundle: %w", err)
	}
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing credential bundle %q: %w", *credentialBundlePath, err)
	}
	return &cert, nil
}

// dialTunnel opens a CONNECT tunnel to destination. A gateway refusal comes back
// as an error carrying the status line and response body, which is where the
// egress PEP's denial reason is.
func dialTunnel(ctx context.Context, destination string, getClientCertificate func(*tls.CertificateRequestInfo) (*tls.Certificate, error)) (net.Conn, error) {
	// Built per request rather than once at startup: cheap, it keeps the trust
	// bundle fresh alongside the credential (NewClient reads it once), and each
	// request may present a different client certificate.
	client, err := atunnel.NewClient(atunnel.ClientConfig{
		GatewayAddress:       *gatewayAddress,
		ServerName:           serverName(*gatewayAddress),
		GetClientCertificate: getClientCertificate,
		TrustBundlePath:      *trustBundlePath,
	})
	if err != nil {
		return nil, fmt.Errorf("building egress client: %w", err)
	}
	conn, err := client.DialContext(ctx, destination)
	if err != nil {
		return nil, fmt.Errorf("opening tunnel to %s: %w", destination, err)
	}
	return conn, nil
}

// innerHandshake completes a TLS handshake inside an open tunnel, which is what
// makes Envoy ask sdsmintd for a secret under sni, and returns the chain it was
// served.
func innerHandshake(ctx context.Context, conn net.Conn, sni string) (string, error) {
	//nolint:gosec // G402: verification is the caller's assertion; see the package comment.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return "", fmt.Errorf("inner TLS handshake for %q: %w", sni, err)
	}
	defer tlsConn.Close()

	var out []byte
	for _, cert := range tlsConn.ConnectionState().PeerCertificates {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("handshake for %q completed with no peer certificates", sni)
	}
	return string(out), nil
}

// serverName is the host half of a host:port address. It is written by hand
// rather than with net.SplitHostPort so that a malformed flag surfaces at
// handshake time with the address in the message, instead of at startup.
func serverName(address string) string {
	for i := len(address) - 1; i >= 0; i-- {
		if address[i] == ':' {
			return address[:i]
		}
	}
	return address
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("egressprobe: encoding response: %v", err)
	}
}

func main() {
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", probe)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:    *listenAddress,
		Handler: mux,
		// Generous relative to handshakeTimeout: a request that is waiting on a
		// gateway which never answers should return the tunnel error, not be
		// cut off by the server and reported as a connection reset.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute,
	}
	log.Printf("egressprobe: listening on %s, gateway %s", *listenAddress, *gatewayAddress)
	log.Fatal(server.ListenAndServe())
}
