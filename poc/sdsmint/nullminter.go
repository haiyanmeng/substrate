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
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// NullMinter is a Minter that does no signing on the request path.
//
// It exists for one reason: measuring Envoy. A mint costs ~375us of P-256
// work, which is orders of magnitude more than anything Envoy does per
// on-demand secret, so a load test driven through the real minter measures
// sdsmintd's signing loop and learns nothing about the data plane. Swapping
// this in removes the signer from the picture; what is left is Envoy plus the
// xDS plumbing.
//
// It pre-signs a small pool of wildcard leaves at construction and hands them
// out round-robin. Wildcard rather than per-host so that startup cost is a
// handful of signs instead of one per name, while the certificate a client
// receives still validates against the SNI it asked for. A pool rather than a
// single certificate because the SDS resource version is the serial: with one
// certificate every rotation push would carry the version Envoy already has,
// and the rotation phases would silently measure nothing.
//
// The allowlist still runs. It is sub-microsecond and it keeps the request
// path shaped like the real one.
type NullMinter struct {
	validate func(host string) error
	log      *slog.Logger
	metrics  *Metrics

	pool []*MintedCert
	next atomic.Uint64
}

// NullMinterOptions configures NewNullMinter.
type NullMinterOptions struct {
	// Validate is the allowlist, required for the same reason as on the real
	// minter: without it this is an impersonation oracle.
	Validate func(host string) error
	// Host is the name the pre-signed leaves are issued for. It should be a
	// wildcard covering the synthetic host set the load generator walks, e.g.
	// "*.mitm.example". Required.
	Host string
	// TTL is the lifetime of the pre-signed leaves. This is deliberately
	// separate from the server's rotation TTL: the rotation phases run a short
	// rotation period against long-lived certificates, because the subject
	// there is the push storm, not certificate validity.
	TTL time.Duration
	// Pool is how many leaves to pre-sign. Zero means 16.
	Pool    int
	Logger  *slog.Logger
	Metrics *Metrics
}

// NewNullMinter pre-signs the pool and returns a Minter over it.
func NewNullMinter(ca *CA, opts NullMinterOptions) (*NullMinter, error) {
	if ca == nil {
		return nil, fmt.Errorf("nil CA")
	}
	if opts.Validate == nil {
		return nil, fmt.Errorf("nil Validate: refusing to mint for unrestricted hostnames")
	}
	if opts.Host == "" {
		return nil, fmt.Errorf("null minter needs a Host to pre-sign for")
	}
	if !strings.HasPrefix(opts.Host, "*.") {
		// Not fatal -- a single-host run is a legitimate thing to measure --
		// but it is almost always a mistake, and the failure it causes is a
		// certificate-name mismatch on the client, which is a confusing way to
		// find out.
		if opts.Logger != nil {
			opts.Logger.Warn("null minter host is not a wildcard; every SNI other than this one will fail client verification",
				slog.String("host", opts.Host))
		}
	}
	if opts.TTL <= 0 {
		opts.TTL = 24 * time.Hour
	}
	if opts.Pool <= 0 {
		opts.Pool = 16
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	pool := make([]*MintedCert, opts.Pool)
	start := time.Now()
	for i := range pool {
		cert, err := ca.Sign(opts.Host, opts.TTL)
		if err != nil {
			return nil, fmt.Errorf("pre-signing the null minter pool: %w", err)
		}
		pool[i] = cert
	}
	opts.Logger.Warn("null minter enabled: certificates are pre-signed and shared, so this measures Envoy and not the signer",
		slog.String("host", opts.Host),
		slog.Int("pool", opts.Pool),
		slog.Duration("ttl", opts.TTL),
		slog.Duration("presign_took", time.Since(start)),
	)

	return &NullMinter{
		validate: opts.Validate,
		log:      opts.Logger,
		metrics:  opts.Metrics,
		pool:     pool,
	}, nil
}

// GetCertificate returns the next leaf from the pool. It never signs and never
// blocks.
func (n *NullMinter) GetCertificate(ctx context.Context, host string) (*MintedCert, error) {
	if err := n.validate(host); err != nil {
		n.metrics.recordDenial()
		return nil, fmt.Errorf("%w: %q: %w", ErrHostNotAllowed, host, err)
	}
	// Counted as a mint so that the rate the harness reads back is the request
	// rate, with a signing cost of roughly zero -- which is the whole point.
	n.metrics.recordMint(0)
	i := n.next.Add(1) - 1
	return n.pool[i%uint64(len(n.pool))], nil
}

// Sample returns one of the pre-signed leaves. The control phase configures
// Envoy with a static certificate, and using a certificate from this pool
// makes that comparison change exactly one variable: the certificate bytes are
// identical, only the selector differs.
func (n *NullMinter) Sample() *MintedCert { return n.pool[0] }
