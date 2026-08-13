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

package sdsmint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

// errHostNotAllowed is returned when a requested hostname will not be minted.
var errHostNotAllowed = errors.New("host not allowed")

// minter returns a leaf certificate for a hostname, minting one if needed, and
// caches what it mints. It is safe for concurrent use.
type minter struct {
	signer *certauth.Signer
	ttl    time.Duration
	// reuseInterval is how long a minted leaf may be served from cache: shorter than
	// ttl, so that a cache hit never hands back a nearly-dead certificate and
	// so that the rotation ticker always finds a stale entry. See
	// reuseFraction.
	reuseInterval time.Duration
	log           *slog.Logger
	metrics       *metrics
	// cache does its own locking, which is what keeps signing off the lock:
	// a miss returns before Sign is called and put takes the lock again after.
	cache *certCache
}

// minterOptions configures newMinter.
type minterOptions struct {
	// TTL is the leaf lifetime.
	TTL time.Duration
	// CacheCapacity bounds the cache. Zero means 256.
	CacheCapacity int
	Logger        *slog.Logger
	Metrics       *metrics
}

// defaultTTL for leaf cert lifetime.
const defaultTTL = 15 * time.Minute

// reuseFraction is how much of a leaf's lifetime the cache will hand it out
// for. A cached leaf handed out at the very end of its life is worse than a
// cache miss: the caller gets a certificate that is about to stop verifying.
//
// It must stay strictly below deltastream.go's rotateFraction. The rotation
// ticker refreshes by calling Certificate, so if the cache were still
// willing to serve the old leaf at rotation time the tick would be a no-op and
// the leaf would not actually be replaced until it had already expired. Half
// against two thirds leaves a comfortable margin; equality would be a race.
// TestRotationNeverServesAnExpiredLeaf is what fails if this inverts.
const reuseFraction = 0.5

// newMinter builds a caching minter over signer.
func newMinter(signer *certauth.Signer, opts minterOptions) (*minter, error) {
	if signer == nil {
		return nil, errors.New("nil signer")
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	if opts.CacheCapacity <= 0 {
		opts.CacheCapacity = 256
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &minter{
		signer:        signer,
		ttl:           opts.TTL,
		reuseInterval: time.Duration(float64(opts.TTL) * reuseFraction),
		log:           opts.Logger,
		metrics:       opts.Metrics,
		cache:         newCertCache(opts.CacheCapacity),
	}, nil
}

// certificate returns a fresh-or-cached leaf for host. It returns an error
// wrapping errHostNotAllowed if host is not a name this will mint for.
func (m *minter) certificate(ctx context.Context, host string) (*certauth.MintedCert, error) {
	if err := checkHostSyntax(host); err != nil {
		m.metrics.recordDenial(ctx)
		m.log.WarnContext(ctx, "certificate request denied",
			slog.String("host", host),
			slog.String("reason", err.Error()),
		)
		// checkHostSyntax already quotes the host, so this does not repeat it.
		return nil, fmt.Errorf("%w: %w", errHostNotAllowed, err)
	}

	now := time.Now()

	cached, ok := m.cache.get(host, now)
	if ok {
		m.metrics.recordCacheHit(ctx)
		return cached, nil
	}

	cert, err := m.signer.Sign(host, m.ttl)
	if err != nil {
		return nil, err
	}
	signed := time.Now()
	m.metrics.recordMint(ctx, signed.Sub(now))

	m.cache.put(host, cert, now.Add(m.reuseInterval))

	if m.log.Enabled(ctx, slog.LevelInfo) {
		m.log.InfoContext(ctx, "certificate issued",
			slog.String("host", host),
			slog.String("serial", cert.Serial),
			slog.Time("not_after", cert.NotAfter),
		)
	}
	return cert, nil
}

// forget releases the cached leaf for host, reporting whether one was actually
// held. The SDS server calls it when it withdraws a name from the data plane,
// so the certificate is dropped on both sides rather than lingering here until
// capacity or its reuse deadline pushes it out.
func (m *minter) forget(host string) bool {
	return m.cache.forget(host)
}

// checkHostSyntax checks whether host is a valid DNS name or IP address.
func checkHostSyntax(host string) error {
	if isValidDNSName(host) || isValidIPAddress(host) {
		return nil
	}
	return fmt.Errorf("invalid host name %q", host)
}

// isValidDNSName reports whether host is a syntactically valid DNS name.
func isValidDNSName(host string) bool {
	// A trailing dot names the root explicitly. It is legal in a DNS name and
	// not in SNI, but Envoy passes on whatever it was given, so it is dropped
	// here rather than making the final label look empty.
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !isValidDNSLabel(label) {
			return false
		}
	}
	return true
}

// isValidDNSLabel reports whether one dot-separated component is a legal label:
// letters, digits, and interior hyphens, up to 63 bytes.
func isValidDNSLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	for i := 0; i < len(label); i++ {
		switch c := label[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		// A hyphen may not open or close a label.
		case c == '-' && i > 0 && i < len(label)-1:
		// Not legal in a hostname, but real names carry it -- service records
		// and a fair number of internal names -- and it smuggles nothing into a
		// certificate. Refusing it would fail handshakes for no gain.
		case c == '_':
		default:
			return false
		}
	}
	return true
}

// isValidIPAddress reports whether host is an IP literal, v4 or v6. SNI is not
// supposed to carry an IP addresss, but some clients send it anyway.
func isValidIPAddress(host string) bool {
	return net.ParseIP(host) != nil
}
