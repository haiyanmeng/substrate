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

package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// ExemptionSet is a normalized set of SNI patterns whose TLS the egress gateway
// must not terminate, addressed by a content-derived ID.
//
// Envoy cannot compare one runtime value against another, so the gateway cannot
// be handed a per-connection list and asked to search it. The ID is the way
// around that: every distinct set becomes its own pre-rendered filter chain, and
// the only per-connection value is the ID naming which one to use.
//
// Two actors that exempt the same destinations therefore share a set, which is
// what keeps the rendered listener proportional to the number of distinct
// policies rather than to the number of actors.
type ExemptionSet struct {
	id       string
	patterns []string
}

// NewExemptionSet normalizes patterns into a set: hostnames are lowercased and
// stripped of the trailing root dot, blanks are dropped, and duplicates are
// collapsed. The result is sorted so that two policies listing the same
// destinations in different orders produce the same ID and share one chain.
func NewExemptionSet(patterns []string) ExemptionSet {
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern = normalizePattern(pattern); pattern != "" {
			normalized = append(normalized, pattern)
		}
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	if len(normalized) == 0 {
		return ExemptionSet{}
	}
	return ExemptionSet{id: fingerprint(normalized), patterns: normalized}
}

// normalizePattern puts one pattern into the form the SNI is compared against.
// Envoy hands us the SNI as the client sent it, and a client may send either
// case and may or may not include the root label's dot.
func normalizePattern(pattern string) string {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	// A single "." is the root, not a hostname with a trailing dot.
	if len(pattern) > 1 {
		pattern = strings.TrimSuffix(pattern, ".")
	}
	return pattern
}

// fingerprint derives the ID of a normalized, non-empty pattern set.
func fingerprint(patterns []string) string {
	// "\n" cannot occur in a validated hostname pattern, so no two distinct sets
	// can hash the same joined string.
	sum := sha256.Sum256([]byte(strings.Join(patterns, "\n")))
	// exemptionIDLength is how much of the digest identifies a set. 128 bits
	// leaves the chance of two live sets colliding — and so of one actor's
	// connection being matched against the other's exemptions — far below the
	// chance of the gateway being wrong for any other reason. The registry
	// rejects a collision outright rather than trusting that bound.
	const exemptionIDLength = 32
	return hex.EncodeToString(sum[:])[:exemptionIDLength]
}

// ID names this set in the gateway's configuration, and is the value ext_proc
// hands the dispatch listener. It is empty for the empty set, which needs no
// configuration at all: a connection carrying no ID is intercepted.
func (s ExemptionSet) ID() string { return s.id }

// IsEmpty reports whether the set exempts nothing.
func (s ExemptionSet) IsEmpty() bool { return len(s.patterns) == 0 }

// Patterns returns the normalized, sorted patterns.
func (s ExemptionSet) Patterns() []string { return slices.Clone(s.patterns) }

// ExemptionRegistry publishes exemption sets to the egress gateway as listener
// configuration.
//
// The control plane has no way to enumerate every actor's egress policy, so the
// gateway's configuration is built from what actually connects: a set is learned
// the first time an actor using it opens a CONNECT. Register is what makes the
// set's chains exist before the connection they are needed for reaches the
// dispatch listener.
type ExemptionRegistry interface {
	// Register renders set into the dispatch listener and returns once the
	// gateway has acknowledged the configuration containing it. It returns an
	// error if that has not happened before ctx expires, in which case the
	// caller must not claim the set is in effect.
	Register(ctx context.Context, set ExemptionSet) error
}
