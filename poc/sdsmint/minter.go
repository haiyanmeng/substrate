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
	"strings"
	"sync"
	"time"
)

// ErrHostNotAllowed is returned when a requested hostname fails the allowlist.
// The SDS server turns this into a resource removal rather than a certificate,
// which is how a server tells Envoy "this name does not exist".
var ErrHostNotAllowed = errors.New("host not allowed")

// Minter returns a leaf certificate for a hostname, minting one if needed.
// It is the analog of agentgateway's DynamicCaCertResolver: cache plus policy
// sitting in front of the signing key.
type Minter interface {
	// GetCertificate returns a fresh-or-cached leaf for host. It returns an
	// error if host is not allowed to be minted.
	GetCertificate(ctx context.Context, host string) (*MintedCert, error)
}

// reuseFraction is how much of a leaf's lifetime the cache will hand it out
// for. A cached leaf handed out at the very end of its life is worse than a
// cache miss: the caller gets a certificate that is about to stop verifying.
//
// It must stay strictly below server.go's rotateFraction. The rotation ticker
// refreshes by calling GetCertificate, so if the cache were still willing to
// serve the old leaf at rotation time the tick would be a no-op and the leaf
// would not actually be replaced until it had already expired. Half against
// two thirds leaves a comfortable margin; equality would be a race.
// TestRotationNeverServesAnExpiredLeaf is what fails if this inverts.
const reuseFraction = 1.0 / 2.0

type minter struct {
	ca       *CA
	validate func(host string) error // allowlist — LIMITS CA ABUSE
	ttl      time.Duration
	// reuse is how long a minted leaf may be served from cache: shorter than
	// ttl, so that a cache hit never hands back a nearly-dead certificate and
	// so that the rotation ticker always finds a stale entry. See
	// reuseFraction.
	reuse   time.Duration
	log     *slog.Logger
	metrics *Metrics

	mu    sync.Mutex
	cache *certCache
}

// MinterOptions configures NewMinter.
type MinterOptions struct {
	// Validate rejects hostnames that must never be minted. Required: a nil
	// Validate would turn this service into an unrestricted impersonation
	// oracle for every host its CA is trusted for.
	Validate func(host string) error
	// TTL is the leaf lifetime. agentgateway uses 300s; short TTLs limit the
	// blast radius of a leaked leaf key.
	TTL time.Duration
	// Cap bounds the cache. Zero means 256, matching agentgateway.
	Cap int
	// Logger receives one structured line per issuance (the audit log).
	Logger *slog.Logger
	// Metrics, if non-nil, counts issuance, cache hits and signing latency.
	// Optional: the audit log is the durable record, this is for measurement.
	Metrics *Metrics
}

// NewMinter builds a caching, allowlist-enforcing Minter over ca.
func NewMinter(ca *CA, opts MinterOptions) (Minter, error) {
	if ca == nil {
		return nil, fmt.Errorf("nil CA")
	}
	if opts.Validate == nil {
		return nil, fmt.Errorf("nil Validate: refusing to mint for unrestricted hostnames")
	}
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	if opts.Cap <= 0 {
		opts.Cap = 256
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &minter{
		ca:       ca,
		validate: opts.Validate,
		ttl:      opts.TTL,
		reuse:    time.Duration(float64(opts.TTL) * reuseFraction),
		log:      opts.Logger,
		metrics:  opts.Metrics,
		cache:    newCertCache(opts.Cap),
	}, nil
}

func (m *minter) GetCertificate(ctx context.Context, host string) (*MintedCert, error) {
	if err := m.validate(host); err != nil {
		// Denials are audited too — a burst of them is the signal that
		// something is probing the CA.
		m.metrics.recordDenial()
		m.log.WarnContext(ctx, "certificate request denied",
			slog.String("host", host),
			slog.String("reason", err.Error()),
		)
		return nil, fmt.Errorf("%w: %q: %w", ErrHostNotAllowed, host, err)
	}

	now := time.Now()

	m.mu.Lock()
	cached, ok := m.cache.get(host, now)
	m.mu.Unlock()
	if ok {
		m.metrics.recordCacheHit()
		return cached, nil
	}

	// Sign outside the lock: it generates a keypair, which is slow enough
	// that holding the map lock across it would serialise every handshake.
	// The cost is that concurrent misses on the same host may both sign; one
	// wins the cache slot and the loser's cert is still perfectly valid.
	cert, err := m.ca.Sign(host, m.ttl)
	if err != nil {
		return nil, err
	}
	signed := time.Now()
	m.metrics.recordMint(signed.Sub(now))

	m.mu.Lock()
	m.cache.put(host, cert, now, now.Add(m.reuse))
	m.mu.Unlock()

	// Guarded rather than left to the handler's own level check: the audit
	// line costs several allocations to assemble, and at a thousand mints a
	// second that assembly is itself a measurable share of the cost. Running
	// with --log-level warn has to be genuinely free, or the load phases are
	// measuring the logger.
	if m.log.Enabled(ctx, slog.LevelInfo) {
		m.log.InfoContext(ctx, "certificate issued",
			slog.String("host", host),
			slog.String("serial", cert.Serial),
			slog.Time("not_after", cert.NotAfter),
		)
	}
	return cert, nil
}

// AllowGlobs builds a validate function accepting hosts that match any of the
// given patterns (e.g. "*.example.com"). It rejects everything else, plus
// anything that is not a plausible hostname.
//
// A "*" label matches exactly one DNS label, so "*.example.com" matches
// "a.example.com" but not "a.b.example.com" and not the bare "example.com".
// This is deliberately narrower than shell globbing: with path.Match semantics
// a single star would span dots, and an operator writing "*.example.com" would
// silently authorise the CA for every depth of subdomain.
func AllowGlobs(patterns []string) func(string) error {
	// Split once, at construction. This runs on every request ahead of the
	// cache lookup, and splitting every pattern per call made the allowlist
	// cost an order of magnitude more than the cache hit it guards.
	split := make([][]string, len(patterns))
	for i, p := range patterns {
		split[i] = strings.Split(p, ".")
	}
	return func(host string) error {
		if err := checkHostSyntax(host); err != nil {
			return err
		}
		for _, p := range split {
			if matchLabels(p, host) {
				return nil
			}
		}
		return fmt.Errorf("no allowlist pattern matches")
	}
}

// matchLabels compares a dot-split pattern against a hostname, where a "*"
// pattern label matches any single non-empty host label.
//
// The host is walked label by label instead of being split, so a check
// allocates nothing at all. That matters because this runs ahead of the cache
// lookup on every request, hit or miss.
func matchLabels(pattern []string, host string) bool {
	for i, want := range pattern {
		label := host
		if j := strings.IndexByte(host, '.'); j >= 0 {
			label, host = host[:j], host[j+1:]
		} else if i != len(pattern)-1 {
			// The host ran out of labels before the pattern did.
			return false
		} else {
			host = ""
		}
		if want == "*" {
			if label == "" {
				return false
			}
			continue
		}
		if !strings.EqualFold(want, label) {
			return false
		}
	}
	// Anything left means the host had more labels than the pattern, which
	// must not match: "*.example.com" is one subdomain level, not any.
	return host == ""
}

// checkHostSyntax rejects names that should never reach the signer, regardless
// of the allowlist: empty, over-long, wildcarded, or containing separators
// that suggest the caller is trying to smuggle something past a pattern.
func checkHostSyntax(host string) error {
	switch {
	case host == "":
		return fmt.Errorf("empty hostname")
	case len(host) > 253:
		return fmt.Errorf("hostname longer than 253 bytes")
	case strings.ContainsAny(host, "*/\\ \t\r\n\x00"):
		return fmt.Errorf("hostname contains a disallowed character")
	case strings.HasPrefix(host, "."), strings.HasSuffix(host, "."):
		return fmt.Errorf("hostname has a leading or trailing dot")
	case strings.Contains(host, ".."):
		return fmt.Errorf("hostname has an empty label")
	}
	return nil
}
