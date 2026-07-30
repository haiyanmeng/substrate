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

// Package sdsmint implements a minting SDS server: an Envoy Secret Discovery
// Service that mints a TLS leaf certificate on demand for whatever hostname
// Envoy asks for, where the requested SDS resource name is the SNI from the
// client hello.
//
// The point of the design is that the MITM CA private key lives here, in a
// dedicated service, rather than in every data-plane proxy. Only short-lived
// leaf keys ever transit to Envoy, over a local-only channel (UDS).
package sdsmint

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// serialBits is the size of the random serial numbers we generate. RFC 5280
// caps serials at 20 octets; 128 bits leaves room for the sign bit.
const serialBits = 128

// CA signs leaf certificates for arbitrary hostnames. It is the only component
// in the system that holds the MITM signing key.
type CA struct {
	cert *x509.Certificate
	// key is a crypto.Signer rather than a concrete key so that a KMS- or
	// HSM-backed signer can be substituted without touching Sign. Getting the
	// CA key out of a file is the main hardening step for production, and this
	// is the seam that makes it a drop-in rather than a rewrite.
	key      crypto.Signer
	chainDER [][]byte // CA cert DER, appended to every leaf chain
}

// Certificate returns the CA certificate. Callers use it to write out the
// trust anchor that MITM'd clients must be configured with.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// CertificatePEM returns the CA certificate in PEM form.
func (c *CA) CertificatePEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

// LoadCA parses a CA certificate and private key from PEM. In production,
// prefer a KMS/HSM signer over a raw PEM key on disk.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("ca cert: no CERTIFICATE PEM block found")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("ca cert: %q is not a CA certificate", cert.Subject)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca key: no PEM block found")
	}
	key, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("ca key: %T does not implement crypto.Signer", key)
	}

	return &CA{cert: cert, key: signer, chainDER: [][]byte{cert.Raw}}, nil
}

func parsePrivateKey(der []byte) (any, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("not a PKCS#8, SEC 1, or PKCS#1 private key")
	}
	return k, nil
}

// GenerateCA creates a throwaway self-signed MITM CA. This exists so the PoC
// can run without external key material; a real deployment loads a managed CA
// via LoadCA (or a KMS signer).
//
// permittedDNSDomains, if non-empty, sets an x509 name constraint on the CA so
// that even a leaked leaf-signing path cannot impersonate hosts outside those
// domains -- a name-constrained CA.
func GenerateCA(commonName string, ttl time.Duration, permittedDNSDomains []string) (*CA, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	if len(permittedDNSDomains) > 0 {
		tmpl.PermittedDNSDomainsCritical = true
		tmpl.PermittedDNSDomains = permittedDNSDomains
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("self-signing CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reparsing CA: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshalling CA key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return &CA{cert: cert, key: key, chainDER: [][]byte{der}}, certPEM, keyPEM, nil
}

// MintedCert is a freshly issued leaf plus its private key, in the PEM form
// Envoy's Secret proto expects.
type MintedCert struct {
	CertChainPEM  []byte // leaf then CA, PEM
	PrivateKeyPEM []byte // leaf private key, PEM
	NotAfter      time.Time
	Serial        string // hex, for the issuance audit log
}

// Sign issues a leaf certificate for host, signed by the CA.
//
// The leaf gets SAN = host, CN = host, IsCA = false, KeyUsage =
// DigitalSignature, EKU = ServerAuth, and a fresh keypair on every call. This
// mirrors what agentgateway's rustls resolver produces, so a client cannot
// tell the two implementations apart.
func (c *CA) Sign(host string, ttl time.Duration) (*MintedCert, error) {
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating leaf key for %q: %w", host, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	notAfter := now.Add(ttl)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		// Skew tolerance: clients and the proxy are not necessarily in sync,
		// and leaf TTLs here are deliberately short.
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	// A literal IP in the SNI position has to land in IPAddresses, not
	// DNSNames, or clients will reject the leaf.
	if ip := parseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else {
		tmpl.DNSNames = []string{host}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		return nil, fmt.Errorf("signing leaf for %q: %w", host, err)
	}

	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	for _, der := range c.chainDER {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshalling leaf key for %q: %w", host, err)
	}

	return &MintedCert{
		CertChainPEM:  chain,
		PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		NotAfter:      notAfter,
		Serial:        serial.Text(16),
	}, nil
}

// parseIP returns the IP if host is a bare address literal, else nil. Envoy
// will hand us whatever was in the SNI, and although SNI is not supposed to
// carry IP literals, some clients send them anyway.
func parseIP(host string) net.IP {
	return net.ParseIP(host)
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), serialBits)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}
	return serial, nil
}
