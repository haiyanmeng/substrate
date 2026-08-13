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
// Three things about that exchange are worth holding onto.
//
// Envoy talks first, and then stops. After the opening subscribe it caches the
// on-demand secret and never asks again on its own, so nothing a client does
// will prompt a second look at a name. This server does not push replacement
// leaves either -- rotation is gone, and so is the idle sweep that replaced it.
//
// What refreshes a leaf is the resource TTL. Every resource goes out stamped
// with one (see pack), Envoy runs a timer per resource and drops the secret
// when it fires, and the next handshake for that name finds nothing cached and
// re-subscribes. That is the entire mechanism: the server holds no timer, no
// goroutine and no per-name state, and the cost is proportional to traffic,
// because a name nobody asks for is simply dropped and never minted again.
//
// This matters more than it looks, because the failure mode without it is
// silent. Sending no ttl leaves Envoy holding the secret for good; it goes on
// serving the leaf past its notAfter and the handshake still completes, since
// it neither refuses a stale secret nor re-subscribes. An actor cannot object
// either: nothing in the cluster trusts the MITM anchor, so an actor speaking
// TLS through the gateway has verification switched off and an expired leaf is
// indistinguishable from a fresh one. Nothing anywhere would report it.
//
// All of that -- the drop, the lazy re-subscribe, and what happens without a
// ttl -- was measured against Envoy 1.37.5 by poc/sdsmint/expiry. A change to
// these paragraphs should be re-checked there.
//
// A reconnect re-mints, all at once. Envoy's first request on a new stream
// replays what it holds as initial_resource_versions and re-subscribes to the
// whole set, so this server mints every held name together: restarting sdsmint
// produced a fresh leaf for an active name about a second after the socket came
// back, with no handshake to prompt it, and the TTL cadence resumed from there.
// This is the one place minting is not proportional to traffic, and it scales
// with how many names the Envoy beside it is holding.
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
// Removal is why this is DELTA_GRPC. A refused name is answered with
// removed_resources, which state-of-the-world SDS has no way to express --
// there, "gone" and "not in this snapshot" look identical. The removal is the
// point: Envoy fails the paused handshake, which is the intended outcome for an
// SNI that is not a hostname.
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

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

// DeltaSecrets is what DELTA_GRPC drives. It is a long-lived loop that mints
// incrementally rather than serving a fixed snapshot.
func (s *server) DeltaSecrets(stream secretservice.SecretDiscoveryService_DeltaSecretsServer) error {
	ctx := stream.Context()

	st := &deltaStream{
		srv:      s,
		stream:   stream,
		names:    make(map[string]struct{}),
		sendCh:   make(chan *discovery.DeltaDiscoveryResponse, 8),
		sendErr:  make(chan error, 1),
		sendDone: make(chan struct{}),
	}

	go st.sendLoop(ctx)

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
	// stream; this is that state. Membership is the whole of it -- the server
	// keeps nothing per name, because nothing it could keep would be read.
	// The versions Envoy replays on a reconnect are not retained either; see
	// handleInitialResourceVersions.
	names map[string]struct{}
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
// ignored it would treat every one of those names as unknown, so an unsubscribe
// would match nothing.
//
// The replayed versions themselves are dropped. A version here is the serial of
// a leaf this server minted, and there is nothing to compare it against: the
// server holds no copy of what it issued and would not re-mint on a mismatch if
// it did.
func (d *deltaStream) handleInitialResourceVersions(replayed map[string]string) {
	if len(replayed) == 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for name := range replayed {
		d.names[name] = struct{}{}
	}
}

// handleUnsubscribe drops names the client has explicitly given up. This is the
// rare path, and now the only one that shrinks the live set: Envoy volunteers
// an unsubscribe when the configuration referencing a secret goes away, not
// when it simply stops using one. A name it quietly stops asking about is held
// for the life of the stream.
func (d *deltaStream) handleUnsubscribe(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, name := range names {
		delete(d.names, name)
	}
}

// handleSubscribe mints a leaf for every name the client asked for and sends
// the batch. A name this stream already holds is minted again rather than
// skipped. Envoy re-subscribes in two cases and both want a certificate: after
// a resource TTL dropped the secret and a handshake needs it back, and on the
// first request of a new stream, where it re-subscribes to everything it holds.
// Suppressing the repeat would leave the second case with nothing.
func (d *deltaStream) handleSubscribe(ctx context.Context, names []string) error {
	if len(names) == 0 {
		// A bare ACK, or an unsubscribe-only request. Nothing to send.
		return nil
	}

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
	d.names[name] = struct{}{}
	d.mu.Unlock()

	return &discovery.Resource{
		Name:     name,
		Version:  version,
		Resource: body,
		// Envoy starts a timer per resource when it receives one and drops the
		// resource when that timer fires. That drop is the whole refresh
		// mechanism: the next handshake for the name finds nothing cached and
		// re-subscribes, and this server mints again. Stamped on every response
		// rather than the first, because a resource that arrives with no ttl
		// has its timer cleared and is then held for good.
		Ttl: durationpb.New(d.srv.resourceTTL),
	}, nil
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
// DeltaSecrets, which would hold up incoming requests behind a write.
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
