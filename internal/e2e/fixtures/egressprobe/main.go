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

// Command egressprobe drives the egress gateway's MITM leg from inside the
// cluster and reports the certificate it was served, so an e2e suite can assert
// on what sdsmintd actually minted.
//
// It has to run in a pod rather than in the test process because the gateway's
// front door requires a client certificate from the podidentity signer, and
// that credential is only issued to pods. Everything after the front door is
// the same path a real actor takes: this uses internal/atunnel, the same client
// cmd/ateom-gvisor builds, so a change that breaks actor egress breaks this too.
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
	"net/http"
	"os"
	"time"

	"github.com/agent-substrate/substrate/internal/atunnel"
)

var (
	listenAddress = flag.String("listen", ":8080", "Address the probe's HTTP API listens on.")
	// The gateway Service, and the SAN its servicedns serving certificate
	// carries. cmd/ateom-gvisor derives ServerName the same way -- the host
	// half of the gateway address -- so keep these consistent.
	gatewayAddress       = flag.String("gateway-address", "atenet-egress.ate-system.svc:443", "host:port of the egress gateway's CONNECT front door.")
	credentialBundlePath = flag.String("credential-bundle", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "PEM credential bundle presented as the client certificate to the gateway.")
	trustBundlePath      = flag.String("trust-bundle", "/run/servicedns.podcert.ate.dev/trust-bundle.pem", "PEM trust bundle used to verify the gateway's serving certificate.")
	handshakeTimeout     = flag.Duration("handshake-timeout", 20*time.Second, "Budget for one CONNECT plus inner handshake.")
)

// tunnelDestination is the CONNECT authority. atunnel takes this from
// SO_ORIGINAL_DST and rejects hostnames, so it must be a literal IP -- and the
// gateway routes every CONNECT to the MITM listener regardless of authority,
// which is why a documentation address that resolves nowhere is enough. The
// name being tested travels in the tunneled ClientHello, not here.
const tunnelDestination = "192.0.2.1:443"

// handshakeResult is the probe's response body.
type handshakeResult struct {
	SNI string `json:"sni"`
	// OK reports whether the inner TLS handshake completed. A denied SNI is a
	// normal outcome, not a probe failure, so it comes back as OK=false with
	// the reason rather than as an HTTP error.
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// ChainPEM is the chain the gateway presented, leaf first.
	ChainPEM string `json:"chain_pem,omitempty"`
}

// handshake opens a tunnel through the gateway and completes an inner TLS
// handshake for the requested SNI, which is what makes Envoy ask sdsmintd for a
// secret under that name.
func handshake(w http.ResponseWriter, r *http.Request) {
	sni := r.URL.Query().Get("sni")
	if sni == "" {
		http.Error(w, "missing sni query parameter", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), *handshakeTimeout)
	defer cancel()

	result := handshakeResult{SNI: sni}
	chain, err := fetchChain(ctx, sni)
	if err != nil {
		result.Error = err.Error()
	} else {
		result.OK = true
		result.ChainPEM = chain
	}
	writeJSON(w, result)
}

// podIdentityCertificate reads the probe's own Pod certificate off disk for
// each handshake.
//
// A real actor gets here differently: atunnel.BrokerCertificateSource asks
// atelet to mint an actor certificate, so the gateway learns which actor a
// connection came from. The probe has no actor to be. It is an ordinary Pod
// testing what sdsmintd mints for a given SNI, so it presents the podidentity
// credential the gateway's front door requires and nothing more.
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

func fetchChain(ctx context.Context, sni string) (string, error) {
	// Built per request rather than once at startup: cheap, and it keeps the
	// trust bundle fresh alongside the credential, which NewClient reads once.
	client, err := atunnel.NewClient(atunnel.ClientConfig{
		GatewayAddress:       *gatewayAddress,
		ServerName:           serverName(*gatewayAddress),
		GetClientCertificate: podIdentityCertificate,
		TrustBundlePath:      *trustBundlePath,
	})
	if err != nil {
		return "", fmt.Errorf("building egress client: %w", err)
	}

	conn, err := client.DialContext(ctx, tunnelDestination)
	if err != nil {
		return "", fmt.Errorf("opening tunnel: %w", err)
	}
	defer conn.Close()

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
	mux.HandleFunc("/handshake", handshake)
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
