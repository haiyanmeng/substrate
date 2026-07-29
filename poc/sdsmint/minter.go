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

type cacheEntry struct {
	cert     *MintedCert
	expires  time.Time
	lastUsed time.Time
}

type minter struct {
	ca       *CA
	validate func(host string) error // allowlist — LIMITS CA ABUSE
	ttl      time.Duration
	cap      int
	log      *slog.Logger

	mu    sync.Mutex
	cache map[string]*cacheEntry
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
		cap:      opts.Cap,
		log:      opts.Logger,
		cache:    make(map[string]*cacheEntry),
	}, nil
}

func (m *minter) GetCertificate(ctx context.Context, host string) (*MintedCert, error) {
	if err := m.validate(host); err != nil {
		// Denials are audited too — a burst of them is the signal that
		// something is probing the CA.
		m.log.WarnContext(ctx, "certificate request denied",
			slog.String("host", host),
			slog.String("reason", err.Error()),
		)
		return nil, fmt.Errorf("%w: %q: %w", ErrHostNotAllowed, host, err)
	}

	now := time.Now()

	m.mu.Lock()
	if e, ok := m.cache[host]; ok && now.Before(e.expires) {
		e.lastUsed = now
		cert := e.cert
		m.mu.Unlock()
		return cert, nil
	}
	m.mu.Unlock()

	// Sign outside the lock: it generates a keypair, which is slow enough
	// that holding the map lock across it would serialise every handshake.
	// The cost is that concurrent misses on the same host may both sign; one
	// wins the cache slot and the loser's cert is still perfectly valid.
	cert, err := m.ca.Sign(host, m.ttl)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.evictLocked(now)
	m.cache[host] = &cacheEntry{cert: cert, expires: now.Add(m.ttl), lastUsed: now}
	m.mu.Unlock()

	m.log.InfoContext(ctx, "certificate issued",
		slog.String("host", host),
		slog.String("serial", cert.Serial),
		slog.Time("not_after", cert.NotAfter),
	)
	return cert, nil
}

// evictLocked drops expired entries, then the least-recently-used ones until
// the cache is back under cap. Callers must hold m.mu.
func (m *minter) evictLocked(now time.Time) {
	for host, e := range m.cache {
		if !now.Before(e.expires) {
			delete(m.cache, host)
		}
	}
	for len(m.cache) >= m.cap {
		var oldestHost string
		var oldest time.Time
		for host, e := range m.cache {
			if oldestHost == "" || e.lastUsed.Before(oldest) {
				oldestHost, oldest = host, e.lastUsed
			}
		}
		if oldestHost == "" {
			return
		}
		delete(m.cache, oldestHost)
	}
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
	return func(host string) error {
		if err := checkHostSyntax(host); err != nil {
			return err
		}
		hostLabels := strings.Split(host, ".")
		for _, p := range patterns {
			if matchLabels(strings.Split(p, "."), hostLabels) {
				return nil
			}
		}
		return fmt.Errorf("no allowlist pattern matches")
	}
}

// matchLabels compares a dot-split pattern against a dot-split hostname, where
// a "*" pattern label matches any single non-empty host label.
func matchLabels(pattern, host []string) bool {
	if len(pattern) != len(host) {
		return false
	}
	for i, want := range pattern {
		if want == "*" {
			if host[i] == "" {
				return false
			}
			continue
		}
		if !strings.EqualFold(want, host[i]) {
			return false
		}
	}
	return true
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
