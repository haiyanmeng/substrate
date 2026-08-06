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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Checkpoint names which of the gateway's two decision points a Server serves.
// The same policy table backs both; only the visible half of the tuple differs.
type Checkpoint string

const (
	// CheckpointConnect runs on the actor's CONNECT. Sees destination IP:port and
	// the X-Ate-* metadata; no hostname.
	CheckpointConnect Checkpoint = "connect"
	// CheckpointInner runs on the tunneled request after MITM. Sees the hostname
	// and the request headers; carries no actor identity of its own.
	CheckpointInner Checkpoint = "inner"
)

// actorSource records how a checkpoint learned the actor.
//
// The CONNECT checkpoint has exactly one source -- the peer certificate -- so
// these counters only vary at the inner checkpoint, where the identity has to
// cross a CONNECT tunnel. There is one sound way to do that (filter state) and
// the counters exist to prove it is the one that fired.
const (
	sourceClientCert  = "client_cert"
	sourceFilterState = "filter_state"
	sourceNone        = "none"
)

// filterStateActorKey is the object_key the CONNECT chain publishes via
// envoy.filters.http.set_filter_state. It must carry factory_key: envoy.string,
// or Envoy rejects the config at load with "does not have an object factory".
const filterStateActorKey = "ate.actor"

// Server implements Envoy's ExternalProcessor for one checkpoint.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer

	checkpoint Checkpoint
	store      *Store
	stats      *Stats
	// innerWired reports whether a CheckpointInner server is actually mounted on
	// the gateway's MITM leg. When it is not, the CONNECT checkpoint cannot defer
	// a decision to anyone, so it denies instead. See WithInnerCheckpoint.
	innerWired bool
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithInnerCheckpoint declares that the gateway also runs the inner checkpoint,
// which lets the CONNECT checkpoint defer the two hostname policies to it.
//
// Off by default, and deliberately so. Without an inner checkpoint a deferral
// has nobody to defer to, and treating it as an allow would leave
// ALLOW_BY_HOSTNAME and BASIC_CREDENTIAL_INJECT actors constrained only by the
// gateway's global sdsmintd allowlist -- a per-actor bypass that looks like
// working authorization from every log line.
func WithInnerCheckpoint() ServerOption {
	return func(s *Server) { s.innerWired = true }
}

// NewServer binds a checkpoint to a policy store. The store is shared between
// the two checkpoints so a single swap updates both atomically.
func NewServer(cp Checkpoint, store *Store, stats *Stats, opts ...ServerOption) *Server {
	s := &Server{checkpoint: cp, store: store, stats: stats}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Serve runs the gRPC server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	srv := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(srv, s)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		srv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

// Process handles one ext_proc stream. Envoy opens one per HTTP stream, so this
// runs for the life of a single request and must not hold any lock across
// iterations.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		var resp *extprocv3.ProcessingResponse
		switch v := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			resp = s.onRequestHeaders(ctx, v.RequestHeaders, req.GetAttributes())
		default:
			// The filter config sets every other processing mode to SKIP, so
			// nothing else should arrive. Continue without mutating rather than
			// failing the request, but log: this means config and code disagree.
			slog.WarnContext(ctx, "Unexpected ext_proc message",
				slog.String("checkpoint", string(s.checkpoint)),
				slog.String("type", fmt.Sprintf("%T", v)))
			s.stats.Inc(string(s.checkpoint) + ".unexpected_message")
			resp = continueResponse(&extprocv3.HeaderMutation{}, false)
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *Server) onRequestHeaders(
	ctx context.Context,
	h *extprocv3.HttpHeaders,
	attrs map[string]*structpb.Struct,
) *extprocv3.ProcessingResponse {
	info := newRequestInfo(h.GetHeaders().GetHeaders())
	s.stats.Inc(string(s.checkpoint) + ".requests")

	snap := s.store.Load()
	if snap == nil {
		// No policy table yet. A replica that has not synced must deny, and its
		// readiness probe should already be failing so Envoy stops sending here.
		s.stats.Inc(string(s.checkpoint) + ".deny.no_snapshot")
		slog.ErrorContext(ctx, "Denying: no policy snapshot loaded",
			slog.String("checkpoint", string(s.checkpoint)))
		return denyResponse("gateway has no policy loaded")
	}

	switch s.checkpoint {
	case CheckpointConnect:
		return s.decideConnect(ctx, info, snap)
	case CheckpointInner:
		return s.decideInner(ctx, info, snap, attrs)
	}
	return denyResponse("gateway misconfigured: unknown checkpoint")
}

// decideConnect resolves the three IP-answerable policies and defers the other
// two to the inner checkpoint by routing them to MITM.
func (s *Server) decideConnect(ctx context.Context, info *requestInfo, snap *Snapshot) *extprocv3.ProcessingResponse {
	if info.method != "" && !strings.EqualFold(info.method, "CONNECT") {
		// The connect checkpoint is only mounted on the CONNECT listener. A
		// non-CONNECT here means the filter is attached to the wrong chain, which
		// would silently apply CONNECT-shaped policy to ordinary requests.
		s.stats.Inc("connect.deny.not_connect")
		slog.ErrorContext(ctx, "Denying: connect checkpoint saw a non-CONNECT method",
			slog.String("method", info.method))
		return denyResponse("connect checkpoint reached by a non-CONNECT request")
	}

	if info.identityErr != nil {
		// A malformed or unusable client certificate. Distinct from a valid
		// certificate that carries no ActorIdentity, which falls through to the
		// ordinary unknown-actor denial below.
		s.stats.Inc("connect.deny.bad_identity")
		slog.ErrorContext(ctx, "Denying: could not read an actor identity from the peer certificate",
			slog.Any("err", info.identityErr))
		return denyResponse("peer certificate carries no usable actor identity")
	}

	actor := info.actor
	policy, known := snap.Lookup(actor)
	dst, dstOK := info.destination()
	dec := DecideConnect(policy, dst, dstOK)

	slog.InfoContext(ctx, "CONNECT checkpoint",
		slog.String("actor", actor.String()),
		slog.String("actorSource", sourceClientCert),
		slog.String("actorUID", info.actorUID),
		slog.Bool("policyKnown", known),
		slog.String("policy", string(policy.Kind)),
		slog.String("authority", info.authority),
		slog.String("mode", string(dec.Mode)),
		slog.String("reason", dec.Reason),
		slog.Int("policyRev", snap.Rev))

	if dec.Mode == ModeMITM && !s.innerWired {
		// DecideConnect returns ModeMITM as a deferral, not an allow. With no
		// inner checkpoint mounted there is nothing to defer to, so this fails
		// closed. See WithInnerCheckpoint.
		s.stats.Inc("connect.deny")
		s.stats.Inc("connect.deny.no_inner_checkpoint")
		slog.WarnContext(ctx, "Denying: policy needs the inner checkpoint, which is not wired",
			slog.String("actor", actor.String()),
			slog.String("policy", string(policy.Kind)))
		return denyResponse("policy " + string(policy.Kind) + " requires the inner checkpoint, which this gateway does not run")
	}

	if !dec.Allowed() {
		s.stats.Inc("connect.deny")
		s.stats.Inc("connect.deny." + string(policy.Kind))
		if !known {
			s.stats.Inc("connect.deny.unknown_actor")
		}
		return denyResponse(dec.Reason)
	}

	mut := &extprocv3.HeaderMutation{}
	// Always overwrite: an actor that sets x-ate-egress-mode on its own CONNECT
	// must not be able to pick its route.
	setHeader(mut, EgressModeHeader, string(dec.Mode))
	setHeader(mut, ActorKeyHeader, actor.String())

	s.stats.Inc("connect.allow")
	s.stats.Inc("connect.mode." + string(dec.Mode))
	s.stats.Inc("connect.allow." + string(policy.Kind))

	// clear_route_cache is mandatory: Envoy selects the route before applying
	// this mutation, so without it the mode header never affects routing and
	// every request lands on the fallback route with a 200.
	return continueResponse(mut, true)
}

// decideInner resolves the two hostname policies against the inner request's
// :authority and applies credential injection.
func (s *Server) decideInner(
	ctx context.Context,
	info *requestInfo,
	snap *Snapshot,
	attrs map[string]*structpb.Struct,
) *extprocv3.ProcessingResponse {
	actor, source := s.resolveActor(info, attrs)
	s.stats.Inc("inner.actor_source." + source)

	policy, known := snap.Lookup(actor)
	dec := DecideInner(policy, info.authority)

	slog.InfoContext(ctx, "Inner checkpoint",
		slog.String("actor", actor.String()),
		slog.String("actorSource", source),
		slog.Bool("policyKnown", known),
		slog.String("policy", string(policy.Kind)),
		slog.String("host", info.authority),
		slog.Bool("allow", dec.Allow),
		slog.String("reason", dec.Reason),
		slog.Int("injections", len(dec.Injections)),
		slog.Int("policyRev", snap.Rev),
		slog.Any("attributes", flattenAttributes(attrs)))

	if !dec.Allow {
		s.stats.Inc("inner.deny")
		s.stats.Inc("inner.deny." + string(policy.Kind))
		if !known {
			s.stats.Inc("inner.deny.unknown_actor")
		}
		return denyResponse(dec.Reason)
	}

	mut := &extprocv3.HeaderMutation{}

	// Credential injection. Remove first, then set: an actor that sends both the
	// From header and the To header must not have either of its values survive.
	for _, in := range dec.Injections {
		if in.From != "" {
			removeHeader(mut, in.From)
		}
		removeHeader(mut, in.To)
		setHeader(mut, strings.ToLower(in.To), in.Value)
		s.stats.Inc("inner.inject")
	}

	// Hygiene: substrate-internal headers must not reach the real destination.
	for _, h := range tunnelInternalHeaders {
		removeHeader(mut, h)
	}

	s.stats.Inc("inner.allow")
	s.stats.Inc("inner.allow." + string(policy.Kind))

	// The inner mutation does not affect route selection here, so the route cache
	// need not be cleared. Set it if a deployment routes on injected headers.
	return continueResponse(mut, false)
}

// resolveActor determines which actor the inner request belongs to.
//
// Filter state is the only source, and there is deliberately no fallback. By
// the time a request reaches the inner listener the peer certificate belongs to
// Envoy's own MITM, not the actor, so nothing on this leg is self-evidently
// trustworthy: an x-ate-actor-key header here travels inside the tunnel, which
// is a channel the actor controls end to end, and honoring it would let an
// actor name another actor and inherit its policy -- injected credentials
// included. Missing filter state resolves to the zero key, which has no policy,
// which denies.
//
// The value is what the CONNECT checkpoint wrote, and that came from the
// authenticated peer certificate, so the chain back to a real identity is
// unbroken.
func (s *Server) resolveActor(_ *requestInfo, attrs map[string]*structpb.Struct) (ActorKey, string) {
	if v, ok := lookupAttribute(attrs, filterStateActorKey); ok {
		if k, ok := actorKeyFromHeader(v); ok {
			return k, sourceFilterState
		}
	}
	return ActorKey{}, sourceNone
}

// lookupAttribute searches every filter's attribute struct for a key, matching
// either the bare name or a "filter_state.<name>"-style suffix.
//
// Envoy nests attributes under the requesting filter's name and the exact key
// spelling depends on how request_attributes was written, so this matches
// leniently rather than pinning a shape the PoC has not yet confirmed.
func lookupAttribute(attrs map[string]*structpb.Struct, want string) (string, bool) {
	for _, st := range attrs {
		for k, v := range st.GetFields() {
			if k != want && !strings.HasSuffix(k, "."+want) && !strings.HasSuffix(k, "['"+want+"']") {
				continue
			}
			if sv := v.GetStringValue(); sv != "" {
				return sv, true
			}
		}
	}
	return "", false
}

// flattenAttributes renders the attribute map for logging, sorted so successive
// log lines are diffable.
func flattenAttributes(attrs map[string]*structpb.Struct) []string {
	var out []string
	for filter, st := range attrs {
		for k, v := range st.GetFields() {
			out = append(out, fmt.Sprintf("%s/%s=%s", filter, k, v.String()))
		}
	}
	sort.Strings(out)
	return out
}
