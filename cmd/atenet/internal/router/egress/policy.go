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
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// destination is where one request out of an actor's tunnel is headed, as the
// two things an EgressRule can be written against.
//
// Both are needed because neither alone survives the CONNECT/MITM split. The
// CONNECT authority is the address the actor's kernel dialed, so it is the only
// record of the destination IP — the MITM leg never sees it. The hostname is
// the opposite: it exists only inside the tunnel, in the SNI the leaf was
// minted for and the Host header that follows it.
type destination struct {
	// hostname is the request's authority with any port removed, lowercased
	// and with any trailing dot stripped. Empty when the request named an
	// address rather than a name, which no HostnameRule can match.
	hostname string
	// ip is the CONNECT's original destination address. Invalid when the
	// gateway did not carry one across the internal-listener hop, which no
	// IPBlockRule can match.
	ip netip.Addr
}

// String renders the destination for a log line or a denial message.
func (d destination) String() string {
	switch {
	case d.hostname != "" && d.ip.IsValid():
		return fmt.Sprintf("%s (%s)", d.hostname, d.ip)
	case d.hostname != "":
		return d.hostname
	case d.ip.IsValid():
		return d.ip.String()
	default:
		return "unknown"
	}
}

// parseDestinationHostname normalizes the authority of a request inside the
// tunnel into the name an EgressPolicy hostname pattern is matched against.
//
// It returns "" without an error for an authority that names an address rather
// than a hostname: dialing by IP is legitimate, and it simply means no
// HostnameRule can apply.
func parseDestinationHostname(authority string) (string, error) {
	if authority == "" {
		return "", fmt.Errorf("request carries no authority")
	}
	host := authority
	// SplitHostPort rejects a bare hostname and a bare IPv6 literal alike, so
	// its failure is not an error here — it just means there was no port. It
	// also splits at the last colon without looking at what follows, so the
	// port is checked rather than trusted: "https://example.com/x" splits into
	// a host of "https", which is a well-formed name and would authorize
	// against a pattern that has nothing to do with the request.
	if h, p, err := net.SplitHostPort(authority); err == nil && isPort(p) {
		host = h
	}
	// A bracketed IPv6 literal with no port survives the split above.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return "", fmt.Errorf("authority %q names no host", authority)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		// An address, not a name. Hostname rules do not apply.
		return "", nil
	}
	// The root label is not part of the name a pattern is written against.
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !isDNSName(host) {
		return "", fmt.Errorf("authority %q is not a well-formed hostname", authority)
	}
	return host, nil
}

// isPort reports whether p is the port of an authority: a non-empty run of
// digits.
func isPort(p string) bool {
	if p == "" {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	return true
}

// isDNSName reports whether host is a syntactically valid, already-lowercased
// DNS name — the same shape EgressPolicy requires of its patterns.
//
// This is an authorization key, so it is checked rather than assumed. Anything
// rejected here denies the request, which is why the check errs toward strict:
// a name this cannot vouch for is one whose comparison against a pattern would
// not mean what it appears to.
func isDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// parseOriginalDestinationIP reads the address out of the CONNECT authority the
// gateway carried across the internal-listener hop. atunnel takes that
// authority from SO_ORIGINAL_DST and refuses hostnames, so it is always
// "IP:port" — but this runs on data the gateway supplied, not on a promise, so
// anything else yields an invalid address rather than a guess.
func parseOriginalDestinationIP(connectAuthority string) (netip.Addr, error) {
	if connectAuthority == "" {
		return netip.Addr{}, fmt.Errorf("no CONNECT destination was carried across the tunnel")
	}
	host := connectAuthority
	if h, _, err := net.SplitHostPort(connectAuthority); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("CONNECT destination %q is not an address: %w", connectAuthority, err)
	}
	// An IPv4-mapped IPv6 address and its IPv4 form are the same destination,
	// and only the unmapped form is inside an IPv4 prefix.
	return addr.Unmap(), nil
}

// firstMatchingRule returns the rule that authorizes dst, or nil when none
// does. Rules are evaluated in order and the first match wins outright, even
// when a later rule would also match — the order is the policy.
func firstMatchingRule(rules []*ateapipb.EgressRule, dst destination) *ateapipb.EgressRule {
	for _, rule := range rules {
		if ruleMatches(rule, dst) {
			return rule
		}
	}
	return nil
}

func ruleMatches(rule *ateapipb.EgressRule, dst destination) bool {
	switch {
	case rule.GetAll() != nil:
		return true
	case rule.GetHostnames() != nil:
		return hostnameRuleMatches(rule.GetHostnames(), dst.hostname)
	case rule.GetIpBlocks() != nil:
		return ipBlockRuleMatches(rule.GetIpBlocks(), dst.ip)
	default:
		// An EgressRule is a union with exactly one member set. A rule with
		// none is either a policy written against a newer API than this gateway
		// knows or a corrupt record; either way it authorizes nothing.
		return false
	}
}

func hostnameRuleMatches(rule *ateapipb.HostnameRule, hostname string) bool {
	if hostname == "" {
		return false
	}
	for _, pattern := range rule.GetPatterns() {
		if hostnameMatches(pattern, hostname) {
			return true
		}
	}
	return false
}

// hostnameMatches reports whether hostname matches one EgressPolicy pattern: a
// complete name, or a name whose leftmost label is "*" and stands for exactly
// one non-empty label.
//
// The pattern is used as the API stored it. Validation already requires it to
// be a lowercase DNS name, and a pattern that somehow is not simply matches
// nothing, which denies — the safe direction. Normalizing it here would instead
// widen what a malformed pattern authorizes.
func hostnameMatches(pattern, hostname string) bool {
	suffix, wildcard := strings.CutPrefix(pattern, "*.")
	if !wildcard {
		return pattern == hostname
	}
	label, rest, found := strings.Cut(hostname, ".")
	return found && label != "" && rest == suffix
}

func ipBlockRuleMatches(rule *ateapipb.IPBlockRule, ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, cidr := range rule.GetCidrs() {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			// Validation admits only canonical prefixes, so this is a record
			// this gateway cannot evaluate. Skipping it denies where a wrong
			// parse might have allowed.
			continue
		}
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
