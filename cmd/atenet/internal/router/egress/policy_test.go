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
	"net/netip"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func TestParseDestinationHostname(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		want      string
		wantErr   bool
	}{
		{name: "bare name", authority: "example.com", want: "example.com"},
		{name: "name with port", authority: "example.com:8443", want: "example.com"},
		{name: "uppercase is normalized", authority: "API.Example.COM", want: "api.example.com"},
		{name: "the root label is not part of the name", authority: "example.com.", want: "example.com"},
		{name: "root label with a port", authority: "example.com.:443", want: "example.com"},
		{name: "hyphens inside a label", authority: "my-api.example.com", want: "my-api.example.com"},

		// An address is a legitimate destination; it just means no hostname
		// rule can apply, which the empty result says.
		{name: "IPv4 literal", authority: "93.184.216.34"},
		{name: "IPv4 literal with a port", authority: "93.184.216.34:443"},
		{name: "IPv6 literal", authority: "2001:db8::1"},
		{name: "bracketed IPv6 literal", authority: "[2001:db8::1]"},
		{name: "bracketed IPv6 literal with a port", authority: "[2001:db8::1]:443"},

		{name: "empty", authority: "", wantErr: true},
		{name: "port only", authority: ":443", wantErr: true},
		{name: "empty leftmost label", authority: ".example.com", wantErr: true},
		{name: "empty interior label", authority: "foo..example.com", wantErr: true},
		{name: "two trailing dots", authority: "example.com..", wantErr: true},
		// Anything that is not a DNS name is not something a pattern
		// comparison would mean what it appears to on.
		{name: "a URL, not an authority", authority: "https://example.com/x", wantErr: true},
		{name: "leftover port separator", authority: "example.com:80:90", wantErr: true},
		{name: "underscore", authority: "bad_host.example.com", wantErr: true},
		{name: "leading hyphen in a label", authority: "-example.com", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDestinationHostname(tc.authority)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDestinationHostname(%q) = %q, want an error", tc.authority, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDestinationHostname(%q): %v", tc.authority, err)
			}
			if got != tc.want {
				t.Errorf("parseDestinationHostname(%q) = %q, want %q", tc.authority, got, tc.want)
			}
		})
	}
}

func TestParseOriginalDestinationIP(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		want      string
		wantErr   bool
	}{
		{name: "the shape atunnel sends", authority: "93.184.216.34:443", want: "93.184.216.34"},
		{name: "IPv6 with a port", authority: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "no port", authority: "93.184.216.34", want: "93.184.216.34"},
		// An IPv4-mapped address and its IPv4 form are one destination, and
		// only the unmapped form is inside an IPv4 prefix.
		{name: "IPv4-mapped IPv6 is unmapped", authority: "[::ffff:93.184.216.34]:443", want: "93.184.216.34"},

		{name: "absent", authority: "", wantErr: true},
		// Envoy renders an absent value as "-" unless omit_empty_values is set;
		// the config sets it, and this is the belt to that suspenders.
		{name: "Envoy's empty-value placeholder", authority: "-", wantErr: true},
		{name: "a hostname, which atunnel never sends", authority: "example.com:443", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOriginalDestinationIP(tc.authority)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOriginalDestinationIP(%q) = %v, want an error", tc.authority, got)
				}
				if got.IsValid() {
					t.Errorf("parseOriginalDestinationIP(%q) returned both an error and a valid address %v", tc.authority, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOriginalDestinationIP(%q): %v", tc.authority, err)
			}
			if got.String() != tc.want {
				t.Errorf("parseOriginalDestinationIP(%q) = %v, want %s", tc.authority, got, tc.want)
			}
		})
	}
}

func TestHostnameMatches(t *testing.T) {
	tests := []struct {
		pattern, hostname string
		want              bool
	}{
		{pattern: "example.com", hostname: "example.com", want: true},
		{pattern: "example.com", hostname: "api.example.com"},
		{pattern: "example.com", hostname: "notexample.com"},
		{pattern: "example.com", hostname: "example.com.evil.test"},

		// A wildcard stands for exactly one complete, non-empty label.
		{pattern: "*.example.com", hostname: "api.example.com", want: true},
		{pattern: "*.example.com", hostname: "example.com"},
		{pattern: "*.example.com", hostname: "nested.api.example.com"},
		{pattern: "*.example.com", hostname: ".example.com"},
		{pattern: "*.example.com", hostname: "api.example.com.evil.test"},
		// Not the leftmost label, so not a wildcard: "*" is matched literally
		// and the API would never have stored the pattern in the first place.
		{pattern: "api.*.example.com", hostname: "api.foo.example.com"},

		// Patterns are stored lowercase by the API. One that is not matches
		// nothing, which denies.
		{pattern: "Example.com", hostname: "example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+" vs "+tc.hostname, func(t *testing.T) {
			if got := hostnameMatches(tc.pattern, tc.hostname); got != tc.want {
				t.Errorf("hostnameMatches(%q, %q) = %v, want %v", tc.pattern, tc.hostname, got, tc.want)
			}
		})
	}
}

func TestFirstMatchingRule(t *testing.T) {
	hostnames := func(patterns ...string) *ateapipb.EgressRule {
		return &ateapipb.EgressRule{Hostnames: &ateapipb.HostnameRule{Patterns: patterns}}
	}
	ipBlocks := func(cidrs ...string) *ateapipb.EgressRule {
		return &ateapipb.EgressRule{IpBlocks: &ateapipb.IPBlockRule{Cidrs: cidrs}}
	}
	all := func() *ateapipb.EgressRule {
		return &ateapipb.EgressRule{All: &emptypb.Empty{}}
	}
	dst := func(hostname, ip string) destination {
		d := destination{hostname: hostname}
		if ip != "" {
			d.ip = netip.MustParseAddr(ip)
		}
		return d
	}

	tests := []struct {
		name  string
		rules []*ateapipb.EgressRule
		dst   destination
		// want is the index of the rule expected to match, or -1 for a denial.
		want int
	}{
		{name: "no rules denies", rules: nil, dst: dst("example.com", "93.184.216.34"), want: -1},
		{name: "hostname match", rules: []*ateapipb.EgressRule{hostnames("example.com")}, dst: dst("example.com", "93.184.216.34"), want: 0},
		{name: "hostname miss", rules: []*ateapipb.EgressRule{hostnames("example.com")}, dst: dst("evil.test", "93.184.216.34"), want: -1},
		{name: "wildcard match", rules: []*ateapipb.EgressRule{hostnames("*.example.com")}, dst: dst("api.example.com", "93.184.216.34"), want: 0},
		{name: "any pattern in the rule matches", rules: []*ateapipb.EgressRule{hostnames("a.test", "example.com")}, dst: dst("example.com", ""), want: 0},
		{name: "all matches anything", rules: []*ateapipb.EgressRule{all()}, dst: dst("evil.test", "203.0.113.9"), want: 0},

		{name: "ip block match", rules: []*ateapipb.EgressRule{ipBlocks("93.184.216.0/24")}, dst: dst("example.com", "93.184.216.34"), want: 0},
		{name: "ip block miss", rules: []*ateapipb.EgressRule{ipBlocks("10.0.0.0/8")}, dst: dst("example.com", "93.184.216.34"), want: -1},
		{name: "ipv6 block match", rules: []*ateapipb.EgressRule{ipBlocks("2001:db8::/32")}, dst: dst("example.com", "2001:db8::1"), want: 0},
		{name: "an ipv4 address is not in an ipv6 block", rules: []*ateapipb.EgressRule{ipBlocks("2001:db8::/32")}, dst: dst("", "93.184.216.34"), want: -1},

		// Neither rule kind can match on evidence the gateway does not have,
		// which is what makes a missing attribute a denial rather than a pass.
		{name: "a hostname rule cannot match an address destination", rules: []*ateapipb.EgressRule{hostnames("example.com")}, dst: dst("", "93.184.216.34"), want: -1},
		{name: "an ip rule cannot match without an address", rules: []*ateapipb.EgressRule{ipBlocks("93.184.216.0/24")}, dst: dst("example.com", ""), want: -1},

		// The order is the policy: the first match wins even when a later rule
		// would also match, because only its effects are applied.
		{
			name:  "first match wins",
			rules: []*ateapipb.EgressRule{hostnames("other.test"), hostnames("example.com"), all()},
			dst:   dst("example.com", "93.184.216.34"),
			want:  1,
		},

		// A rule this gateway cannot evaluate authorizes nothing.
		{name: "an empty union member denies", rules: []*ateapipb.EgressRule{{}}, dst: dst("example.com", "93.184.216.34"), want: -1},
		{name: "an unparseable cidr is skipped", rules: []*ateapipb.EgressRule{ipBlocks("not-a-cidr", "93.184.216.0/24")}, dst: dst("", "93.184.216.34"), want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := firstMatchingRule(tc.rules, tc.dst)
			if tc.want < 0 {
				if got != nil {
					t.Fatalf("firstMatchingRule matched %v, want no match", got)
				}
				return
			}
			if got != tc.rules[tc.want] {
				t.Errorf("firstMatchingRule matched %v, want rule %d (%v)", got, tc.want, tc.rules[tc.want])
			}
		})
	}
}
