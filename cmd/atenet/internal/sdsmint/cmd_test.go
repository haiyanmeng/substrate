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
	"testing"
	"time"
)

func TestValidateTTL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ttl     time.Duration
		wantErr bool
	}{
		{name: "default", ttl: defaultTTL},
		{name: "deployed value", ttl: 15 * time.Minute},
		{name: "the load-test value", ttl: 5 * time.Minute},
		// The edges of validateTTL's accepted band, spelled out because the
		// bounds are local to it. Both are inclusive.
		{name: "at the band floor", ttl: time.Minute},
		{name: "at the band ceiling", ttl: 24 * time.Hour},

		// The whole point of the exercise: --leaf-cert-ttl=0 used to start a server
		// that logged 0 and issued defaultTTL leaves.
		{name: "zero", ttl: 0, wantErr: true},
		{name: "negative", ttl: -time.Minute, wantErr: true},

		// Short enough that rotation would be floored, so a pushed leaf could
		// already have expired. Rotation is unconditional, so there is no
		// longer a variant of this that is merely a bad idea.
		{name: "below the rotation floor", ttl: time.Second, wantErr: true},

		// Outside the band. These used to start the server with a warning; a
		// TTL that no longer means what it says is now refused outright.
		{name: "short", ttl: 30 * time.Second, wantErr: true},
		{name: "long", ttl: 48 * time.Hour, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := config{LeafCertTTL: tc.ttl}.validateTTL()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateTTL(ttl=%s) = %v; want an error = %v", tc.ttl, err, tc.wantErr)
			}
		})
	}
}

// TestValidateTTLRejectsExactlyTheFlooredRange pins the error boundary to the
// clamp it is protecting rather than to a hard-coded duration, so that moving
// minRotateInterval or rotateFraction cannot leave the check guarding the
// wrong range.
// TestValidateTTLNeverAcceptsAFlooredRotation pins the relationship between the
// accepted band and minRotateInterval. Nothing validateTTL lets through may have
// its rotation interval clamped, or --leaf-cert-ttl would stop setting the
// rotation period and a leaf could expire before its replacement is pushed.
//
// The band floor is a minute and flooring happens below ~1.5s, so this holds by
// a wide margin today. It is checked anyway because the two bounds are set
// independently, in different functions, and nothing else would notice if the
// floor were lowered to where they overlap.
func TestValidateTTLNeverAcceptsAFlooredRotation(t *testing.T) {
	// A millisecond either side of the point where rotateFraction*ttl crosses
	// minRotateInterval. Both are refused now -- the one below by the rotation
	// check, the one above by the band -- so the interesting assertion is that
	// the boundary still lands where rotateInterval says it does.
	boundary := time.Duration(float64(minRotateInterval) / rotateFraction)
	if got := rotateInterval(boundary - time.Millisecond); got != minRotateInterval {
		t.Errorf("rotateInterval(%s) = %s; want it floored at %s", boundary-time.Millisecond, got, minRotateInterval)
	}
	for _, ttl := range []time.Duration{boundary - time.Millisecond, boundary + time.Millisecond} {
		if err := (config{LeafCertTTL: ttl}).validateTTL(); err == nil {
			t.Errorf("validateTTL(ttl=%s) = nil; a TTL near the rotation floor is far below the accepted band", ttl)
		}
	}

	// The smallest TTL that is accepted has to clear the floor on its own.
	const smallestAccepted = time.Minute
	if err := (config{LeafCertTTL: smallestAccepted}).validateTTL(); err != nil {
		t.Fatalf("validateTTL(ttl=%s) = %v; want the band floor accepted", smallestAccepted, err)
	}
	if got := rotateInterval(smallestAccepted); got == minRotateInterval {
		t.Errorf("rotateInterval(%s) = %s, the floor; the shortest accepted TTL must not clamp rotation", smallestAccepted, got)
	}
}

// TestAcceptedTTLsKeepTheRotationWindow checks the ordering the rotation
// design rests on, for every TTL run will accept: cache reuse
// ends, then the ticker fires, then the leaf expires.
func TestAcceptedTTLsKeepTheRotationWindow(t *testing.T) {
	for _, ttl := range []time.Duration{
		time.Minute, // the shortest run accepts
		5 * time.Minute,
		defaultTTL,
		24 * time.Hour, // validateTTL's ceiling
	} {
		if err := (config{LeafCertTTL: ttl}).validateTTL(); err != nil {
			t.Fatalf("validateTTL(ttl=%s) = %v; the case is supposed to be an accepted one", ttl, err)
		}
		reuse := time.Duration(float64(ttl) * reuseFraction)
		rotate := rotateInterval(ttl)
		if rotate <= reuse {
			t.Errorf("ttl %s: rotation at %s is not after cache reuse ends at %s, so a tick would find a fresh entry and hand back the same leaf instead of re-minting", ttl, rotate, reuse)
		}
		if rotate >= ttl {
			t.Errorf("ttl %s: rotation at %s is not before the leaf expires, so the replacement arrives after the leaf it replaces is already dead", ttl, rotate)
		}
	}
}

// TestDefaultTTLIsTheFlagDefault guards the trap this validation was added
// for: a fallback and a flag default that disagree, so the lifetime depends on
// which path you came in through.
func TestDefaultTTLIsTheFlagDefault(t *testing.T) {
	flag := NewSdsmintCmd().Flags().Lookup("leaf-cert-ttl")
	if flag == nil {
		t.Fatal("no --leaf-cert-ttl flag")
	}
	if got, want := flag.DefValue, defaultTTL.String(); got != want {
		t.Errorf("--leaf-cert-ttl default = %q; want %q, the same constant newMinter and newServer fall back to", got, want)
	}
	if err := (config{LeafCertTTL: defaultTTL}).validateTTL(); err != nil {
		t.Errorf("validateTTL(defaultTTL) = %v; the default has to be a value run will start with", err)
	}
}
