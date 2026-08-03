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

// This file is the seam. Everything else in the package treats policy as
// something a Store hands it; only this file knows where the table came from.
//
// Replacing it with a real source means writing a refresher that builds a
// *Snapshot from ate-api and calls Store.Swap on a timer. Nothing above needs to
// change. See "Policy distribution and cache invalidation" in egress-authn.md
// for why that is a poll rather than a watch today, and why the refresher must
// serve the last good snapshot on failure instead of clearing it.

// Demo atespace used by every hardcoded actor.
const DemoAtespace = "acme-prod"

// HardcodedSnapshot returns a table exercising all five policies, one actor
// each. It panics on a malformed entry: this is a compile-time-ish constant, so
// a mistake here is a bug in the PoC rather than bad input.
func HardcodedSnapshot() *Snapshot {
	s, err := NewSnapshot(1, map[ActorKey]Policy{
		// No egress at all. Denied at the CONNECT, before any TLS work.
		{Atespace: DemoAtespace, Name: "quarantined"}: {
			Kind: KindDenyAll,
		},

		// Unrestricted. Tunneled straight through; the gateway never sees
		// plaintext and never mints a certificate.
		{Atespace: DemoAtespace, Name: "wide-open"}: {
			Kind: KindAllowAll,
		},

		// A fixed hostname allowlist. Needs MITM, because the CONNECT authority
		// is an IP literal (internal/atunnel/client.go:197-213 rejects anything
		// else) and the actor resolves DNS itself.
		{Atespace: DemoAtespace, Name: "repo-reader"}: {
			Kind:      KindAllowByHostname,
			Hostnames: []string{"github.com", "microsoft.com", "my-app.my-company.com"},
		},

		// A fixed CIDR allowlist. Decided entirely from the CONNECT authority,
		// so this costs one map lookup and no TLS termination.
		{Atespace: DemoAtespace, Name: "metrics-shipper"}: {
			Kind:     KindAllowByIPBlock,
			IPBlocks: mustPrefixes("1.2.3.4/24", "2600:2d00::/32", "127.0.0.0/8"),
		},

		// Hostname allowlist plus credential rewriting. The actor sends whatever
		// Authorization header it likes; the gateway replaces it with a token the
		// actor never held. This is the only policy that requires the gateway to
		// hold third-party secrets, which is the argument in egress-authn.md for
		// keeping this process separate from sdsmintd.
		{Atespace: DemoAtespace, Name: "invoice-agent"}: {
			Kind:      KindBasicCredentialInject,
			Hostnames: []string{"api.stripe.com", "github.com"},
			Inject: map[string][]Injection{
				"api.stripe.com": {
					{From: "authorization", To: "token", Value: "X"},
				},
				// Same destination as repo-reader's, but reached with a
				// credential attached rather than bare.
				"github.com": {
					{From: "authorization", To: "authorization", Value: "Bearer gho_poc_not_a_real_token"},
				},
			},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("hardcoded policy table is invalid: %v", err))
	}
	return s
}

// mustPrefixes parses CIDRs and canonicalizes them with Masked.
//
// Masked matters: the design doc's own example is "1.2.3.4/24", which netip
// stores verbatim. Prefix.Contains only compares the leading Bits so matching
// is correct either way, but an uncanonicalized prefix prints back as
// "1.2.3.4/24" in logs and audit output, which reads like a host route.
func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			panic(fmt.Sprintf("bad CIDR %q in hardcoded policy: %v", c, err))
		}
		out = append(out, p.Masked())
	}
	return out
}
