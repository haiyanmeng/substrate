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

package egressxds

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/genproto/googleapis/rpc/status"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/egress"
)

// testServer builds a registry with a gateway already subscribed, so Register
// only has to wait for the acknowledgement and not for a connection.
func testServer(t *testing.T) *Server {
	t.Helper()
	s := New()
	s.ackTimeout = time.Second
	s.streamOpened()
	return s
}

// ackEverything answers each published version the way a healthy gateway would.
// It stops when the test ends.
func ackEverything(t *testing.T, s *Server) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			version := s.currentVersion()
			s.observeAck(&discoveryv3.DiscoveryRequest{
				TypeUrl:     resource.ListenerType,
				VersionInfo: s.versionString(version),
			})
		}
	}()
}

func TestRegisterPublishesAndWaitsForTheAck(t *testing.T) {
	s := testServer(t)
	ackEverything(t, s)

	set := newSet("api.example.com")
	if err := s.Register(context.Background(), set); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if got := s.registeredIDs(); !slices.Equal(got, []string{set.ID()}) {
		t.Errorf("registered IDs = %v, want %v", got, []string{set.ID()})
	}

	// A second actor on the same set reuses the chains already published.
	before := s.currentVersion()
	if err := s.Register(context.Background(), newSet("api.example.com")); err != nil {
		t.Fatalf("Register() on a known set error = %v", err)
	}
	if got := s.currentVersion(); got != before {
		t.Errorf("registering a known set bumped the version from %d to %d", before, got)
	}
}

// The empty set is the absence of configuration, not a set to configure.
func TestRegisterIgnoresTheEmptySet(t *testing.T) {
	s := New()
	if err := s.Register(context.Background(), newSet()); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if got := s.registeredIDs(); len(got) != 0 {
		t.Errorf("registered IDs = %v, want none", got)
	}
	if got := s.currentVersion(); got != 0 {
		t.Errorf("version = %d, want 0: the empty set must not cause a push", got)
	}
}

// Waiting out the timeout on every CONNECT would be a slow way to learn what a
// closed stream already says.
func TestRegisterFailsFastWithNoGatewaySubscribed(t *testing.T) {
	s := New()
	s.ackTimeout = time.Minute

	start := time.Now()
	err := s.Register(context.Background(), newSet("api.example.com"))
	if err == nil {
		t.Fatal("Register() error = nil, want an error when no gateway is subscribed")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Register() took %v; it should not have waited out the ack timeout", elapsed)
	}
}

// A set the gateway never confirms must not be reported as in effect.
func TestRegisterTimesOutWithoutAnAck(t *testing.T) {
	s := testServer(t)
	s.ackTimeout = 50 * time.Millisecond

	err := s.Register(context.Background(), newSet("api.example.com"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Register() error = %v, want a deadline error", err)
	}
}

// A rejected snapshot is not an acknowledged one.
func TestNackDoesNotCountAsAnAck(t *testing.T) {
	s := testServer(t)
	s.ackTimeout = 50 * time.Millisecond

	go func() {
		time.Sleep(5 * time.Millisecond)
		version := s.versionString(s.currentVersion())
		s.observeAck(&discoveryv3.DiscoveryRequest{
			TypeUrl:     resource.ListenerType,
			VersionInfo: version,
			ErrorDetail: &status.Status{Message: "listener rejected"},
		})
	}()

	if err := s.Register(context.Background(), newSet("api.example.com")); err == nil {
		t.Fatal("Register() error = nil, want an error after a NACK")
	}
}

// A version from the process this one replaced says nothing about what this one
// has published.
func TestAckFromAnotherEpochIsIgnored(t *testing.T) {
	s := testServer(t)
	s.observeAck(&discoveryv3.DiscoveryRequest{
		TypeUrl:     resource.ListenerType,
		VersionInfo: "1700000000-OLDEPOCH-99",
	})
	if got := s.ackedVersion(); got != 0 {
		t.Errorf("acked = %d after a foreign version, want 0", got)
	}
}

// A gateway that reconnects starts from an empty configuration, so nothing it
// acknowledged over the old stream still holds.
func TestReconnectClearsTheAckedVersion(t *testing.T) {
	s := testServer(t)
	ackEverything(t, s)
	if err := s.Register(context.Background(), newSet("api.example.com")); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	s.streamClosed()
	if got := s.ackedVersion(); got != 0 {
		t.Errorf("acked = %d after the stream closed, want 0", got)
	}
}

// Two different pattern sets under one ID would apply one actor's exemptions to
// another's traffic.
func TestRegisterRejectsAnIDCollision(t *testing.T) {
	s := testServer(t)
	ackEverything(t, s)

	first := newSet("api.example.com")
	if err := s.Register(context.Background(), first); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	// Forge the collision: the digest makes a natural one unreachable in a test.
	s.mu.Lock()
	s.sets[first.ID()].set = newSet("evil.example.com")
	s.mu.Unlock()

	if err := s.Register(context.Background(), first); err == nil {
		t.Fatal("Register() error = nil, want a collision error")
	}
}

// The rendered listener has to stay bounded even if every actor exempts
// something different.
func TestRegisterEvictsTheLeastRecentlyUsedSet(t *testing.T) {
	s := testServer(t)
	ackEverything(t, s)
	s.maxSets = 2

	clock := time.Now()
	s.now = func() time.Time { clock = clock.Add(time.Minute); return clock }

	first, second, third := newSet("a.example.com"), newSet("b.example.com"), newSet("c.example.com")
	for _, set := range []egress.ExemptionSet{first, second} {
		if err := s.Register(context.Background(), set); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	// Touch the first so the second becomes the oldest.
	if err := s.Register(context.Background(), first); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := s.Register(context.Background(), third); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got := s.registeredIDs()
	want := slices.Sorted(slices.Values([]string{first.ID(), third.ID()}))
	if !slices.Equal(got, want) {
		t.Errorf("registered IDs = %v, want %v", got, want)
	}
}

// A policy that stops being used stops being rendered.
func TestSweepDropsIdleSets(t *testing.T) {
	s := testServer(t)
	ackEverything(t, s)
	s.idleTimeout = time.Hour

	clock := time.Now()
	s.now = func() time.Time { return clock }

	stale, fresh := newSet("stale.example.com"), newSet("fresh.example.com")
	if err := s.Register(context.Background(), stale); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	clock = clock.Add(2 * time.Hour)
	if err := s.Register(context.Background(), fresh); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	s.sweepOnce()
	if got := s.registeredIDs(); !slices.Equal(got, []string{fresh.ID()}) {
		t.Errorf("registered IDs = %v, want %v", got, []string{fresh.ID()})
	}
}
