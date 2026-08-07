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
	"sync"
	"testing"
)

func TestPolicyValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  Policy
		wantErr bool
	}{
		{"deny all", Policy{Kind: KindDenyAll}, false},
		{"allow all", Policy{Kind: KindAllowAll}, false},
		{"hostname", Policy{Kind: KindAllowByHostname, Hostnames: []string{"github.com"}}, false},
		{"ip block", Policy{Kind: KindAllowByIPBlock, IPBlocks: mustPrefixes("1.2.3.0/24")}, false},
		{
			"inject",
			Policy{
				Kind:      KindBasicCredentialInject,
				Hostnames: []string{"api.stripe.com"},
				Inject:    map[string][]Injection{"api.stripe.com": {{To: "token", Value: "X"}}},
			},
			false,
		},

		{"unknown kind", Policy{Kind: "NOPE"}, true},
		{"empty kind", Policy{}, true},
		// A parameterless kind carrying parameters means someone expected them to
		// apply. Silently ignoring them is how a policy reads stricter than it is.
		{"deny all with hostnames", Policy{Kind: KindDenyAll, Hostnames: []string{"github.com"}}, true},
		{"allow all with blocks", Policy{Kind: KindAllowAll, IPBlocks: mustPrefixes("1.2.3.0/24")}, true},
		{"hostname with no hostnames", Policy{Kind: KindAllowByHostname}, true},
		{"ip block with no blocks", Policy{Kind: KindAllowByIPBlock}, true},
		{"inject with no hostnames", Policy{Kind: KindBasicCredentialInject}, true},
		{
			"hostname policy that injects",
			Policy{
				Kind:      KindAllowByHostname,
				Hostnames: []string{"github.com"},
				Inject:    map[string][]Injection{"github.com": {{To: "token", Value: "X"}}},
			},
			true,
		},
		// An injection for a host the allowlist does not permit is dead config,
		// and reads as though the credential is in use when it never fires.
		{
			"inject into an unlisted host",
			Policy{
				Kind:      KindBasicCredentialInject,
				Hostnames: []string{"api.stripe.com"},
				Inject:    map[string][]Injection{"evil.example": {{To: "token", Value: "X"}}},
			},
			true,
		},
		{
			"injection with no target",
			Policy{
				Kind:      KindBasicCredentialInject,
				Hostnames: []string{"api.stripe.com"},
				Inject:    map[string][]Injection{"api.stripe.com": {{From: "authorization"}}},
			},
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewSnapshotRejectsBadInput(t *testing.T) {
	if _, err := NewSnapshot(1, map[ActorKey]Policy{
		{Atespace: "acme-prod", Name: "a"}: {Kind: "NOPE"},
	}); err == nil {
		t.Error("NewSnapshot accepted an invalid policy")
	}

	// A key missing its atespace would collide across atespaces, and actor names
	// are only unique within one.
	if _, err := NewSnapshot(1, map[ActorKey]Policy{
		{Name: "a"}: {Kind: KindAllowAll},
	}); err == nil {
		t.Error("NewSnapshot accepted a key with no atespace")
	}
	if _, err := NewSnapshot(1, map[ActorKey]Policy{
		{Atespace: "acme-prod"}: {Kind: KindAllowAll},
	}); err == nil {
		t.Error("NewSnapshot accepted a key with no name")
	}
}

// The snapshot must not alias the caller's map, or a later write to it would
// mutate a published table that handlers are already reading.
func TestNewSnapshotCopiesTheTable(t *testing.T) {
	key := ActorKey{Atespace: "acme-prod", Name: "a"}
	in := map[ActorKey]Policy{key: {Kind: KindDenyAll}}

	snap, err := NewSnapshot(1, in)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	in[key] = Policy{Kind: KindAllowAll}

	if p, _ := snap.Lookup(key); p.Kind != KindDenyAll {
		t.Errorf("snapshot aliased the caller's map: policy became %s", p.Kind)
	}
}

func TestLookupFailsClosed(t *testing.T) {
	snap, err := NewSnapshot(1, map[ActorKey]Policy{
		{Atespace: "acme-prod", Name: "known"}: {Kind: KindAllowAll},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	for _, tc := range []struct {
		name string
		key  ActorKey
	}{
		{"unknown actor", ActorKey{Atespace: "acme-prod", Name: "stranger"}},
		{"right name, wrong atespace", ActorKey{Atespace: "other", Name: "known"}},
		{"no atespace", ActorKey{Name: "known"}},
		{"no name", ActorKey{Atespace: "acme-prod"}},
		{"empty", ActorKey{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, known := snap.Lookup(tc.key)
			if known {
				t.Error("Lookup reported the actor as known")
			}
			if p.Kind != KindDenyAll {
				t.Errorf("Lookup returned %s, want DENY_ALL", p.Kind)
			}
		})
	}

	// A nil snapshot is what an unsynced replica holds. It must deny too.
	if p, known := (*Snapshot)(nil).Lookup(ActorKey{Atespace: "a", Name: "b"}); known || p.Kind != KindDenyAll {
		t.Errorf("nil snapshot returned (%s, %v), want (DENY_ALL, false)", p.Kind, known)
	}
}

// The claim this Store exists to support: a policy change is a pointer swap, so
// it needs no restart and cannot be observed half-applied by a concurrent
// reader. Run with -race.
func TestStoreSwapIsSafeUnderConcurrentReads(t *testing.T) {
	key := ActorKey{Atespace: "acme-prod", Name: "a"}
	deny, err := NewSnapshot(1, map[ActorKey]Policy{key: {Kind: KindDenyAll}})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	allow, err := NewSnapshot(2, map[ActorKey]Policy{key: {Kind: KindAllowAll}})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}

	store := NewStore(deny)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := store.Load()
				p, known := snap.Lookup(key)
				if !known {
					t.Errorf("reader saw an empty table at rev %d", snap.Rev)
					return
				}
				// Every observation must be one of the two whole tables, never a
				// rev/contents mismatch.
				if (snap.Rev == 1) != (p.Kind == KindDenyAll) {
					t.Errorf("torn read: rev %d holds %s", snap.Rev, p.Kind)
					return
				}
			}
		}()
	}

	for i := range 500 {
		if i%2 == 0 {
			store.Swap(allow)
		} else {
			store.Swap(deny)
		}
	}
	close(stop)
	wg.Wait()
}

func TestStoreReadyGatesOnASnapshot(t *testing.T) {
	var st Store
	if st.Ready() {
		t.Error("the zero Store reports ready with no snapshot")
	}
	if st.Load() != nil {
		t.Error("the zero Store returned a snapshot")
	}

	snap, err := NewSnapshot(1, nil)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	st.Swap(snap)
	if !st.Ready() {
		t.Error("Store is not ready after a swap")
	}
}

// The hardcoded table is the PoC's entire policy source, so a mistake in it
// looks like a bug in the engine. Check it names all five kinds and validates.
func TestHardcodedSnapshot(t *testing.T) {
	snap := HardcodedSnapshot()

	seen := map[Kind]bool{}
	for _, name := range []string{"quarantined", "wide-open", "repo-reader", "metrics-shipper", "invoice-agent"} {
		p, known := snap.Lookup(ActorKey{Atespace: DemoAtespace, Name: name})
		if !known {
			t.Errorf("hardcoded table is missing actor %q", name)
			continue
		}
		if err := p.Validate(); err != nil {
			t.Errorf("hardcoded policy for %q is invalid: %v", name, err)
		}
		seen[p.Kind] = true
	}

	for _, kind := range []Kind{
		KindDenyAll, KindAllowAll, KindAllowByHostname, KindAllowByIPBlock, KindBasicCredentialInject,
	} {
		if !seen[kind] {
			t.Errorf("hardcoded table does not exercise %s", kind)
		}
	}
}

// mustPrefixes canonicalizes, so the doc's "1.2.3.4/24" is stored as the network
// it actually denotes rather than printing back as a host route in audit logs.
func TestMustPrefixesMasks(t *testing.T) {
	got := mustPrefixes("1.2.3.4/24")
	if len(got) != 1 || got[0].String() != "1.2.3.0/24" {
		t.Errorf("mustPrefixes(1.2.3.4/24) = %v, want [1.2.3.0/24]", got)
	}
}
