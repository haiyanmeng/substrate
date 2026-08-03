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
	"net/netip"
	"testing"
)

func mustAddrPort(t *testing.T, s string) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		t.Fatalf("ParseAddrPort(%q) = %v", s, err)
	}
	return ap
}

func TestDecideConnect(t *testing.T) {
	ipBlock := Policy{Kind: KindAllowByIPBlock, IPBlocks: mustPrefixes("1.2.3.4/24", "2600:2d00::/32")}
	hostname := Policy{Kind: KindAllowByHostname, Hostnames: []string{"github.com"}}
	inject := Policy{
		Kind:      KindBasicCredentialInject,
		Hostnames: []string{"api.stripe.com"},
		Inject:    map[string][]Injection{"api.stripe.com": {{From: "authorization", To: "token", Value: "X"}}},
	}

	for _, tc := range []struct {
		name   string
		policy Policy
		dst    string
		want   Mode
	}{
		{"deny all", Policy{Kind: KindDenyAll}, "1.2.3.9:443", ModeDeny},
		{"allow all", Policy{Kind: KindAllowAll}, "9.9.9.9:443", ModePassthrough},

		// The doc's own example CIDR is "1.2.3.4/24", which is not a canonical
		// prefix. It must still match the whole /24, including .0 and .255.
		{"ip block matches host bits set", ipBlock, "1.2.3.9:443", ModePassthrough},
		{"ip block matches network address", ipBlock, "1.2.3.0:443", ModePassthrough},
		{"ip block matches broadcast address", ipBlock, "1.2.3.255:443", ModePassthrough},
		{"ip block rejects neighbour /24", ipBlock, "1.2.4.1:443", ModeDeny},
		{"ip block matches v6", ipBlock, "[2600:2d00::1]:443", ModePassthrough},
		{"ip block rejects other v6", ipBlock, "[2601::1]:443", ModeDeny},

		// A v4-mapped v6 literal is the same host as its v4 form. Without Unmap
		// it would miss every v4 prefix and be denied, which is a fail-closed bug
		// rather than a hole, but still wrong.
		{"ip block matches v4-mapped v6", ipBlock, "[::ffff:1.2.3.9]:443", ModePassthrough},

		{"hostname defers to mitm", hostname, "140.82.121.4:443", ModeMITM},
		{"inject defers to mitm", inject, "1.1.1.1:443", ModeMITM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideConnect(tc.policy, mustAddrPort(t, tc.dst), true)
			if got.Mode != tc.want {
				t.Errorf("DecideConnect(%s, %s) mode = %q, want %q (reason: %s)",
					tc.policy.Kind, tc.dst, got.Mode, tc.want, got.Reason)
			}
			if got.Reason == "" {
				t.Error("decision carries no reason")
			}
		})
	}
}

// A CONNECT authority that is not an ip:port literal must fail closed for every
// policy that would otherwise need to inspect it. Resolving it instead would
// hand the actor a DNS-rebinding primitive: pass the check with one answer, get
// dialled with another.
func TestDecideConnectUnparseableDestinationFailsClosed(t *testing.T) {
	for _, p := range []Policy{
		{Kind: KindAllowByIPBlock, IPBlocks: mustPrefixes("1.2.3.0/24")},
		{Kind: KindAllowByHostname, Hostnames: []string{"github.com"}},
		{Kind: KindBasicCredentialInject, Hostnames: []string{"github.com"}},
	} {
		t.Run(string(p.Kind), func(t *testing.T) {
			got := DecideConnect(p, netip.AddrPort{}, false)
			if got.Allowed() {
				t.Errorf("DecideConnect with an unparseable destination = %q, want deny", got.Mode)
			}
		})
	}

	// ALLOW_ALL genuinely does not care what the destination is, and DENY_ALL
	// never reads it. Neither should change behaviour.
	if got := DecideConnect(Policy{Kind: KindAllowAll}, netip.AddrPort{}, false); got.Mode != ModePassthrough {
		t.Errorf("ALLOW_ALL with an unparseable destination = %q, want passthrough", got.Mode)
	}
	if got := DecideConnect(Policy{Kind: KindDenyAll}, netip.AddrPort{}, false); got.Allowed() {
		t.Error("DENY_ALL allowed something")
	}
}

func TestDecideConnectUnknownKindDenies(t *testing.T) {
	got := DecideConnect(Policy{Kind: "ALLOW_EVERYTHING_PLEASE"}, mustAddrPort(t, "1.1.1.1:443"), true)
	if got.Allowed() {
		t.Errorf("unknown kind = %q, want deny", got.Mode)
	}
}

func TestDecideInnerHostname(t *testing.T) {
	p := Policy{Kind: KindAllowByHostname, Hostnames: []string{"github.com", "my-app.my-company.com"}}

	for _, tc := range []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"my-app.my-company.com", true},
		// Spelling must not be a bypass.
		{"GitHub.com", true},
		{"github.com.", true},
		{"github.com:443", true},
		// Neighbours that a sloppy prefix or suffix match would let through.
		{"evil-github.com", false},
		{"github.com.evil.example", false},
		{"sub.github.com", false},
		{"microsoft.com", false},
		{"", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got := DecideInner(p, tc.host)
			if got.Allow != tc.want {
				t.Errorf("DecideInner(ALLOW_BY_HOSTNAME, %q) = %v, want %v (reason: %s)",
					tc.host, got.Allow, tc.want, got.Reason)
			}
			if len(got.Injections) != 0 {
				t.Error("ALLOW_BY_HOSTNAME produced injections")
			}
		})
	}
}

func TestDecideInnerCredentialInject(t *testing.T) {
	p := Policy{
		Kind:      KindBasicCredentialInject,
		Hostnames: []string{"api.stripe.com", "github.com"},
		Inject: map[string][]Injection{
			"api.stripe.com": {{From: "authorization", To: "token", Value: "X"}},
		},
	}

	got := DecideInner(p, "api.stripe.com")
	if !got.Allow {
		t.Fatalf("api.stripe.com denied: %s", got.Reason)
	}
	if len(got.Injections) != 1 || got.Injections[0].To != "token" || got.Injections[0].Value != "X" {
		t.Errorf("injections = %+v, want one token:X", got.Injections)
	}

	// An allowlisted host with no injection entry is reachable, just bare. That
	// distinction is the reason Hostnames and Inject are separate fields.
	got = DecideInner(p, "github.com")
	if !got.Allow {
		t.Fatalf("github.com denied: %s", got.Reason)
	}
	if len(got.Injections) != 0 {
		t.Errorf("github.com got injections %+v, want none", got.Injections)
	}

	if got := DecideInner(p, "evil.example"); got.Allow {
		t.Error("BASIC_CREDENTIAL_INJECT allowed a host outside its allowlist")
	}
}

// The three IP-answerable policies are decided at the CONNECT checkpoint and
// routed to passthrough. Reaching the inner checkpoint means the route table and
// the policy table disagree, which would otherwise look like traffic quietly
// skipping its hostname check.
func TestDecideInnerRejectsConnectResolvablePolicies(t *testing.T) {
	for _, p := range []Policy{
		{Kind: KindDenyAll},
		{Kind: KindAllowAll},
		{Kind: KindAllowByIPBlock, IPBlocks: mustPrefixes("1.2.3.0/24")},
	} {
		t.Run(string(p.Kind), func(t *testing.T) {
			got := DecideInner(p, "github.com")
			if got.Allow {
				t.Errorf("%s allowed at the inner checkpoint, want deny", p.Kind)
			}
		})
	}
}

func TestNeedsMITM(t *testing.T) {
	for kind, want := range map[Kind]bool{
		KindDenyAll:               false,
		KindAllowAll:              false,
		KindAllowByIPBlock:        false,
		KindAllowByHostname:       true,
		KindBasicCredentialInject: true,
	} {
		if got := (Policy{Kind: kind}).NeedsMITM(); got != want {
			t.Errorf("%s.NeedsMITM() = %v, want %v", kind, got, want)
		}
	}
}

func TestNormalizeHostname(t *testing.T) {
	for in, want := range map[string]string{
		"GitHub.com":              "github.com",
		"github.com.":             "github.com",
		"github.com:443":          "github.com",
		"  github.com  ":          "github.com",
		"[2600:2d00::1]:443":      "2600:2d00::1",
		"[2600:2d00::1]":          "2600:2d00::1",
		"2600:2d00::1":            "2600:2d00::1",
		"my-app.my-company.com.:": "my-app.my-company.com",
		"":                        "",
	} {
		if got := NormalizeHostname(in); got != want {
			t.Errorf("NormalizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}
