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
	"fmt"
	"net/netip"
)

// Mode is the routing decision the CONNECT checkpoint publishes as the
// x-ate-egress-mode header. Envoy's route table branches on it.
type Mode string

const (
	// ModeDeny is never emitted as a header: the CONNECT checkpoint answers a
	// denial with an ImmediateResponse instead of routing to a deny cluster. It
	// exists so the decision type can express the outcome.
	ModeDeny Mode = "deny"
	// ModePassthrough tunnels to the original destination without terminating
	// TLS. Covers ALLOW_ALL and a satisfied ALLOW_BY_IP_BLOCK.
	ModePassthrough Mode = "passthrough"
	// ModeMITM hands the tunnel to the internal listener, which terminates TLS
	// with an on-demand minted certificate and runs the inner checkpoint. Covers
	// ALLOW_BY_HOSTNAME and BASIC_CREDENTIAL_INJECT.
	ModeMITM Mode = "mitm"
)

// ConnectDecision is the outcome at the CONNECT checkpoint.
type ConnectDecision struct {
	Mode Mode
	// Reason is client-visible on a denial and lands in %RESPONSE_CODE_DETAILS%.
	// It names the policy that decided, never the policy's contents: telling a
	// denied actor which CIDRs it missed is a disclosure.
	Reason string
}

// Allowed reports whether the tunnel may proceed in any form.
func (d ConnectDecision) Allowed() bool { return d.Mode != ModeDeny }

// DecideConnect resolves a policy against the CONNECT authority.
//
// dstOK is false when the authority was not an IP literal with a port. That is
// a protocol violation rather than a policy question — atunnel's
// validateDestination already requires an IP literal
// (internal/atunnel/client.go:197-213) — and it fails closed for every policy
// except DENY_ALL, which does not need to parse it.
//
// The three IP-resolvable policies finish here. The two hostname policies cannot
// be decided yet and return ModeMITM, which is an authorization *deferral*, not
// an allow: the inner checkpoint still has to say yes.
func DecideConnect(p Policy, dst netip.AddrPort, dstOK bool) ConnectDecision {
	switch p.Kind {
	case KindDenyAll:
		return ConnectDecision{Mode: ModeDeny, Reason: "policy DENY_ALL"}

	case KindAllowAll:
		return ConnectDecision{Mode: ModePassthrough, Reason: "policy ALLOW_ALL"}

	case KindAllowByIPBlock:
		if !dstOK {
			return ConnectDecision{Mode: ModeDeny, Reason: "CONNECT authority is not an ip:port literal"}
		}
		addr := dst.Addr().Unmap()
		for _, block := range p.IPBlocks {
			if block.Contains(addr) {
				return ConnectDecision{
					Mode:   ModePassthrough,
					Reason: fmt.Sprintf("policy ALLOW_BY_IP_BLOCK matched %s", block),
				}
			}
		}
		return ConnectDecision{Mode: ModeDeny, Reason: "policy ALLOW_BY_IP_BLOCK: destination not in any allowed block"}

	case KindAllowByHostname, KindBasicCredentialInject:
		if !dstOK {
			return ConnectDecision{Mode: ModeDeny, Reason: "CONNECT authority is not an ip:port literal"}
		}
		return ConnectDecision{
			Mode:   ModeMITM,
			Reason: fmt.Sprintf("policy %s: deferred to the inner checkpoint", p.Kind),
		}
	}

	// An unknown kind reaching here means Validate was bypassed. Deny.
	return ConnectDecision{Mode: ModeDeny, Reason: "unknown policy kind"}
}

// InnerDecision is the outcome at the post-MITM checkpoint.
type InnerDecision struct {
	Allow bool
	// Reason is client-visible on a denial, same disclosure rule as above.
	Reason string
	// Injections apply only when Allow is true.
	Injections []Injection
}

// DecideInner resolves a policy against the inner request's hostname.
//
// The three IP-resolvable policies must never reach this function: the CONNECT
// checkpoint routes them to passthrough or denies them outright, so arriving
// here means the route table and the policy table disagree. That is a
// misconfiguration which would otherwise present as traffic silently skipping
// its hostname check, so it denies and says so.
func DecideInner(p Policy, host string) InnerDecision {
	host = NormalizeHostname(host)
	if host == "" {
		// No :authority means nothing to check the allowlist against. Envoy
		// requires one on HTTP/1.1 and HTTP/2, so this is defensive.
		return InnerDecision{Reason: "request has no host"}
	}

	switch p.Kind {
	case KindAllowByHostname:
		if !p.allowsHostname(host) {
			return InnerDecision{Reason: "policy ALLOW_BY_HOSTNAME: host not allowed"}
		}
		return InnerDecision{Allow: true, Reason: "policy ALLOW_BY_HOSTNAME matched"}

	case KindBasicCredentialInject:
		if !p.allowsHostname(host) {
			return InnerDecision{Reason: "policy BASIC_CREDENTIAL_INJECT: host not allowed"}
		}
		return InnerDecision{
			Allow:      true,
			Reason:     "policy BASIC_CREDENTIAL_INJECT matched",
			Injections: p.injectionsFor(host),
		}

	case KindDenyAll, KindAllowAll, KindAllowByIPBlock:
		return InnerDecision{Reason: fmt.Sprintf(
			"policy %s resolves at the CONNECT checkpoint and must not reach MITM; check the route table", p.Kind)}
	}

	return InnerDecision{Reason: "unknown policy kind"}
}
