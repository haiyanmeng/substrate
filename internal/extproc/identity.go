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

package extproc

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"strings"

	"github.com/agent-substrate/substrate/internal/substratex509"
)

// XFCCHeader is where the CONNECT checkpoint learns who is calling.
//
// The actor's identity lives in a custom X.509 extension on its client
// certificate (substratex509.ActorIdentity), not in a SAN, so no Envoy CEL
// attribute can surface it -- ext_proc has to be handed the whole certificate
// and parse it. Envoy fills this header from the verified peer certificate at
// HCM ingress, before the filter chain runs, which is what makes it usable
// here. A route-level request_headers_to_add with %DOWNSTREAM_PEER_CERT% would
// not be: the router filter runs after ext_proc.
//
// SECURITY: the gateway's HCM must set forward_client_cert_details:
// SANITIZE_SET. That makes Envoy overwrite whatever the client sent under this
// name rather than append to it. With APPEND_FORWARD an actor could prepend a
// forged element and choose its own identity.
const XFCCHeader = "x-forwarded-client-cert"

// actorFromXFCC extracts the authenticated actor from Envoy's
// x-forwarded-client-cert header.
//
// The three return values distinguish three outcomes the caller must treat
// differently:
//
//   - (key, uid, nil)      an actor certificate, valid and for atunnel.
//   - (zero, "", nil)      a valid peer certificate that carries no
//     ActorIdentity -- an ordinary pod, such as the e2e
//     egressprobe. Not an error; it simply has no policy,
//     and the caller denies it for that reason.
//   - (zero, "", err)      malformed input, or an identity that fails
//     validation. Deny and log.
func actorFromXFCC(value string) (ActorKey, string, error) {
	if strings.TrimSpace(value) == "" {
		return ActorKey{}, "", fmt.Errorf("no %s header: the listener is not configured to forward the client certificate", XFCCHeader)
	}

	certPEM, ok := xfccField(value, "Cert")
	if !ok {
		return ActorKey{}, "", fmt.Errorf("%s carries no Cert field: set_current_client_cert_details must set cert: true", XFCCHeader)
	}

	cert, err := parsePeerCertificate(certPEM)
	if err != nil {
		return ActorKey{}, "", err
	}

	// This call is the egress PEP the atelet credential broker was waiting for.
	// It returns (nil, nil) when the extension is absent, and validation inside
	// it rejects any Purpose other than atunnel -- so a certificate minted for
	// some future purpose cannot be replayed at the egress gateway.
	identity, err := substratex509.ActorIdentityFromCertificate(cert)
	if err != nil {
		return ActorKey{}, "", fmt.Errorf("reading actor identity from the peer certificate: %w", err)
	}
	if identity == nil {
		return ActorKey{}, "", nil
	}
	return ActorKey{Atespace: identity.Atespace, Name: identity.ActorName}, identity.ActorUid, nil
}

// parsePeerCertificate decodes the percent-encoded PEM Envoy puts in the Cert
// field.
//
// url.PathUnescape, not QueryUnescape: Envoy percent-encodes, and
// QueryUnescape would additionally turn '+' into a space. Base64 uses '+', so
// that corrupts roughly half of all certificates -- and the ones it corrupts
// depend on the key material, which makes it an intermittent bug.
func parsePeerCertificate(encoded string) (*x509.Certificate, error) {
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return nil, fmt.Errorf("percent-decoding the peer certificate: %w", err)
	}
	block, _ := pem.Decode([]byte(decoded))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("peer certificate is not a PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the peer certificate: %w", err)
	}
	return cert, nil
}

// xfccField returns one key's value from the last element of an XFCC header.
//
// The last element rather than the first: XFCC is ordered outermost hop first,
// so the nearest peer -- the one Envoy just authenticated -- is at the end.
// Under the SANITIZE_SET the gateway is configured with there is only ever one,
// but reading the wrong end would fail silently if that ever changed.
//
// Keys are matched case-insensitively, per the header's own grammar.
func xfccField(header, key string) (string, bool) {
	elements := splitUnquoted(header, ',')
	if len(elements) == 0 {
		return "", false
	}
	for _, kv := range splitUnquoted(elements[len(elements)-1], ';') {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		return unquoteXFCC(strings.TrimSpace(v)), true
	}
	return "", false
}

// splitUnquoted splits on sep, ignoring separators inside a double-quoted
// section. Both separators need this: a Subject is a DN and contains commas,
// and a quoted value may contain either.
func splitUnquoted(s string, sep byte) []string {
	var (
		out      []string
		start    int
		inQuotes bool
	)
	for i := 0; i < len(s); i++ {
		switch {
		case inQuotes && s[i] == '\\' && i+1 < len(s):
			i++ // Skip the escaped character, quote included.
		case s[i] == '"':
			inQuotes = !inQuotes
		case s[i] == sep && !inQuotes:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// unquoteXFCC strips surrounding quotes and undoes the header's backslash
// escaping.
func unquoteXFCC(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	v = v[1 : len(v)-1]
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}
