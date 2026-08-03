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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// inlineBytes wraps PEM bytes as an inline Envoy DataSource. Leaf material is
// inlined rather than written to a path because it is per-connection and
// short-lived; putting it on a filesystem would only widen exposure.
func inlineBytes(b []byte) *corev3.DataSource {
	return &corev3.DataSource{
		Specifier: &corev3.DataSource_InlineBytes{InlineBytes: b},
	}
}

// SecretTypeURL is the xDS type URL for SDS resources.
const SecretTypeURL = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"

// rotateFraction is the point in a leaf's lifetime at which we proactively
// push a replacement. Envoy has no TTL of its own for an on-demand secret, so
// if we do not push, the leaf simply expires under a live subscription and
// handshakes start failing. See README "Open question 1".
//
// Rotation refreshes through Minter.GetCertificate, so this only replaces
// anything if the minter's cache has already stopped reusing the old leaf by
// now: reuseFraction < rotateFraction < 1 is the invariant.
const rotateFraction = 2.0 / 3.0

// maxWithdrawPerSweep bounds how many names one idle sweep pulls back.
//
// Removals are just strings, so the size limit is not the concern -- the
// concern is the data plane. Every withdrawal is a name that goes cold, and a
// sweep that withdraws the entire live set at once turns the next minute of
// traffic into an all-cold-miss storm. Whatever is over budget is withdrawn by
// the following tick, a fraction of an idle window later.
const maxWithdrawPerSweep = 1024

// idleSweepDivisor is how many times per idle window the sweep runs. A name is
// therefore withdrawn within idleTimeout + idleTimeout/divisor of going quiet,
// rather than up to a full window late.
const idleSweepDivisor = 4

// Server implements Envoy's Secret Discovery Service, minting a certificate
// per requested resource name. Because the on-demand certificate selector maps
// SNI to the secret name, "resource name" and "hostname" are the same thing.
type Server struct {
	secretservice.UnimplementedSecretDiscoveryServiceServer

	minter  Minter
	log     *slog.Logger
	metrics *Metrics
	// rotate enables the proactive re-mint push. Off by default so the
	// experiment in the harness can toggle it.
	rotate bool
	ttl    time.Duration
	// idleTimeout, if positive, is how long a name may sit without the client
	// asking for it before the server withdraws it. Zero disables reclamation
	// and the live set only grows.
	idleTimeout time.Duration

	nonce atomic.Uint64
}

// ServerOptions configures NewServer.
type ServerOptions struct {
	Logger *slog.Logger
	// Rotate makes the server push a replacement leaf at ~2/3 of TTL for
	// every name a client is still subscribed to.
	Rotate bool
	// TTL must match the minter's leaf TTL for rotation timing to be right.
	TTL time.Duration
	// IdleTimeout, if positive, withdraws a name the client has not asked for
	// in that long. Zero -- the default -- means names are held for the life
	// of the stream, which is what makes an on-demand deployment's live set
	// monotonically increasing.
	//
	// There is no such thing as server-observable idleness here. Once Envoy
	// holds a secret it never mentions the name again, however much traffic
	// flows for it, so this cannot distinguish "nobody has used this host in
	// an hour" from "this host is busy". Withdrawing a busy name is not an
	// outage: the next connection to it pauses and re-fetches, exactly as it
	// did the first time. The cost of a wrong guess is one cold handshake, and
	// the setting trades that against holding every host forever.
	IdleTimeout time.Duration
	// Metrics, if non-nil, counts streams, subscriptions, response sizes and
	// rotation cost. Pass the same instance the minter got.
	Metrics *Metrics
}

// NewServer builds an SDS server over m.
func NewServer(m Minter, opts ServerOptions) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	return &Server{
		minter:      m,
		log:         opts.Logger,
		metrics:     opts.Metrics,
		rotate:      opts.Rotate,
		ttl:         opts.TTL,
		idleTimeout: opts.IdleTimeout,
	}
}

func (s *Server) nextNonce() string {
	return strconv.FormatUint(s.nonce.Add(1), 10)
}

// toSecret packs a minted cert into the Secret proto Envoy expects back. The
// secret's name MUST equal the requested resource name (the SNI), or Envoy
// will not match the response to its subscription.
func toSecret(name string, c *MintedCert) *tlsv3.Secret {
	return &tlsv3.Secret{
		Name: name,
		Type: &tlsv3.Secret_TlsCertificate{
			TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: inlineBytes(c.CertChainPEM),
				PrivateKey:       inlineBytes(c.PrivateKeyPEM),
			},
		},
	}
}

// DeltaSecrets is what DELTA_GRPC drives. It is a long-lived loop that mints
// incrementally rather than serving a fixed snapshot.
//
// Measured, and not what this was originally written to expect: Envoy opens a
// separate stream per secret name and holds it open, so in practice each of
// these loops carries exactly one subscription. The code does not rely on that
// -- a stream with many names still works -- but it is why the live host count
// is a concurrent-request count against the SDS cluster.
//
// Note on failure signalling: the intuitive design is to NACK a name that fails
// validation, but NACK is a *client* action in xDS -- a server cannot NACK. The
// server-side way to say "this name will not be issued" is to return it in
// removed_resources, which per the Envoy docs also cancels the data-plane
// subscription for that name. That is what we do.
func (s *Server) DeltaSecrets(stream secretservice.SecretDiscoveryService_DeltaSecretsServer) error {
	ctx := stream.Context()

	st := &deltaStream{
		srv:    s,
		stream: stream,
		names:  make(map[string]*nameEntry),
		sendCh: make(chan *discovery.DeltaDiscoveryResponse, 8),
	}

	s.metrics.recordStreamOpen()
	defer func() {
		st.mu.Lock()
		held := len(st.names)
		st.mu.Unlock()
		s.metrics.recordStreamClose(held)
	}()

	// Sends are funnelled through one goroutine because gRPC forbids
	// concurrent Send on a stream, and rotation pushes race with responses to
	// incoming requests.
	sendErr := make(chan error, 1)
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		for {
			select {
			case <-ctx.Done():
				return
			case resp, ok := <-st.sendCh:
				if !ok {
					return
				}
				if err := stream.Send(resp); err != nil {
					select {
					case sendErr <- err:
					default:
					}
					return
				}
			}
		}
	}()

	var rotateTicker *time.Ticker
	var rotateC <-chan time.Time
	if s.rotate {
		interval := time.Duration(float64(s.ttl) * rotateFraction)
		if interval < time.Second {
			interval = time.Second
		}
		rotateTicker = time.NewTicker(interval)
		defer rotateTicker.Stop()
		rotateC = rotateTicker.C
	}

	var idleTicker *time.Ticker
	var idleC <-chan time.Time
	if s.idleTimeout > 0 {
		interval := s.idleTimeout / idleSweepDivisor
		// The floor keeps a very short timeout -- which only a test would set
		// -- from turning into a busy loop. The ceiling keeps a very long one
		// from making the sweep so rare that the reclamation lag is dominated
		// by the polling rather than the timeout.
		if interval < 250*time.Millisecond {
			interval = 250 * time.Millisecond
		}
		if interval > 30*time.Second {
			interval = 30 * time.Second
		}
		idleTicker = time.NewTicker(interval)
		defer idleTicker.Stop()
		idleC = idleTicker.C
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

	defer func() {
		close(st.sendCh)
		<-sendDone
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-sendErr:
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
			if err := st.rotateAll(ctx); err != nil {
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
	srv    *Server
	stream secretservice.SecretDiscoveryService_DeltaSecretsServer
	sendCh chan *discovery.DeltaDiscoveryResponse

	mu sync.Mutex
	// names is the client's subscription set. Delta xDS is stateful per
	// stream; this is that state.
	names map[string]*nameEntry
}

// nameEntry is what the server knows about one live resource name.
type nameEntry struct {
	// version is the resource version the client last accepted.
	version string
	// touched is the last time the client showed interest in this name: a
	// subscribe, or a replay in initial_resource_versions after a reconnect.
	//
	// Rotation deliberately does not update it. A rotation is the server
	// talking to the client, not the client saying the name still matters, and
	// refreshing it here would mean an idle name was kept alive forever by the
	// very pushes that make it expensive.
	touched time.Time
}

func (d *deltaStream) handle(ctx context.Context, req *discovery.DeltaDiscoveryRequest) error {
	if req.GetTypeUrl() != "" && req.GetTypeUrl() != SecretTypeURL {
		return fmt.Errorf("unexpected type_url %q on the SDS stream", req.GetTypeUrl())
	}

	// A request carrying error_detail is a NACK of whatever we last sent. It
	// is not a new subscription, but it is worth surfacing loudly: it means
	// Envoy rejected a certificate we minted.
	if ed := req.GetErrorDetail(); ed != nil {
		d.srv.metrics.recordNACK()
		d.srv.log.ErrorContext(ctx, "envoy NACKed an SDS response",
			slog.String("message", ed.GetMessage()),
			slog.Int("code", int(ed.GetCode())),
			slog.String("nonce", req.GetResponseNonce()),
		)
		return nil
	}

	// On stream reconnect Envoy replays what it already holds. Seed our view
	// so we do not re-push resources it has, and so unsubscribes are correct.
	if replayed := req.GetInitialResourceVersions(); len(replayed) > 0 {
		d.srv.metrics.recordResync(len(replayed))
	}
	now := time.Now()
	adopted, dropped := 0, 0
	d.mu.Lock()
	for name, version := range req.GetInitialResourceVersions() {
		if e, known := d.names[name]; known {
			e.touched = now
			continue
		}
		d.names[name] = &nameEntry{version: version, touched: now}
		adopted++
	}
	for _, name := range req.GetResourceNamesUnsubscribe() {
		if _, known := d.names[name]; known {
			delete(d.names, name)
			dropped++
		}
	}
	// A subscribe is the only signal the client ever sends about a name it
	// already holds, so it is the only thing that can keep one alive. Stamp
	// them before minting, in one pass, because the mint loop below cannot
	// hold this lock.
	for _, name := range req.GetResourceNamesSubscribe() {
		if e, known := d.names[name]; known {
			e.touched = now
		}
	}
	d.mu.Unlock()
	d.srv.metrics.addLiveNames(adopted - dropped)
	d.srv.metrics.recordUnsubscribe(len(req.GetResourceNamesUnsubscribe()))

	subscribe := req.GetResourceNamesSubscribe()
	d.srv.metrics.recordSubscribe(len(subscribe))
	if len(subscribe) == 0 {
		// A bare ACK, or an unsubscribe-only request. Nothing to send.
		return nil
	}

	var resources []*discovery.Resource
	var removed []string

	for _, name := range subscribe {
		cert, err := d.srv.minter.GetCertificate(ctx, name)
		if err != nil {
			// Refused. Tell Envoy the name does not exist; the paused
			// handshake for that SNI then fails, which is the intended
			// outcome for a host outside the allowlist.
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

// rotateAll re-mints every live subscription and pushes the replacements. This
// is the only way a secret gets refreshed: Envoy caches an on-demand secret
// indefinitely until the server sends a new version or a removal.
func (d *deltaStream) rotateAll(ctx context.Context) error {
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
		if !cutoff.IsZero() && e.touched.Before(cutoff) {
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
		cert, err := d.srv.minter.GetCertificate(ctx, name)
		if err != nil {
			// The allowlist changed under us, or minting broke. Withdraw it.
			removed = append(removed, name)
			continue
		}
		res, err := d.pack(name, cert)
		if err != nil {
			return err
		}
		resources = append(resources, res)
	}

	d.srv.metrics.recordRotation(time.Since(start), len(resources))
	d.srv.log.InfoContext(ctx, "rotating on-demand secrets",
		slog.Int("pushed", len(resources)),
		slog.Int("withdrawn", len(removed)),
		slog.Duration("took", time.Since(start)),
	)
	return d.send(ctx, resources, removed)
}

// withdrawIdle returns names the client has stopped asking about to the client
// as removals, which per the Envoy docs cancels the data-plane subscription
// for each one.
//
// This is the only thing that ever shrinks the live set. Without it a proxy
// that has seen a host once holds a certificate for it until the stream dies:
// the subscription is not refcounted against traffic, has no expiry of its
// own, and Envoy never volunteers that it is finished with a name. Measured at
// ~60KB of Envoy RSS per live secret, "held forever" is the difference between
// a bounded footprint and one that tracks the number of distinct hosts the
// proxy has ever been asked for.
//
// What it costs: a withdrawn name that turns out to still be wanted pauses its
// next handshake and re-fetches. That is the ordinary cold path, already
// measured in phase 2, and it is bounded by the mint -- not an error the
// client sees.
func (d *deltaStream) withdrawIdle(ctx context.Context) error {
	if d.srv.idleTimeout <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-d.srv.idleTimeout)

	var expired []string
	capped := false
	d.mu.Lock()
	for name, e := range d.names {
		if !e.touched.Before(cutoff) {
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

	d.srv.metrics.recordIdleWithdrawal(len(expired))
	d.srv.log.InfoContext(ctx, "withdrawing idle secrets",
		slog.Int("withdrawn", len(expired)),
		slog.Duration("idle_for", d.srv.idleTimeout),
		slog.Bool("capped", capped),
	)

	// Release the leaves on this side too, so reclamation is not one-sided.
	// Optional because a minter with a fixed pool has nothing to give back.
	if f, ok := d.srv.minter.(Forgetter); ok {
		for _, name := range expired {
			f.Forget(name)
		}
	}

	// send does the bookkeeping: it drops these from d.names and takes them
	// off the live gauge.
	return d.send(ctx, nil, expired)
}

// pack wraps a minted cert as a versioned delta Resource and records the
// version as subscribed.
func (d *deltaStream) pack(name string, cert *MintedCert) (*discovery.Resource, error) {
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
	e, known := d.names[name]
	if !known {
		// Only a subscribe reaches an unknown name -- rotation walks names
		// that are already here -- so this is first contact, and now is the
		// right touch time.
		e = &nameEntry{touched: time.Now()}
		d.names[name] = e
	}
	e.version = version
	d.mu.Unlock()
	if !known {
		d.srv.metrics.addLiveNames(1)
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
	d.srv.metrics.addLiveNames(-withdrawn)

	resp := &discovery.DeltaDiscoveryResponse{
		TypeUrl:          SecretTypeURL,
		Resources:        resources,
		RemovedResources: removed,
		Nonce:            d.srv.nextNonce(),
	}
	// Sized here rather than after Send because the send goroutine has no
	// error path back to the caller. proto.Size walks the message, so it is
	// only paid when counters are on -- but it is the only way to see a
	// rotation response cross Envoy's 4MB gRPC receive limit before it fails.
	if d.srv.metrics.enabled() {
		d.srv.metrics.recordResponse(proto.Size(resp), len(resources), len(removed))
	}
	select {
	case d.sendCh <- resp:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StreamSecrets implements state-of-the-world SDS. The on-demand selector
// should be configured with DELTA_GRPC — SotW cannot express resource removal,
// so a refused or rotated-away name has no wire representation. It is provided
// for completeness and for clients that predate delta.
func (s *Server) StreamSecrets(stream secretservice.SecretDiscoveryService_StreamSecretsServer) error {
	ctx := stream.Context()
	var version uint64

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if ed := req.GetErrorDetail(); ed != nil {
			s.log.ErrorContext(ctx, "envoy NACKed a SotW SDS response",
				slog.String("message", ed.GetMessage()))
			continue
		}
		if len(req.GetResourceNames()) == 0 {
			continue
		}

		resp, err := s.buildSotW(ctx, req.GetResourceNames(), &version)
		if err != nil {
			return err
		}
		if resp == nil {
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// FetchSecrets is the unary form of SotW SDS.
func (s *Server) FetchSecrets(ctx context.Context, req *discovery.DiscoveryRequest) (*discovery.DiscoveryResponse, error) {
	var version uint64
	resp, err := s.buildSotW(ctx, req.GetResourceNames(), &version)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &discovery.DiscoveryResponse{TypeUrl: SecretTypeURL}, nil
	}
	return resp, nil
}

func (s *Server) buildSotW(ctx context.Context, names []string, version *uint64) (*discovery.DiscoveryResponse, error) {
	var resources []*anypb.Any
	for _, name := range names {
		cert, err := s.minter.GetCertificate(ctx, name)
		if err != nil {
			// SotW has no removal channel, so a refusal can only be expressed
			// by omission.
			continue
		}
		body, err := anypb.New(toSecret(name, cert))
		if err != nil {
			return nil, fmt.Errorf("marshalling secret for %q: %w", name, err)
		}
		resources = append(resources, body)
	}
	if len(resources) == 0 {
		return nil, nil
	}
	*version++
	return &discovery.DiscoveryResponse{
		TypeUrl:     SecretTypeURL,
		VersionInfo: strconv.FormatUint(*version, 10),
		Resources:   resources,
		Nonce:       s.nextNonce(),
	}, nil
}
