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
	"log/slog"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/internal/localca"
)

// serialBits is the size of the random serial numbers we generate. RFC 5280
// caps serials at 20 octets; 128 bits leaves room for the sign bit.
const serialBits = 128

// renewFraction is how far into an intermediate's life we re-issue it. The
// same 2/3 the SDS server uses for leaf rotation, for the same reason: it
// leaves a full third of the lifetime as slack for a failure to retry in.
const renewFraction = 2.0 / 3.0

// Options configures how a CA is constructed. The zero value signs leaves
// directly with the root key and requires the root to be name-constrained.
type Options struct {
	// AllowUnconstrained permits a CA that carries no dNSName name constraint.
	//
	// It defaults to false because an unconstrained MITM CA is the worst
	// artifact this service can produce: whoever holds its key can forge a
	// certificate for any name on the internet that every actor configured to
	// trust it will accept. A constrained one can only forge names beneath the
	// permitted domains. That difference is worth more than any amount of care
	// about where the key file sits, so the unsafe case has to be asked for.
	AllowUnconstrained bool

	// IntermediateLifetime, if positive, makes the CA generate a short-lived
	// intermediate in memory, have the root sign it, and issue leaves from
	// that instead of from the root directly. Chains become
	// [leaf, intermediate, root]; the trust anchor clients pin is unchanged.
	//
	// This is what lets the root key be held somewhere the signing path cannot
	// reach on every request -- an external crypto.Signer backed by a KMS, or
	// simply a file read once at startup -- while leaf signing stays a local
	// operation at full speed. It also bounds a compromise of this process to
	// the intermediate's remaining lifetime rather than the root's.
	//
	// The root must permit an intermediate: a root with a pathLenConstraint of
	// zero cannot, and NewCA refuses rather than emit chains that fail to
	// verify at handshake time.
	IntermediateLifetime time.Duration

	// Logger receives one line per intermediate issuance. Optional.
	Logger *slog.Logger
}

// issuer is the certificate and key that leaves are actually signed with:
// either the root itself, or the current delegated intermediate. It is
// immutable, and replaced wholesale on renewal, so Sign can read it without a
// lock.
type issuer struct {
	cert     *x509.Certificate
	key      crypto.Signer
	chainDER [][]byte // issuer chain toward the root, nearest first
	// renewAt is when this issuer should be replaced. Zero means never, which
	// is the case when leaves are signed by the root directly.
	renewAt time.Time
}

// CA signs leaf certificates for arbitrary hostnames. It is the only component
// in the system that holds the MITM signing key.
type CA struct {
	// root is the trust anchor: what clients are configured to trust, and what
	// CertificatePEM writes out. It is not necessarily what signs leaves.
	root    *x509.Certificate
	rootKey crypto.Signer

	intermediateLifetime time.Duration
	logger               *slog.Logger

	// current is read on every Sign and written only on renewal.
	current atomic.Pointer[issuer]
	// renewMu serialises renewal so a burst of concurrent Signs past the
	// renewal point issues one intermediate rather than one per caller.
	renewMu sync.Mutex
}

// Certificate returns the CA's trust anchor. Callers use it to write out the
// certificate that MITM'd clients must be configured with. With a delegated
// intermediate in play this is still the root, which is the certificate
// clients pin -- the intermediate travels in the chain instead.
func (c *CA) Certificate() *x509.Certificate { return c.root }

// CertificatePEM returns the CA's trust anchor in PEM form.
func (c *CA) CertificatePEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.root.Raw})
}

// IssuerCertificate returns the certificate currently signing leaves: the root
// itself, or the live delegated intermediate.
func (c *CA) IssuerCertificate() *x509.Certificate { return c.current.Load().cert }

// NewCA builds a signing CA from a trust anchor and its key.
//
// key is a crypto.Signer rather than a concrete key type so that a signer
// whose private key never leaves a KMS or HSM can be substituted for a parsed
// one. Nothing below this point needs the key bytes.
func NewCA(root *x509.Certificate, key crypto.Signer, opts Options) (*CA, error) {
	if root == nil {
		return nil, fmt.Errorf("ca cert: missing")
	}
	if key == nil {
		return nil, fmt.Errorf("ca key: missing")
	}
	if !root.IsCA {
		return nil, fmt.Errorf("ca cert: %q is not a CA certificate", root.Subject)
	}
	if err := checkNameConstraint(root, opts.AllowUnconstrained); err != nil {
		return nil, err
	}
	if !keyMatchesCert(root, key) {
		return nil, fmt.Errorf("ca key: public key does not match the certificate for %q", root.Subject)
	}

	ca := &CA{
		root:                 root,
		rootKey:              key,
		intermediateLifetime: opts.IntermediateLifetime,
		logger:               opts.Logger,
	}

	if opts.IntermediateLifetime <= 0 {
		ca.current.Store(&issuer{cert: root, key: key, chainDER: [][]byte{root.Raw}})
		return ca, nil
	}

	// A pathLenConstraint of zero means "may only issue end-entity
	// certificates". Signing an intermediate under such a root produces a
	// chain that every conforming verifier rejects, and the failure surfaces
	// as an opaque TLS handshake error in whatever client the actor happens to
	// run. Refuse here, where the message can say what to do about it.
	if root.MaxPathLenZero {
		return nil, fmt.Errorf(
			"ca cert: %q has a pathLenConstraint of 0 and cannot issue the intermediate that "+
				"IntermediateLifetime asks for; regenerate the root with a path length of at least 1, "+
				"or sign leaves with the root directly",
			root.Subject)
	}
	if err := ca.renew(); err != nil {
		return nil, err
	}
	return ca, nil
}

// checkNameConstraint enforces the default that a MITM CA must say which names
// it is allowed to impersonate.
func checkNameConstraint(cert *x509.Certificate, allowUnconstrained bool) error {
	if len(cert.PermittedDNSDomains) > 0 {
		return nil
	}
	if allowUnconstrained {
		return nil
	}
	return fmt.Errorf(
		"ca cert: %q carries no dNSName name constraint, so its key can forge a certificate for "+
			"any name on the internet that clients trusting it will accept; regenerate it with "+
			"permitted DNS domains, or pass AllowUnconstrained to accept that risk deliberately",
		cert.Subject)
}

// keyMatchesCert catches a mismatched cert/key pair at load rather than at the
// first handshake. x509.CreateCertificate will happily sign with the wrong key
// and produce a chain nothing can verify.
func keyMatchesCert(cert *x509.Certificate, key crypto.Signer) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	pub, ok := cert.PublicKey.(equaler)
	if !ok {
		// An unrecognised key type cannot be compared. Do not block on it.
		return true
	}
	return pub.Equal(key.Public())
}

// LoadCA parses a CA certificate and private key from PEM.
//
// Prefer FromPool: it is the shape the rest of substrate stores CAs in, and it
// carries the intermediates that a delegated signing setup needs. This exists
// for tests and for bring-your-own-PEM deployments.
func LoadCA(certPEM, keyPEM []byte, opts Options) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("ca cert: no CERTIFICATE PEM block found")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
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

	return NewCA(cert, signer, opts)
}

// FromPool builds a signing CA from a localca pool, the format substrate's
// other CAs are stored and mounted in. id selects a CA within the pool; empty
// takes the first.
func FromPool(pool *localca.Pool, id string, opts Options) (*CA, error) {
	if pool == nil || len(pool.CAs) == 0 {
		return nil, fmt.Errorf("ca pool: empty")
	}

	entry := pool.CAs[0]
	if id != "" {
		entry = nil
		ids := make([]string, 0, len(pool.CAs))
		for _, candidate := range pool.CAs {
			ids = append(ids, candidate.ID)
			if candidate.ID == id {
				entry = candidate
				break
			}
		}
		if entry == nil {
			return nil, fmt.Errorf("ca pool: no CA with ID %q (have %q)", id, ids)
		}
	}
	if entry.SigningKey == nil {
		return nil, fmt.Errorf("ca pool: CA %q has no signing key", entry.ID)
	}

	return NewCA(entry.RootCertificate, entry.SigningKey, opts)
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

// GenerateRootOptions configures GenerateRoot.
type GenerateRootOptions struct {
	CommonName string
	Lifetime   time.Duration
	// PermittedDNSDomains name-constrains the root. Leaving it empty produces
	// a CA that NewCA will refuse without Options.AllowUnconstrained.
	PermittedDNSDomains []string
	// AllowIntermediate gives the root a pathLenConstraint of 1 so it can
	// delegate leaf signing. Without it the root gets a pathLenConstraint of
	// 0, which is the tighter and more common case.
	AllowIntermediate bool
}

// GenerateRoot creates a throwaway self-signed MITM root. This exists so the
// PoC can run without external key material; a real deployment supplies a
// managed pool and loads it with FromPool.
//
// It returns a localca.CA so the caller can serialise it into a pool file in
// the same format every other substrate CA uses.
func GenerateRoot(opts GenerateRootOptions) (*localca.CA, error) {
	maxPathLen := 0
	if opts.AllowIntermediate {
		maxPathLen = 1
	}
	// P-256 rather than substrate's usual Ed25519: these leaves are validated
	// by whatever HTTP client an actor happens to run, and Ed25519 in a chain
	// needs OpenSSL 1.1.1+. Nothing here is hot enough for the size difference
	// to matter.
	return localca.GenerateCA(localca.GenerateOptions{
		ID:                  "mitm",
		CommonName:          opts.CommonName,
		KeyType:             localca.KeyTypeECDSAP256,
		Lifetime:            opts.Lifetime,
		PermittedDNSDomains: opts.PermittedDNSDomains,
		MaxPathLen:          &maxPathLen,
	})
}

// GenerateCA creates a throwaway self-signed MITM CA ready to sign, along with
// the pool entry it came from so the caller can persist it.
func GenerateCA(commonName string, ttl time.Duration, permittedDNSDomains []string, opts Options) (*CA, *localca.CA, error) {
	entry, err := GenerateRoot(GenerateRootOptions{
		CommonName:          commonName,
		Lifetime:            ttl,
		PermittedDNSDomains: permittedDNSDomains,
		AllowIntermediate:   opts.IntermediateLifetime > 0,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA: %w", err)
	}
	ca, err := NewCA(entry.RootCertificate, entry.SigningKey, opts)
	if err != nil {
		return nil, nil, err
	}
	return ca, entry, nil
}

// issuerFor returns the issuer to sign with, renewing it first if the current
// one has reached its renewal point.
func (c *CA) issuerFor(now time.Time) (*issuer, error) {
	iss := c.current.Load()
	if iss.renewAt.IsZero() || now.Before(iss.renewAt) {
		return iss, nil
	}

	c.renewMu.Lock()
	defer c.renewMu.Unlock()
	// Re-read: another goroutine may have renewed while we waited.
	if iss := c.current.Load(); iss.renewAt.IsZero() || now.Before(iss.renewAt) {
		return iss, nil
	}
	if err := c.renew(); err != nil {
		return nil, err
	}
	return c.current.Load(), nil
}

// renew issues a fresh delegated intermediate and installs it. Callers hold
// renewMu, except at construction where nothing else can observe the CA yet.
func (c *CA) renew() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating intermediate key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}

	now := time.Now()
	notAfter := now.Add(c.intermediateLifetime)
	// An intermediate that outlives its root produces a chain that stops
	// verifying partway through the intermediate's stated life.
	if notAfter.After(c.root.NotAfter) {
		notAfter = c.root.NotAfter
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: delegatedCN(c.root)},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	// Restate the root's constraint on the intermediate. A verifier applies
	// the root's constraints to the whole chain regardless, so this changes no
	// outcome; it means the intermediate is still constrained when read on its
	// own, which is how it will be read in an incident.
	if len(c.root.PermittedDNSDomains) > 0 {
		tmpl.PermittedDNSDomains = c.root.PermittedDNSDomains
		tmpl.PermittedDNSDomainsCritical = true
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.root, key.Public(), c.rootKey)
	if err != nil {
		return fmt.Errorf("signing intermediate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("reparsing intermediate: %w", err)
	}

	lifetime := notAfter.Sub(now)
	c.current.Store(&issuer{
		cert:     cert,
		key:      key,
		chainDER: [][]byte{der, c.root.Raw},
		renewAt:  now.Add(time.Duration(float64(lifetime) * renewFraction)),
	})

	if c.logger != nil {
		c.logger.Info("issued a delegated signing intermediate",
			slog.String("subject", cert.Subject.CommonName),
			slog.String("serial", serial.Text(16)),
			slog.Time("not_after", notAfter),
			slog.String("issuer", c.root.Subject.String()),
		)
	}
	return nil
}

func delegatedCN(root *x509.Certificate) string {
	if root.Subject.CommonName == "" {
		return "delegated signer"
	}
	return root.Subject.CommonName + " delegated signer"
}

// MintedCert is a freshly issued leaf plus its private key, in the PEM form
// Envoy's Secret proto expects.
type MintedCert struct {
	CertChainPEM  []byte // leaf, then any intermediate, then the root, PEM
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

	now := time.Now()
	iss, err := c.issuerFor(now)
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating leaf key for %q: %w", host, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	notAfter := now.Add(ttl)
	// A leaf that outlives its issuer is accepted now and rejected later, for
	// reasons that point at the leaf rather than at the CA behind it.
	if notAfter.After(iss.cert.NotAfter) {
		notAfter = iss.cert.NotAfter
	}
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

	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, iss.cert, key.Public(), iss.key)
	if err != nil {
		return nil, fmt.Errorf("signing leaf for %q: %w", host, err)
	}

	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	for _, der := range iss.chainDER {
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
