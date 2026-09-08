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

// Package egressxds serves the egress gateway's TLS-interception dispatch
// listener over xDS.
//
// The gateway is otherwise statically configured, and stays that way: this
// package owns exactly one listener, whose only variable part is the set of
// destinations each actor has exempted from interception. That listener has to
// be dynamic because Envoy cannot match a runtime value against another runtime
// value — the exemption list cannot travel on the connection and be searched
// there. It has to be compiled into configuration, and configuration that
// depends on actor policy cannot be baked into a bootstrap.
//
// Because the control plane cannot enumerate every actor's egress policy, the
// listener is built from demand rather than from a full inventory: a set is
// added the first time an actor using it opens a CONNECT, and dropped again
// once nothing has used it for a while. This mirrors how the same gateway mints
// leaf certificates on demand, and keeps the rendered config proportional to
// the exemptions actually in use.
package egressxds

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	listenerservicev3 "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/egress"
)

// NodeID is the node the egress gateway's bootstrap identifies itself with. It
// must match the `node.id` in manifests/ate-install/atenet-egress*.yaml, or the
// gateway subscribes to a snapshot that is never written.
const NodeID = "atenet-egress"

// Server is the registry of live exemption sets and the xDS server that
// publishes them. It implements egress.ExemptionRegistry.
type Server struct {
	cache cachev3.SnapshotCache
	srv   serverv3.Server

	// versionEpoch distinguishes this process's snapshot versions from those of
	// the process it replaced. Without it a gateway that reconnects after a
	// restart, still holding the old process's version, would look like it had
	// already acknowledged configuration this one has not sent.
	versionEpoch string

	// ackTimeout bounds how long a CONNECT waits for the gateway to acknowledge
	// a newly added set. It has to stay well inside the ext_proc message
	// timeout: overrunning that fails the CONNECT outright, where overrunning
	// this only costs the connection its exemption.
	ackTimeout time.Duration
	// idleTimeout is how long an unused set stays configured. Long enough that
	// an actor's periodic traffic keeps its set alive, short enough that a
	// deleted policy stops being rendered the same day.
	idleTimeout time.Duration
	// maxSets caps the rendered listener. Reaching it means evicting the
	// least-recently-used set, which costs that actor its exemptions rather
	// than growing config without bound.
	maxSets int
	now     func() time.Time

	mu sync.Mutex
	// sets is the live registry, keyed by exemption set ID.
	sets map[string]*registeredSet
	// version is the count half of the current snapshot version.
	version int64
	// acked is the highest version this process has seen the gateway
	// acknowledge, and streams is how many ADS streams are open. A set is only
	// in effect once acked has reached the version that introduced it.
	acked   int64
	streams int
	// ackedCh is closed and replaced whenever acked advances, so that waiters
	// can block without polling.
	ackedCh chan struct{}
}

type registeredSet struct {
	set egress.ExemptionSet
	// version is the snapshot version that first contained this set.
	version  int64
	lastUsed time.Time
}

// New builds the dispatch listener's xDS server with an empty registry.
func New() *Server {
	const (
		defaultAckTimeout  = 2 * time.Second
		defaultIdleTimeout = 30 * time.Minute
		defaultMaxSets     = 512
	)

	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, nil)
	s := &Server{
		cache:        cache,
		versionEpoch: strconv.FormatInt(time.Now().Unix(), 10) + "-" + rand.Text()[:8],
		ackTimeout:   defaultAckTimeout,
		idleTimeout:  defaultIdleTimeout,
		maxSets:      defaultMaxSets,
		now:          time.Now,
		sets:         map[string]*registeredSet{},
		ackedCh:      make(chan struct{}),
	}
	s.srv = serverv3.NewServer(context.Background(), cache, serverv3.CallbackFuncs{
		StreamOpenFunc:    func(context.Context, int64, string) error { s.streamOpened(); return nil },
		StreamClosedFunc:  func(int64, *corev3.Node) { s.streamClosed() },
		StreamRequestFunc: func(_ int64, req *discoveryv3.DiscoveryRequest) error { s.observeAck(req); return nil },
	})
	return s
}

// Register makes set's chains part of the dispatch listener and waits for the
// gateway to acknowledge them. It returns an error if that has not happened
// before the caller's context or the ack timeout expires; the caller must then
// treat the set as not in effect.
func (s *Server) Register(ctx context.Context, set egress.ExemptionSet) error {
	if set.IsEmpty() {
		// Nothing to configure: the empty set is what an absent filter state
		// already means.
		return nil
	}

	want, published, err := s.ensure(set)
	if err != nil {
		return err
	}
	if !published {
		// Already acknowledged on an earlier connection; nothing to wait for.
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.ackTimeout)
	defer cancel()
	return s.awaitAck(ctx, want)
}

// ensure records the set, publishing a new snapshot if it is new. It returns
// the version the set has to be acknowledged at, and whether waiting for that
// acknowledgement is still necessary.
func (s *Server) ensure(set egress.ExemptionSet) (version int64, mustWait bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.sets[set.ID()]; ok {
		// Two different sets under one ID would silently apply one actor's
		// exemptions to another's traffic. Refuse rather than pick.
		if !slices.Equal(existing.set.Patterns(), set.Patterns()) {
			return 0, false, fmt.Errorf("exemption set ID %s is already registered for a different set of patterns", set.ID())
		}
		existing.lastUsed = s.now()
		return existing.version, s.acked < existing.version, nil
	}

	if len(s.sets) >= s.maxSets {
		s.evictOldestLocked()
	}
	entry := &registeredSet{set: set, lastUsed: s.now()}
	s.sets[set.ID()] = entry

	version, err = s.publishLocked()
	if err != nil {
		// Leaving the set registered after a failed publish would make the next
		// connection think it is configured when it is not.
		delete(s.sets, set.ID())
		return 0, false, err
	}
	entry.version = version
	return version, true, nil
}

// evictOldestLocked drops the least-recently-used set to make room.
func (s *Server) evictOldestLocked() {
	var oldestID string
	var oldest time.Time
	for id, entry := range s.sets {
		if oldestID == "" || entry.lastUsed.Before(oldest) {
			oldestID, oldest = id, entry.lastUsed
		}
	}
	if oldestID == "" {
		return
	}
	slog.Warn("egress exemption set registry is full; evicting the least recently used set",
		slog.String("exemptionSet", oldestID),
		slog.Int("maxSets", s.maxSets))
	delete(s.sets, oldestID)
}

// publishLocked renders the current registry into a new snapshot and returns
// its version.
func (s *Server) publishLocked() (int64, error) {
	current := make(map[string]egress.ExemptionSet, len(s.sets))
	for id, entry := range s.sets {
		current[id] = entry.set
	}
	listener, err := buildListener(sortedSets(current))
	if err != nil {
		return 0, fmt.Errorf("building the egress dispatch listener: %w", err)
	}

	s.version++
	version := s.version
	snapshot, err := cachev3.NewSnapshot(s.versionString(version), map[string][]types.Resource{
		resource.ListenerType: {listener},
	})
	if err != nil {
		s.version--
		return 0, fmt.Errorf("building the egress dispatch snapshot: %w", err)
	}
	if err := s.cache.SetSnapshot(context.Background(), NodeID, snapshot); err != nil {
		s.version--
		return 0, fmt.Errorf("publishing the egress dispatch snapshot: %w", err)
	}
	slog.Info("published egress dispatch listener",
		slog.String("version", s.versionString(version)),
		slog.Int("exemptionSets", len(s.sets)))
	return version, nil
}

func (s *Server) versionString(version int64) string {
	return s.versionEpoch + "-" + strconv.FormatInt(version, 10)
}

// awaitAck blocks until the gateway has acknowledged at least version.
func (s *Server) awaitAck(ctx context.Context, version int64) error {
	for {
		s.mu.Lock()
		acked, streams, ch := s.acked, s.streams, s.ackedCh
		s.mu.Unlock()

		if acked >= version {
			return nil
		}
		if streams == 0 {
			// No gateway is subscribed, so no acknowledgement is coming. Say so
			// now instead of spending the whole timeout on every connection.
			return fmt.Errorf("no egress gateway is subscribed to the dispatch listener")
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("egress gateway did not acknowledge dispatch listener version %d: %w", version, ctx.Err())
		}
	}
}

// observeAck advances the acknowledged version from a discovery request.
//
// An xDS request's version_info is the version the client is confirming, so a
// request naming ours is the gateway telling us it has the listener loaded. A
// request carrying an error detail is a rejection of that version and must not
// count.
func (s *Server) observeAck(req *discoveryv3.DiscoveryRequest) {
	if req.GetTypeUrl() != resource.ListenerType {
		return
	}
	if detail := req.GetErrorDetail(); detail != nil {
		slog.Error("egress gateway rejected the dispatch listener",
			slog.String("version", req.GetVersionInfo()),
			slog.String("err", detail.GetMessage()))
		return
	}
	version, ok := s.parseVersion(req.GetVersionInfo())
	if !ok {
		// Either the initial empty version, or one left over from the process
		// this one replaced.
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if version <= s.acked {
		return
	}
	s.acked = version
	close(s.ackedCh)
	s.ackedCh = make(chan struct{})
}

// parseVersion recovers the count from a version string this process minted.
func (s *Server) parseVersion(version string) (int64, bool) {
	count, found := strings.CutPrefix(version, s.versionEpoch+"-")
	if !found {
		return 0, false
	}
	parsed, err := strconv.ParseInt(count, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func (s *Server) streamOpened() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams++
}

func (s *Server) streamClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams--
	// A gateway that reconnects starts from nothing, so anything it
	// acknowledged over the closed stream no longer holds.
	s.acked = 0
}

// Serve publishes the initial listener and runs the xDS server until ctx is
// cancelled, sweeping idle exemption sets as it goes.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	// Publish before accepting: a gateway that connects to a node with no
	// snapshot gets no response at all, and warns about a listener it was told
	// to fetch and never received.
	s.mu.Lock()
	_, err := s.publishLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}

	go s.sweep(ctx)

	grpcServer := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	discoveryv3.RegisterAggregatedDiscoveryServiceServer(grpcServer, s.srv)
	listenerservicev3.RegisterListenerDiscoveryServiceServer(grpcServer, s.srv)

	errCh := make(chan error, 1)
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		// Hard stop for the same reason the ingress xDS server uses one: ADS
		// streams are open-ended, so GracefulStop would wait for a gateway that
		// only disconnects by dying.
		grpcServer.Stop()
		return nil
	case err := <-errCh:
		return err
	}
}

// sweep drops exemption sets nothing has used recently, so a policy that stops
// being used stops being rendered.
func (s *Server) sweep(ctx context.Context) {
	// Frequent enough that the registry tracks reality within a fraction of the
	// idle timeout, rare enough to be free.
	interval := s.idleTimeout / 10
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce()
		}
	}
}

func (s *Server) sweepOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.now().Add(-s.idleTimeout)
	var dropped []string
	for id, entry := range s.sets {
		if entry.lastUsed.Before(cutoff) {
			dropped = append(dropped, id)
		}
	}
	if len(dropped) == 0 {
		return
	}
	for _, id := range dropped {
		delete(s.sets, id)
	}
	if _, err := s.publishLocked(); err != nil {
		slog.Error("dropping idle egress exemption sets failed", slog.Any("err", err))
		return
	}
	slog.Info("dropped idle egress exemption sets", slog.Int("count", len(dropped)))
}

// currentVersion and ackedVersion read the snapshot counters the acknowledgement
// bookkeeping turns on, for tests and diagnostics.
func (s *Server) currentVersion() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *Server) ackedVersion() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// registeredIDs returns the IDs currently rendered, for tests and diagnostics.
func (s *Server) registeredIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.sets))
}
