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
// The handshake itself deliberately does NOT verify the certificate it is
// served. The chain goes back to the test verbatim, because verifying at the
// handshake would collapse "the leaf was minted for the wrong name", "it does
// not chain to the MITM CA" and "it has already expired" into one
// indistinguishable handshake error.
//
// Verification against the image's own trust store happens separately, after
// the handshake, and its verdict is reported alongside the chain. That is what
// lets a test assert the two outcomes together: the gateway served a valid
// MITM chain, AND an ordinary client in this image rejects it. Doing the same
// verification in the test process would assert nothing -- it would be checking
// the CI runner's trust store, which has never heard of this CA either and
// would keep saying so long after actors had been taught to trust it.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/agent-substrate/substrate/internal/atunnel"
	"github.com/agent-substrate/substrate/internal/credbundle"
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

	// SystemTrusted reports whether the served chain verifies against this
	// image's own CA bundle -- the store an ordinary TLS client in an actor
	// would consult. False is the expected answer today: nothing installs the
	// MITM anchor into a workload, which is why every actor speaking TLS
	// through the gateway has to disable verification.
	SystemTrusted bool `json:"system_trusted"`
	// SystemVerifyError is the verification failure verbatim, empty when
	// SystemTrusted is true.
	SystemVerifyError string `json:"system_verify_error,omitempty"`
	// SystemVerifyErrorKind classifies the failure, because WHY the chain was
	// rejected is the whole assertion. An untrusted anchor and an expired leaf
	// are both "verification failed" to a caller matching on a boolean, and a
	// test that cannot tell them apart passes for the wrong reason the day the
	// leaf TTL breaks. One of "unknown_authority", "hostname_mismatch",
	// "invalid_reason", "other", or empty when the chain verified.
	SystemVerifyErrorKind string `json:"system_verify_error_kind,omitempty"`
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
		result.ChainPEM = encodeChain(chain)
		result.SystemTrusted, result.SystemVerifyError, result.SystemVerifyErrorKind = verifyAgainstSystemRoots(chain, sni)
	}
	writeJSON(w, result)
}

// verifyAgainstSystemRoots runs the verification an ordinary TLS client in this
// image would run: the image's own CA bundle as the only trust anchors, the
// requested name as the hostname, server auth as the usage. Intermediates come
// from the chain the gateway presented, exactly as a real client would take
// them, so the only thing being tested is whether the anchor is trusted.
//
// A nil pool is deliberately NOT an error: x509.SystemCertPool returns one on
// an image with no trust store at all, and "this image trusts nothing" is a
// correct answer to the question being asked, not a probe malfunction.
func verifyAgainstSystemRoots(chain []*x509.Certificate, sni string) (trusted bool, verifyErr, kind string) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return false, fmt.Sprintf("loading system trust store: %v", err), "other"
	}
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{
		DNSName:       sni,
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return false, err.Error(), classifyVerifyError(err)
	}
	return true, "", ""
}

func classifyVerifyError(err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameMismatch x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknownAuthority):
		return "unknown_authority"
	case errors.As(err, &hostnameMismatch):
		return "hostname_mismatch"
	case errors.As(err, &invalid):
		// Covers expiry, name constraints and basic-constraint violations.
		return "invalid_reason"
	default:
		return "other"
	}
}

func encodeChain(chain []*x509.Certificate) string {
	var out []byte
	for _, cert := range chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	return string(out)
}

func fetchChain(ctx context.Context, sni string) ([]*x509.Certificate, error) {
	// Built per request rather than once at startup so that a bad flag or an
	// unreadable bundle surfaces in the response body, where the test can print
	// it, instead of at startup where it becomes a CrashLoopBackOff. The
	// credential itself is rotated on disk under the probe, and
	// credbundle.ClientLoader re-reads it when the file changes, so nothing here
	// goes stale partway through a long suite.
	client, err := atunnel.NewClient(atunnel.ClientConfig{
		GatewayAddress:       *gatewayAddress,
		ServerName:           serverName(*gatewayAddress),
		GetClientCertificate: credbundle.ClientLoader(*credentialBundlePath),
		TrustBundlePath:      *trustBundlePath,
	})
	if err != nil {
		return nil, fmt.Errorf("building egress client: %w", err)
	}

	conn, err := client.DialContext(ctx, tunnelDestination)
	if err != nil {
		return nil, fmt.Errorf("opening tunnel: %w", err)
	}
	defer conn.Close()

	//nolint:gosec // G402: verification is the caller's assertion; see the package comment.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("inner TLS handshake for %q: %w", sni, err)
	}
	defer tlsConn.Close()

	out := tlsConn.ConnectionState().PeerCertificates
	if len(out) == 0 {
		return nil, fmt.Errorf("handshake for %q completed with no peer certificates", sni)
	}
	return out, nil
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
