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
// behavior rather than on a simulation of it.
//
// It runs in two shapes, and Request.Via says which trip to make.
//
// As a PLAIN POD (egressprobe.yaml.tmpl) it issues its own CONNECT, presenting
// whichever client certificate the caller supplies:
//
//   - internal/e2e/suites/egressauthz asks whether the CONNECT was authorized.
//     extprocd decides that from the client certificate.
//   - internal/e2e/suites/sdsmint asks what certificate the MITM leg served for
//     a given SNI, which needs the CONNECT to be allowed first.
//
// In this shape the probe is not an actor and cannot become one -- an actor
// certificate comes from atelet, which mints only for a worker's real
// assignment. A suite that needs the gateway to see an actor signs a
// certificate itself and posts it, so the identity the gateway authenticates is
// the suite's choice. That is a test affordance and depends on the suite
// holding the actor CA, which is true only against a test cluster. It also
// means these suites prove nothing about the certificate ateapi really issues.
//
// As an ACTOR (egressprobe-actor.yaml.tmpl) it does the opposite: it dials the
// destination directly, presents nothing, and knows nothing about a gateway.
// internal/e2e/suites/actoregress uses this. ateom REDIRECTs the connection and
// builds the tunnel with the certificate the broker minted for the actor's real
// assignment, so this is the shape that covers the path production traffic
// takes -- and the only one that covers the broker at all.
//
// The two shapes need different things mounted, which is why direct mode reads
// neither the credential bundle nor the trust bundle: an Actor has neither.
//
// In either shape, a request that sets both SNI and RequestPath goes one step
// further and sends a real GET inside the tunnel. That step is what reaches the
// gateway's inner checkpoint: the second ext_proc filter fires on request
// headers, so a probe that stops at the handshake never consults the hostname
// allowlist or credential injection at all. The response goes back to the test
// unexamined, for the same reason the chain does.
//
// The probe deliberately does NOT verify the certificate it is served. The
// chain goes back to the test verbatim, because verifying here would collapse
// "the leaf was minted for the wrong name", "it does not chain to the MITM CA"
// and "it has already expired" into one indistinguishable handshake error.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
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
	// Covers the whole trip, which since RequestPath may include a round trip to
	// a real destination on the internet rather than two handshakes against
	// something in the pod.
	handshakeTimeout = flag.Duration("handshake-timeout", 30*time.Second, "Budget for one CONNECT, the inner handshake, and any request sent through it.")
	// maxRequestBytes bounds the posted credential. A client certificate chain
	// is a few kilobytes; this is generous enough not to matter and small enough
	// that a runaway body cannot exhaust the probe.
	maxRequestBytes = flag.Int64("max-request-bytes", 256<<10, "Maximum accepted request body size.")
)

// maxResponseBytes bounds what an inner response can put in a Result. Tests read
// a denial reason or a short status message out of it -- extprocd's "egress
// denied: ..." and GitHub's error JSON both fit in a fraction of this -- and an
// arbitrary origin's response body has no business traveling back through the
// probe API in full.
const maxResponseBytes = 16 << 10

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

	via := req.Via
	if via == "" {
		via = probeapi.ViaTunnel
	}
	destination := req.Destination
	if destination == "" {
		destination = probeapi.DefaultDestination
	}
	result := probeapi.Result{
		Via:         via,
		Destination: destination,
		SNI:         req.SNI,
	}

	ctx, cancel := context.WithTimeout(r.Context(), *handshakeTimeout)
	defer cancel()

	var (
		conn net.Conn
		err  error
	)
	switch via {
	case probeapi.ViaDirect:
		// No credential and no proxy: the whole point is that this looks like an
		// ordinary outbound connection. If it reaches the gateway anyway,
		// something below this process put it there.
		conn, err = dialDirect(ctx, destination)
	case probeapi.ViaTunnel:
		result.Identity = probeapi.IdentityPod
		if req.ClientCredentialPEM != "" {
			result.Identity = probeapi.IdentitySupplied
		}
		var getClientCertificate func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
		getClientCertificate, err = clientCertificateSource(req.ClientCredentialPEM)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		conn, err = dialTunnel(ctx, destination, getClientCertificate)
	default:
		http.Error(w, fmt.Sprintf("unknown via %q, want %q or %q", via, probeapi.ViaTunnel, probeapi.ViaDirect), http.StatusBadRequest)
		return
	}
	if err != nil {
		result.ConnectError = err.Error()
		writeJSON(w, result)
		return
	}
	defer conn.Close()
	result.Connected = true

	if req.SNI != "" {
		tlsConn, chain, err := innerHandshake(ctx, conn, req.SNI)
		if err != nil {
			result.HandshakeError = err.Error()
			writeJSON(w, result)
			return
		}
		defer tlsConn.Close()
		result.HandshakeOK = true
		result.ChainPEM = chain

		// Only now is the inner ext_proc reachable: it fires on request headers,
		// which the handshake above does not produce.
		if req.RequestPath != "" {
			status, body, err := innerRequest(ctx, tlsConn, req.SNI, req.RequestPath)
			// Recorded independently of err: a status that arrived before the
			// body failed is the more useful half of the answer.
			result.HTTPStatus = status
			result.HTTPBody = body
			if err != nil {
				result.HTTPError = err.Error()
			}
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

// dialDirect opens a plain TCP connection to destination, with no knowledge that
// a gateway exists.
//
// Under an Actor this is the entire mechanism under test. ateom's nftables
// prerouting rule REDIRECTs the connection to atunnel's local listener, which
// recovers the intended address from SO_ORIGINAL_DST and rebuilds it as a
// CONNECT carrying the Actor's certificate. None of that is visible from here,
// which is why the interesting assertion is on what comes back from the
// handshake rather than on this call.
//
// Two consequences for a caller reading the Result. Success proves almost
// nothing on its own -- the REDIRECT is local, so this returns before atunnel
// has spoken to the gateway. And a policy denial arrives as an EOF partway
// through the handshake, because atunnel logs the 403 and closes the socket
// (internal/atunnel/egress.go).
func dialDirect(ctx context.Context, destination string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", destination)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", destination, err)
	}
	return conn, nil
}

// innerHandshake completes a TLS handshake inside an open tunnel, which is what
// makes Envoy ask sdsmintd for a secret under sni, and returns both the chain it
// was served and the connection it was served on.
//
// The connection comes back open because the caller may have a request to send
// on it. It is closed here only on the paths that return an error, where there
// is nothing left to send.
func innerHandshake(ctx context.Context, conn net.Conn, sni string) (*tls.Conn, string, error) {
	//nolint:gosec // G402: verification is the caller's assertion; see the package comment.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tlsConn.Close()
		return nil, "", fmt.Errorf("inner TLS handshake for %q: %w", sni, err)
	}

	var out []byte
	for _, cert := range tlsConn.ConnectionState().PeerCertificates {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
	}
	if len(out) == 0 {
		tlsConn.Close()
		return nil, "", fmt.Errorf("handshake for %q completed with no peer certificates", sni)
	}
	return tlsConn, string(out), nil
}

// innerRequest sends one GET over an established inner connection and reads the
// response. This is the call the gateway's inner ext_proc sees, and the only one
// that reaches the hostname allowlist or credential injection.
//
// Written and read directly rather than through an http.Client: the connection
// already exists and is the thing under test, and a Transport would be free to
// open another one, retry, or upgrade the protocol -- none of which a test
// asserting on a single request through a single tunnel can tolerate.
//
// A response status is a result, not an error, however hostile: extprocd denies
// with a 403 and so may the origin. Only a request that could not be completed
// comes back as an error.
func innerRequest(ctx context.Context, conn net.Conn, host, path string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return 0, "", fmt.Errorf("building request for %q: %w", path, err)
	}
	req.Host = host
	// Named explicitly rather than left to net/http's default. Some origins
	// refuse a request whose User-Agent they do not like -- GitHub answers one
	// with no User-Agent at all with a 403, which is indistinguishable at a
	// glance from the gateway's own denial.
	req.Header.Set("User-Agent", "substrate-egressprobe/1")

	if deadline, ok := ctx.Deadline(); ok {
		// The write and the read below are raw socket operations and do not
		// observe ctx on their own.
		if err := conn.SetDeadline(deadline); err != nil {
			return 0, "", fmt.Errorf("setting deadline: %w", err)
		}
	}
	if err := req.Write(conn); err != nil {
		return 0, "", fmt.Errorf("sending request for %q: %w", path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return 0, "", fmt.Errorf("reading response for %q: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		// The status arrived, so report it: a truncated body is still worth
		// less than knowing the destination answered at all.
		return resp.StatusCode, "", fmt.Errorf("reading response body: %w", err)
	}
	return resp.StatusCode, string(body), nil
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
