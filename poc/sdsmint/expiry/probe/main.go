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

// probe completes one tunneled handshake through the harness gateway and
// reports the leaf it was served, as a line of JSON.
//
// It exists because openssl s_client -proxy sends an HTTP/1.0 CONNECT and
// Envoy answers 426 Upgrade Required. poc/sdsmint/__run/il-check.sh drives the
// gateway that way with stderr redirected to /dev/null, so it has been
// reporting "<none>" for every serial rather than failing.
//
// The report separates two things that an expiry run has to keep apart:
//
//	Handshake  what an actor sees. Actors dial through the gateway with
//	           verification off -- nothing in the cluster trusts the MITM
//	           anchor, so they cannot do otherwise -- and this connection is
//	           made the same way. An expired leaf is accepted here.
//	Verified   what verification would have said, computed afterwards against
//	           the harness CA. This is the only place an expiry shows up, and
//	           VerifyError names it.
//
// A run where Handshake stays true while Verified goes false is Envoy serving
// an expired leaf to a client too blind to care. A run where Handshake goes
// false is Envoy refusing to serve it. Those are the two outcomes the harness
// is there to tell apart, and no single boolean distinguishes them.
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// report is one probe's result, one JSON object per line.
type report struct {
	SNI       string `json:"sni"`
	Handshake bool   `json:"handshake"`
	Stage     string `json:"stage,omitempty"`
	Error     string `json:"error,omitempty"`

	Serial    string `json:"serial,omitempty"`
	NotBefore string `json:"not_before,omitempty"`
	NotAfter  string `json:"not_after,omitempty"`
	// SecondsToExpiry is negative once the leaf is past notAfter, which is the
	// field the harness reads to decide whether a probe is before or after the
	// moment of interest. No omitempty: the one value it would drop is zero,
	// the exact instant of expiry, and a probe that lands there is the most
	// interesting row in the run, not one to leave out of the JSON.
	SecondsToExpiry int      `json:"seconds_to_expiry"`
	ChainLen        int      `json:"chain_len,omitempty"`
	DNSNames        []string `json:"dns_names,omitempty"`

	Verified    bool   `json:"verified"`
	VerifyError string `json:"verify_error,omitempty"`
}

func main() {
	var (
		proxy   = flag.String("proxy", "127.0.0.1:18500", "CONNECT proxy, the gateway front door")
		sni     = flag.String("sni", "", "SNI to ask for; also the CONNECT authority host")
		caFile  = flag.String("ca", "ca.pem", "MITM root to verify against, after the handshake")
		timeout = flag.Duration("timeout", 10*time.Second, "budget for the whole probe")
	)
	flag.Parse()

	if *sni == "" {
		fmt.Fprintln(os.Stderr, "probe: --sni is required")
		os.Exit(2)
	}

	r := probe(*proxy, *sni, *caFile, *timeout)
	out, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: marshalling report:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))

	// Exit 0 even for a refused handshake. A failure is a reading, not an
	// error: the harness loop wants every probe's line, including the ones
	// where the gateway said no.
}

func probe(proxy, sni, caFile string, timeout time.Duration) report {
	r := report{SNI: sni}
	deadline := time.Now().Add(timeout)

	conn, err := net.DialTimeout("tcp", proxy, timeout)
	if err != nil {
		r.Stage, r.Error = "dial", err.Error()
		return r
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	// Port 443 is what the CONNECT authority claims; nothing dials it. Envoy
	// serves the leaf during the handshake below, before HCM looks upstream,
	// so the harness needs neither DNS nor a reachable destination.
	if err := connect(conn, sni+":443"); err != nil {
		r.Stage, r.Error = "connect", err.Error()
		return r
	}

	//nolint:gosec // G402: not verifying is the point; see the package comment.
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		r.Stage, r.Error = "handshake", err.Error()
		return r
	}
	defer tlsConn.Close()

	chain := tlsConn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		r.Stage, r.Error = "handshake", "completed with no peer certificates"
		return r
	}
	r.Handshake = true

	leaf := chain[0]
	r.Serial = leaf.SerialNumber.Text(16)
	r.NotBefore = leaf.NotBefore.UTC().Format(time.RFC3339)
	r.NotAfter = leaf.NotAfter.UTC().Format(time.RFC3339)
	r.SecondsToExpiry = int(time.Until(leaf.NotAfter).Round(time.Second).Seconds())
	r.ChainLen = len(chain)
	r.DNSNames = leaf.DNSNames

	r.Verified, r.VerifyError = verify(chain, sni, caFile)
	return r
}

// connect performs the CONNECT that opens the tunnel. The request line is
// written by hand: net/http would do it, but the tunnel has to be handed to
// tls.Client afterwards as a raw conn, and spelling out the three lines is
// clearer than borrowing a Transport to get its hijacked connection back.
//
// HTTP/1.1 specifically. openssl s_client -proxy sends 1.0 and Envoy answers
// 426 Upgrade Required, which is what made the older openssl-driven scripts
// here look like they were measuring something.
func connect(conn net.Conn, authority string) error {
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority); err != nil {
		return fmt.Errorf("writing CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return fmt.Errorf("reading CONNECT response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway refused CONNECT: %s", resp.Status)
	}
	// A CONNECT response has no body and Envoy sends nothing before the
	// tunnel opens, so anything the reader buffered would be bytes belonging
	// to the TLS handshake. There are none, but if that ever changes it must
	// be caught here rather than silently dropped along with br.
	if br.Buffered() != 0 {
		return fmt.Errorf("gateway sent %d bytes after CONNECT, before the tunnel", br.Buffered())
	}
	return nil
}

// verify is the check an actor cannot make: does the served chain stand up
// against the MITM root, for this name, right now. Expiry surfaces here and
// nowhere else.
func verify(chain []*x509.Certificate, sni, caFile string) (bool, string) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return false, fmt.Sprintf("reading %s: %v", caFile, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		return false, fmt.Sprintf("%s holds no certificates", caFile)
	}
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	_, err = chain[0].Verify(x509.VerifyOptions{
		DNSName:       sni,
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}
