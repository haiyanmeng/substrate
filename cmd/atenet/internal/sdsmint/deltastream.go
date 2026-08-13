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
//	sdsmint -> removed ["api.example.com"]                      nonce 2
//	              Unprompted, after --idle without Envoy mentioning the name;
//	              see withdrawIdle.
//	  Envoy -> ack: response_nonce 2
//
// Three things about that exchange are worth holding onto.
//
// Envoy talks first, and then stops. After the opening subscribe it caches the
// on-demand secret and never asks again, so nothing the client does will
// prompt a second look at a name. This server does not push replacement leaves
// either: a leaf reaches its notAfter under a live subscription and stays
// there, and handshakes for that SNI fail until something removes the secret
// from Envoy's cache.
//
// The idle sweep is that something, and with rotation gone it is the only
// thing that refreshes a leaf. A withdrawal makes Envoy drop the secret, so
// the next handshake for the name subscribes afresh and gets a new
// certificate. Nothing renews a name in place. That puts a requirement on the
// flags that nothing in the code enforces: --idle has to be shorter than
// --leaf-cert-ttl, or a name is withdrawn only after its leaf has already
// expired and every handshake in between fails. With --idle unset, no name is
// ever refreshed and --leaf-cert-ttl is simply how long a host keeps working
// after it is first reached.
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
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

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

	go st.sendLoop(ctx)

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

	d.handleInitialResourceVersions(req.GetInitialResourceVersions())
	d.handleUnsubscribe(req.GetResourceNamesUnsubscribe())
	return d.handleSubscribe(ctx, req.GetResourceNamesSubscribe())
}

// logNACK records that Envoy rejected the last response. Nothing is resent: the
// server has no second thing to offer for the name, and a retry loop against a
// client that is rejecting on principle is worse than the failure.
func (d *deltaStream) logNACK(ctx context.Context, req *discovery.DeltaDiscoveryRequest) {
	ed := req.GetErrorDetail()
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
func (d *deltaStream) handleInitialResourceVersions(replayed map[string]string) {
	if len(replayed) == 0 {
		return
	}

	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for name := range replayed {
		if e, known := d.names[name]; known {
			e.lastTouched = now
			continue
		}
		d.names[name] = &nameEntry{lastTouched: now}
	}
}

// handleUnsubscribe drops names the client has explicitly given up. This is the
// rare path: Envoy volunteers an unsubscribe when the configuration referencing
// a secret goes away, not when it simply stops using one. A name it quietly
// stops asking about is withdrawIdle's job.
func (d *deltaStream) handleUnsubscribe(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range names {
		delete(d.names, name)
	}
}

// handleSubscribe mints a leaf for every name the client asked for and sends
// the batch. A name it already holds is refreshed rather than skipped, because
// a subscribe is also the only thing that keeps a name off the idle sweep.
func (d *deltaStream) handleSubscribe(ctx context.Context, names []string) error {
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
		res, err := d.pack(name, cert)
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

// withdrawIdle returns names the client has stopped asking about.
//
// This is the only thing that ever shrinks the live set, and the only thing
// that ever gets a name a fresh certificate: nothing renews a leaf in place, so
// a withdrawal is what makes Envoy drop the secret and subscribe again on the
// next handshake. Without it a proxy that has seen a host once holds one
// certificate for it until the stream dies, expiry included -- the subscription
// is not refcounted against traffic, has no expiry of its own, and Envoy never
// volunteers that it is finished with a name.
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

	d.srv.log.InfoContext(ctx, "withdrawing idle secrets",
		slog.Int("withdrawn", len(expired)),
		slog.Duration("idle_for", d.srv.idleTimeout),
		slog.Bool("capped", capped),
	)

	// send does the bookkeeping: it drops these from d.names.
	return d.send(ctx, nil, expired)
}

// pack wraps a minted cert as a versioned delta Resource and records the name
// as subscribed.
func (d *deltaStream) pack(name string, cert *certauth.MintedCert) (*discovery.Resource, error) {
	secret := toSecret(name, cert)
	body, err := anypb.New(secret)
	if err != nil {
		return nil, fmt.Errorf("marshalling secret for %q: %w", name, err)
	}
	// The serial changes on every mint, so it is a natural resource version:
	// every mint looks like a new version to Envoy.
	version := cert.Serial

	d.mu.Lock()
	if _, known := d.names[name]; !known {
		d.names[name] = &nameEntry{lastTouched: time.Now()}
	}
	d.mu.Unlock()

	return &discovery.Resource{Name: name, Version: version, Resource: body}, nil
}

func (d *deltaStream) send(ctx context.Context, resources []*discovery.Resource, removed []string) error {
	d.mu.Lock()
	for _, name := range removed {
		delete(d.names, name)
	}
	d.mu.Unlock()

	resp := &discovery.DeltaDiscoveryResponse{
		TypeUrl:          secretTypeURL,
		Resources:        resources,
		RemovedResources: removed,
		Nonce:            d.srv.nextNonce(),
	}

	select {
	case d.sendCh <- resp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendLoop drains sendCh onto the stream. gRPC forbids concurrent Send on a
// stream, so everything is funneled through this one goroutine; sendCh's
// buffer is what keeps a slow client from stalling the select loop in
// DeltaSecrets, which would hold up idle sweeps and incoming requests behind
// a write.
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
