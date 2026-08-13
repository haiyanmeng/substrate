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

// Delta SDS: the stateful, per-connection half of the server. Everything here
// exists because delta xDS keeps a subscription set per stream.
//
// # The conversation
//
// DeltaSecrets is one long-lived bidirectional gRPC stream, not a series of
// request-response pairs. Envoy opens it once, at startup, and both sides then
// speak whenever they have something to say. A typical stream:
//
//	  Envoy -> subscribe ["api.example.com"]
//	              A client hit the MITM listener with that SNI, Envoy had no
//	              secret cached for it, and the TLS handshake is now paused
//	              waiting for one.
//	sdsmint -> resources ["api.example.com" @ version 3f1c...]  nonce 1
//	              A freshly minted leaf. The handshake resumes.
//	  Envoy -> ack: response_nonce 1
//
//	sdsmint -> resources ["api.example.com" @ version 9b04...]  nonce 2
//	              Unprompted, ~2/3 of the way through the leaf's life. Nothing
//	              asked for this; see rotateStale.
//	  Envoy -> ack: response_nonce 2
//
//	sdsmint -> removed ["api.example.com"]                      nonce 3
//	              Also unprompted, after --idle without Envoy mentioning the
//	              name; see withdrawIdle.
//	  Envoy -> ack: response_nonce 3
//
// Three things about that exchange are worth holding onto.
//
// Envoy talks first only once. After the opening subscribe, most of what
// this file sends is unsolicited: Envoy caches an on-demand secret forever and
// never asks again, so a leaf that is not pushed simply expires underneath a
// live subscription and handshakes start failing. Rotation is not an
// optimization, it is the only thing keeping the secret alive.
//
// A request is a bundle, not a command. One message can carry subscribes,
// unsubscribes, an error_detail, and -- on the first message after a
// reconnect -- initial_resource_versions, which is Envoy replaying the whole
// set it still holds so a fresh server can adopt it:
//
//	  Envoy -> initial_resource_versions {"api.example.com": "9b04..."}
//	           subscribe []
//
// handle applies those parts in order; see the methods it dispatches to.
//
// Removal is why this is DELTA_GRPC. A refused name and an idle name are both
// answered with removed_resources, which state-of-the-world SDS has no way to
// express -- there, "gone" and "not in this snapshot" look identical. For a
// refusal the removal is the point: Envoy fails the paused handshake, which is
// the intended outcome for an SNI that is not a hostname.
//
// Every response carries a nonce, and the next request echoes it as
// response_nonce. A request that also carries error_detail is a NACK of that
// specific response. This server logs NACKs and does not correlate nonces
// otherwise; nothing here retries.

package sdsmint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

// rotateFraction is the point in a leaf's lifetime at which we proactively
// push a replacement. Envoy has no TTL of its own for an on-demand secret, so
// if we do not push, the leaf simply expires under a live subscription and
// handshakes start failing.
//
// Rotation refreshes through minter.certificate, so this only replaces
// anything if the minter's cache has already stopped reusing the old leaf by
// now: reuseFraction < rotateFraction < 1 is the invariant.
const rotateFraction = 2.0 / 3.0

const minRotateInterval = 1 * time.Second

// rotateInterval is how often a stream re-mints its names.
func rotateInterval(ttl time.Duration) time.Duration {
	interval := time.Duration(float64(ttl) * rotateFraction)
	if interval < minRotateInterval {
		return minRotateInterval
	}
	return interval
}

// DeltaSecrets is what DELTA_GRPC drives. It is a long-lived loop that mints
// incrementally rather than serving a fixed snapshot.
func (s *server) DeltaSecrets(stream secretservice.SecretDiscoveryService_DeltaSecretsServer) error {
	ctx := stream.Context()

	st := &deltaStream{
		srv:      s,
		stream:   stream,
		names:    make(map[string]*nameEntry),
		sendCh:   make(chan *discovery.DeltaDiscoveryResponse, 8),
		sendErr:  make(chan error, 1),
		sendDone: make(chan struct{}),
	}

	s.metrics.recordStreamOpen(ctx)
	defer func() {
		st.mu.Lock()
		held := make([]string, 0, len(st.names))
		for name := range st.names {
			held = append(held, name)
		}
		st.mu.Unlock()

		// Release the leaves this stream was holding, the same way withdrawIdle
		// does for a name that goes quiet. Nothing else will: rotation and the
		// idle sweep both run per stream, so once this one is gone its names
		// are unreachable and their certificates would sit in the minter cache
		// until capacity pushed them out.
		for _, name := range held {
			s.minter.forget(name)
		}
		s.metrics.recordStreamClose(ctx, len(held))
	}()

	go st.sendLoop(ctx)

	// Rotation is unconditional: Envoy has no TTL of its own for an on-demand
	// secret, so a stream that never pushes lets its leaves expire underneath
	// a live subscription.
	rotateEvery := rotateInterval(s.ttl)
	rotateTicker := time.NewTicker(rotateEvery)
	defer rotateTicker.Stop()
	rotateC := rotateTicker.C
	s.log.DebugContext(ctx, "rotation armed",
		slog.Duration("ttl", s.ttl),
		slog.Duration("rotate_interval", rotateEvery),
	)

	var idleTicker *time.Ticker
	var idleC <-chan time.Time
	if s.idleTimeout > 0 {
		// A name goes idle silently and is only noticed on the next tick, so
		// the interval is how late a withdrawal can be. Taking a fraction of
		// the window keeps that lateness proportional to the timeout the
		// operator configured rather than to a fixed guess, which would be
		// either too coarse for a short timeout or pure wakeups for a long
		// one. The bounds are where proportionality stops paying: under the
		// floor the loop spins for nothing, over the ceiling a name outlives
		// its timeout by longer than is worth the wait.
		const (
			idleSweepDivisor = 4
			minSweepInterval = 250 * time.Millisecond
			maxSweepInterval = 30 * time.Second
		)
		interval := s.idleTimeout / idleSweepDivisor
		if interval < minSweepInterval {
			interval = minSweepInterval
		}
		if interval > maxSweepInterval {
			interval = maxSweepInterval
		}
		idleTicker = time.NewTicker(interval)
		defer idleTicker.Stop()
		idleC = idleTicker.C
		s.log.DebugContext(ctx, "idle sweep armed",
			slog.Duration("idle_timeout", s.idleTimeout),
			slog.Duration("sweep_interval", interval),
		)
	}

	recvCh := make(chan *discovery.DeltaDiscoveryRequest)
	recvErrCh := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				recvErrCh <- err
				return
			}
			select {
			case recvCh <- req:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for send thread to finish before exiting.
	defer func() {
		close(st.sendCh)
		<-st.sendDone
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-st.sendErr:
			return err
		case err := <-recvErrCh:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-idleC:
			if err := st.withdrawIdle(ctx); err != nil {
				return err
			}
		case <-rotateC:
			if err := st.rotateStale(ctx); err != nil {
				return err
			}
		case req := <-recvCh:
			if err := st.handle(ctx, req); err != nil {
				return err
			}
		}
	}
}

// deltaStream is the per-connection state for a DeltaSecrets stream.
type deltaStream struct {
	srv    *server
	stream secretservice.SecretDiscoveryService_DeltaSecretsServer

	// sendCh queues responses for sendLoop. sendErr carries the first send
	// failure back to DeltaSecrets, and sendDone closes once the loop has
	// stopped touching the stream.
	sendCh   chan *discovery.DeltaDiscoveryResponse
	sendErr  chan error
	sendDone chan struct{}

	mu sync.Mutex
	// names is the client's subscription set. Delta xDS is stateful per
	// stream; this is that state.
	names map[string]*nameEntry
}

// nameEntry is the state for one name the client subscribes.
type nameEntry struct {
	// lastTouched is when the client last asked for this name. Two things
	// count as asking: a subscribe, and listing the name in
	// initial_resource_versions, which is how Envoy says what it still holds
	// when a stream reconnects.
	lastTouched time.Time
}

// handle applies one request from the client.
func (d *deltaStream) handle(ctx context.Context, req *discovery.DeltaDiscoveryRequest) error {
	if url := req.GetTypeUrl(); url != "" && url != secretTypeURL {
		return fmt.Errorf("unexpected type_url %q on the SDS stream", url)
	}

	// A request carrying error_detail is a NACK of whatever we last sent, and
	// only that: it brings no subscription changes to apply.
	if req.GetErrorDetail() != nil {
		d.logNACK(ctx, req)
		return nil
	}

	d.handleInitialResourceVersions(ctx, req.GetInitialResourceVersions())
	d.handleUnsubscribe(ctx, req.GetResourceNamesUnsubscribe())
	return d.handleSubscribe(ctx, req.GetResourceNamesSubscribe())
}

// logNACK records that Envoy rejected the last response. Nothing is resent: the
// server has no second thing to offer for the name, and a retry loop against a
// client that is rejecting on principle is worse than the failure.
func (d *deltaStream) logNACK(ctx context.Context, req *discovery.DeltaDiscoveryRequest) {
	ed := req.GetErrorDetail()
	d.srv.metrics.recordNACK(ctx)
	d.srv.log.ErrorContext(ctx, "envoy NACKed an SDS response",
		slog.String("message", ed.GetMessage()),
		slog.Int("code", int(ed.GetCode())),
		slog.String("nonce", req.GetResponseNonce()),
	)
}

// handleInitialResourceVersions adopts what the client says it already holds.
// On a stream reconnect Envoy replays its whole set this way, and a server that
// ignored it would treat every one of those names as unknown: the live count
// would be short, an unsubscribe would match nothing, and the idle sweep could
// never reach them. The replayed versions themselves are not kept; see
// nameEntry.
func (d *deltaStream) handleInitialResourceVersions(ctx context.Context, replayed map[string]string) {
	if len(replayed) == 0 {
		return
	}
	d.srv.metrics.recordResync(ctx, len(replayed))

	now := time.Now()
	adopted := 0
	d.mu.Lock()
	for name := range replayed {
		if e, known := d.names[name]; known {
			e.lastTouched = now
			continue
		}
		d.names[name] = &nameEntry{lastTouched: now}
		adopted++
	}
	d.mu.Unlock()
	d.srv.metrics.addLiveNames(ctx, adopted)
}

// handleUnsubscribe drops names the client has explicitly given up. This is the
// rare path: Envoy volunteers an unsubscribe when the configuration referencing
// a secret goes away, not when it simply stops using one. A name it quietly
// stops asking about is withdrawIdle's job.
func (d *deltaStream) handleUnsubscribe(ctx context.Context, names []string) {
	dropped := 0
	d.mu.Lock()
	for _, name := range names {
		if _, known := d.names[name]; known {
			delete(d.names, name)
			dropped++
		}
	}
	d.mu.Unlock()
	d.srv.metrics.addLiveNames(ctx, -dropped)
	d.srv.metrics.recordUnsubscribe(ctx, len(names))
}

// handleSubscribe mints a leaf for every name the client asked for and sends
// the batch. A name it already holds is refreshed rather than skipped, because
// a subscribe is also the only thing that keeps a name off the idle sweep.
func (d *deltaStream) handleSubscribe(ctx context.Context, names []string) error {
	d.srv.metrics.recordSubscribe(ctx, len(names))
	if len(names) == 0 {
		// A bare ACK, or an unsubscribe-only request. Nothing to send.
		return nil
	}

	// A subscribe is the only signal the client ever sends about a name it
	// already holds, so it is the only thing that can keep one alive. Stamp
	// them all up front, in one pass, because the mint loop below cannot hold
	// the lock.
	now := time.Now()
	d.mu.Lock()
	for _, name := range names {
		if e, known := d.names[name]; known {
			e.lastTouched = now
		}
	}
	d.mu.Unlock()

	var resources []*discovery.Resource
	var removed []string

	for _, name := range names {
		cert, err := d.srv.minter.certificate(ctx, name)
		if err != nil {
			// Refused. Tell Envoy the name does not exist; the paused
			// handshake for that SNI then fails, which is the intended
			// outcome for something that is not a hostname.
			removed = append(removed, name)
			continue
		}
		res, err := d.pack(ctx, name, cert)
		if err != nil {
			return err
		}
		resources = append(resources, res)
	}

	if len(resources) == 0 && len(removed) == 0 {
		return nil
	}
	return d.send(ctx, resources, removed)
}

// rotateStale re-mints the names on this stream whose cached leaf has aged out
// of its reuse window. Two things narrow what that comes to: idle names are
// dropped before minting (see the cutoff below), and minter.certificate hands
// back the existing leaf for anything still inside its window. Note that those
// unchanged names are still pushed, at their existing version -- the reuse
// window skips the signing, not the send.
//
// This is the only way a secret ever changes in the data plane: Envoy caches an
// on-demand secret indefinitely until the server sends a new version or a
// removal.
func (d *deltaStream) rotateStale(ctx context.Context) error {
	// Names the idle sweep is about to withdraw are skipped. Signing a
	// replacement for a certificate that is being dropped in the next few
	// seconds is pure waste, and at scale it is the difference between a
	// rotation tick costing the live set and costing the active set.
	var cutoff time.Time
	if d.srv.idleTimeout > 0 {
		cutoff = time.Now().Add(-d.srv.idleTimeout)
	}

	d.mu.Lock()
	names := make([]string, 0, len(d.names))
	for name, e := range d.names {
		if !cutoff.IsZero() && e.lastTouched.Before(cutoff) {
			continue
		}
		names = append(names, name)
	}
	d.mu.Unlock()

	if len(names) == 0 {
		return nil
	}

	start := time.Now()
	var resources []*discovery.Resource
	var removed []string
	for _, name := range names {
		cert, err := d.srv.minter.certificate(ctx, name)
		if err != nil {
			// Not a mintable name, or minting broke. Withdraw it.
			removed = append(removed, name)
			continue
		}
		res, err := d.pack(ctx, name, cert)
		if err != nil {
			return err
		}
		resources = append(resources, res)
	}

	d.srv.metrics.recordRotation(ctx, time.Since(start), len(resources))
	d.srv.log.InfoContext(ctx, "rotating on-demand secrets",
		slog.Int("pushed", len(resources)),
		slog.Int("withdrawn", len(removed)),
		slog.Duration("took", time.Since(start)),
	)
	return d.send(ctx, resources, removed)
}

// withdrawIdle returns names the client has stopped asking about.
//
// This is the only thing that ever shrinks the live set. Without it a proxy
// that has seen a host once holds a certificate for it until the stream dies:
// the subscription is not refcounted against traffic, has no expiry of its
// own, and Envoy never volunteers that it is finished with a name.
func (d *deltaStream) withdrawIdle(ctx context.Context) error {
	if d.srv.idleTimeout <= 0 {
		return nil
	}
	// maxWithdrawPerSweep bounds how many names one sweep pulls back.
	const maxWithdrawPerSweep = 1024
	cutoff := time.Now().Add(-d.srv.idleTimeout)

	var expired []string
	capped := false
	d.mu.Lock()
	for name, e := range d.names {
		if !e.lastTouched.Before(cutoff) {
			continue
		}
		if len(expired) >= maxWithdrawPerSweep {
			capped = true
			break
		}
		expired = append(expired, name)
	}
	d.mu.Unlock()

	if len(expired) == 0 {
		return nil
	}

	d.srv.metrics.recordIdleWithdrawal(ctx, len(expired))
	d.srv.log.InfoContext(ctx, "withdrawing idle secrets",
		slog.Int("withdrawn", len(expired)),
		slog.Duration("idle_for", d.srv.idleTimeout),
		slog.Bool("capped", capped),
	)

	// Release the leaves on this side too, so reclamation is not one-sided.
	for _, name := range expired {
		d.srv.minter.forget(name)
	}

	// send does the bookkeeping: it drops these from d.names and takes them
	// off the live gauge.
	return d.send(ctx, nil, expired)
}

// pack wraps a minted cert as a versioned delta Resource and records the name
// as subscribed.
func (d *deltaStream) pack(ctx context.Context, name string, cert *certauth.MintedCert) (*discovery.Resource, error) {
	secret := toSecret(name, cert)
	body, err := anypb.New(secret)
	if err != nil {
		return nil, fmt.Errorf("marshalling secret for %q: %w", name, err)
	}
	// The serial changes on every mint, so it is a natural resource version:
	// a re-mint always looks like a new version to Envoy, and a cache hit
	// always looks like the same one.
	version := cert.Serial

	d.mu.Lock()
	_, known := d.names[name]
	if !known {
		d.names[name] = &nameEntry{lastTouched: time.Now()}
	}
	d.mu.Unlock()
	if !known {
		d.srv.metrics.addLiveNames(ctx, 1)
	}

	return &discovery.Resource{Name: name, Version: version, Resource: body}, nil
}

func (d *deltaStream) send(ctx context.Context, resources []*discovery.Resource, removed []string) error {
	withdrawn := 0
	d.mu.Lock()
	for _, name := range removed {
		if _, known := d.names[name]; known {
			delete(d.names, name)
			withdrawn++
		}
	}
	d.mu.Unlock()
	d.srv.metrics.addLiveNames(ctx, -withdrawn)

	resp := &discovery.DeltaDiscoveryResponse{
		TypeUrl:          secretTypeURL,
		Resources:        resources,
		RemovedResources: removed,
		Nonce:            d.srv.nextNonce(),
	}

	if d.srv.metrics.enabled() {
		d.srv.metrics.recordResponse(ctx, proto.Size(resp), len(resources), len(removed))
	}

	select {
	case d.sendCh <- resp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendLoop drains sendCh onto the stream. Sends are funneled through this one
// goroutine because gRPC forbids concurrent Send on a stream, and rotation
// pushes race with responses to incoming requests.
//
// It stops on the first send failure and hands it to sendErr, which is what
// DeltaSecrets returns; a full sendErr means a failure is already on its way
// back, so the second one is dropped rather than blocking the exit.
func (d *deltaStream) sendLoop(ctx context.Context) {
	defer close(d.sendDone)
	for {
		select {
		case <-ctx.Done():
			return
		case resp, ok := <-d.sendCh:
			if !ok {
				return
			}
			if err := d.stream.Send(resp); err != nil {
				select {
				case d.sendErr <- err:
				default:
				}
				return
			}
		}
	}
}
