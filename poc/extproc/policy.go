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

// Package extproc implements the Envoy external-processing gRPC server that
// authorizes actor egress at the egress gateway. See egress-authn.md for the
// design this prototypes.
//
// The server answers at two checkpoints, because the two see different halves
// of the decision tuple:
//
//   - CheckpointConnect runs on the actor's CONNECT to the gateway. It sees the
//     destination IP:port and the X-Ate-* metadata, but no hostname.
//   - CheckpointInner runs on the tunneled HTTP request after MITM. It sees the
//     hostname and the request headers, but carries no actor identity of its
//     own.
//
// Policies are hardcoded here (see hardcoded.go) so the PoC has no control-plane
// dependency. Everything above the Store interface is written as if they were
// not.
package extproc

import (
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
)

// Kind enumerates the egress policies the gateway supports.
type Kind string

const (
	// KindDenyAll denies every destination. Resolves at the CONNECT checkpoint.
	KindDenyAll Kind = "DENY_ALL"
	// KindAllowAll permits every destination. Resolves at the CONNECT checkpoint.
	KindAllowAll Kind = "ALLOW_ALL"
	// KindAllowByHostname permits a fixed set of hostnames. Requires MITM,
	// because the CONNECT authority is an IP literal and carries no hostname.
	KindAllowByHostname Kind = "ALLOW_BY_HOSTNAME"
	// KindAllowByIPBlock permits a fixed set of CIDRs. Resolves at the CONNECT
	// checkpoint, since the CONNECT authority is exactly the destination IP.
	KindAllowByIPBlock Kind = "ALLOW_BY_IP_BLOCK"
	// KindBasicCredentialInject permits a fixed set of hostnames and rewrites
	// credential headers on the way out. Requires MITM: header rewriting is only
	// possible once the inner request is in the clear.
	KindBasicCredentialInject Kind = "BASIC_CREDENTIAL_INJECT"
)

// ActorKey identifies an actor. The atespace is part of the key because an
// actor name is only unique within its atespace; see the actor DNS name form at
// internal/resources/actor.go:22.
type ActorKey struct {
	Atespace string
	Name     string
}

func (k ActorKey) String() string { return k.Atespace + "/" + k.Name }

// Zero reports whether the key is missing either half. A half-populated key is
// never a valid lookup: it would collide across atespaces.
func (k ActorKey) Zero() bool { return k.Atespace == "" || k.Name == "" }

// Injection rewrites one request header into another on the outbound request.
// It models BASIC_CREDENTIAL_INJECT: drop whatever the actor sent under From
// and set To to a credential the actor never held.
//
// From may be empty, meaning "add To without removing anything". From and To
// may be the same header, which is the usual case for Authorization.
type Injection struct {
	From  string
	To    string
	Value string
}

// Policy is one actor's egress authorization.
//
// Which fields are meaningful depends on Kind:
//
//   - DENY_ALL, ALLOW_ALL — none.
//   - ALLOW_BY_HOSTNAME — Hostnames.
//   - ALLOW_BY_IP_BLOCK — IPBlocks.
//   - BASIC_CREDENTIAL_INJECT — Hostnames is the allowlist; Inject maps a subset
//     of those hostnames to the rewrites applied when one is reached. A hostname
//     in Inject but not in Hostnames is unreachable, and Validate rejects it.
type Policy struct {
	Kind      Kind
	Hostnames []string
	IPBlocks  []netip.Prefix
	Inject    map[string][]Injection
}

// NeedsMITM reports whether this policy can only be decided on the inner
// request. The CONNECT checkpoint uses this to pick between the passthrough and
// mitm routes; only these two kinds pay the cost of terminating TLS.
func (p Policy) NeedsMITM() bool {
	return p.Kind == KindAllowByHostname || p.Kind == KindBasicCredentialInject
}

// Validate rejects policies whose parameters do not match their kind. It exists
// so a malformed table fails at startup rather than by silently denying, which
// is the failure mode a fail-closed gate would otherwise hide.
func (p Policy) Validate() error {
	switch p.Kind {
	case KindDenyAll, KindAllowAll:
		if len(p.Hostnames) > 0 || len(p.IPBlocks) > 0 || len(p.Inject) > 0 {
			return fmt.Errorf("%s takes no parameters", p.Kind)
		}
	case KindAllowByHostname:
		if len(p.Hostnames) == 0 {
			return fmt.Errorf("%s requires at least one hostname", p.Kind)
		}
		if len(p.Inject) > 0 {
			return fmt.Errorf("%s does not inject; use %s", p.Kind, KindBasicCredentialInject)
		}
	case KindAllowByIPBlock:
		if len(p.IPBlocks) == 0 {
			return fmt.Errorf("%s requires at least one CIDR", p.Kind)
		}
	case KindBasicCredentialInject:
		if len(p.Hostnames) == 0 {
			return fmt.Errorf("%s requires at least one hostname", p.Kind)
		}
		for host := range p.Inject {
			if !p.allowsHostname(host) {
				return fmt.Errorf("%s injects into %q, which is not in the allowlist", p.Kind, host)
			}
		}
	default:
		return fmt.Errorf("unknown policy kind %q", p.Kind)
	}
	for _, in := range p.Inject {
		for _, one := range in {
			if one.To == "" {
				return fmt.Errorf("injection with no target header")
			}
		}
	}
	return nil
}

// allowsHostname matches exactly, case-insensitively, ignoring a trailing dot.
// There is deliberately no wildcard support: a wildcard allowlist is a policy
// language decision, and getting it subtly wrong is how allowlists leak.
func (p Policy) allowsHostname(host string) bool {
	host = NormalizeHostname(host)
	if host == "" {
		return false
	}
	for _, h := range p.Hostnames {
		if NormalizeHostname(h) == host {
			return true
		}
	}
	return false
}

// injectionsFor returns the rewrites to apply for an allowed hostname.
func (p Policy) injectionsFor(host string) []Injection {
	host = NormalizeHostname(host)
	for h, in := range p.Inject {
		if NormalizeHostname(h) == host {
			return in
		}
	}
	return nil
}

// NormalizeHostname lowercases, strips a trailing dot and strips any port. Both
// "GitHub.com." and "github.com:443" must compare equal to "github.com", or the
// allowlist is bypassable by spelling.
func NormalizeHostname(host string) string {
	host = strings.TrimSpace(host)
	// Strip the port, but leave a bare IPv6 literal alone: only the bracketed
	// form is ambiguous with a port, and only that form can be split on the
	// last colon safely.
	if i := strings.LastIndex(host, "]"); i >= 0 {
		host = strings.TrimPrefix(host[:i], "[")
	} else if strings.Count(host, ":") == 1 {
		host = host[:strings.Index(host, ":")]
	}
	host = strings.TrimSuffix(host, ".")
	return strings.ToLower(host)
}

// DenyAll is the policy served for any actor the snapshot does not name. A
// gateway that cannot identify the caller must not forward its traffic.
var DenyAll = Policy{Kind: KindDenyAll}

// Snapshot is an immutable policy table. Handlers hold one for the duration of
// a decision, so a concurrent update can never be observed half-applied.
type Snapshot struct {
	// Rev identifies this table in logs and in /stats. It has no ordering
	// meaning beyond "different revs are different tables".
	Rev      int
	policies map[ActorKey]Policy
}

// NewSnapshot validates every policy and copies the map, so the caller cannot
// mutate the table after publishing it.
func NewSnapshot(rev int, policies map[ActorKey]Policy) (*Snapshot, error) {
	cp := make(map[ActorKey]Policy, len(policies))
	for k, p := range policies {
		if k.Zero() {
			return nil, fmt.Errorf("policy key %q is missing an atespace or a name", k)
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("policy for %s: %w", k, err)
		}
		cp[k] = p
	}
	return &Snapshot{Rev: rev, policies: cp}, nil
}

// Lookup returns the actor's policy, or DenyAll if it has none. The bool
// distinguishes "explicitly denied" from "unknown actor" for logging; both deny.
func (s *Snapshot) Lookup(k ActorKey) (Policy, bool) {
	if s == nil || k.Zero() {
		return DenyAll, false
	}
	p, ok := s.policies[k]
	if !ok {
		return DenyAll, false
	}
	return p, true
}

// Len reports how many actors the table names.
func (s *Snapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.policies)
}

// Store holds the live Snapshot behind an atomic pointer.
//
// This is the shape egress-authn.md argues for: policy is data, not
// configuration, so a policy change must never restart the process. Envoy opens
// one ext_proc stream per HTTP stream, and with failure_mode_allow: false a
// restart converts every in-flight stream into a denial. Readers here are
// wait-free and an update is invisible to requests already in flight.
//
// The zero Store holds no snapshot and denies everything, which is what a
// replica that has not yet synced must do.
type Store struct {
	cur atomic.Pointer[Snapshot]
}

// NewStore publishes an initial snapshot.
func NewStore(s *Snapshot) *Store {
	st := &Store{}
	st.Swap(s)
	return st
}

// Load returns the current snapshot, or nil if none has been published.
func (st *Store) Load() *Snapshot { return st.cur.Load() }

// Swap publishes a new snapshot. In production this is what the background
// refresher calls after a successful poll of ate-api; a failed poll must leave
// the last good snapshot in place rather than clearing it.
func (st *Store) Swap(s *Snapshot) { st.cur.Store(s) }

// Ready reports whether a snapshot has been published. Gate the process's
// readiness probe on this: an unsynced replica denies everything, so Envoy must
// not route to it.
func (st *Store) Ready() bool { return st.cur.Load() != nil }
