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

package egress

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/router/extproc"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// actorSPIFFETrustDomain is the trust domain of the SPIFFE URI SAN the
// actor-identity CA mints into every actor certificate. The path underneath it
// is "/atespace/<atespace>/actor/<name>"; see cmd/ateapi/internal/actoridentity.
const actorSPIFFETrustDomain = "substrate-actor.local"

// MITMHandler authorizes each request on the egress gateway's decrypted leg
// against its actor's EgressPolicy.
//
// This is the second of the gateway's two egress checkpoints, and it answers
// the question the first one cannot. The CONNECT leg sees an IP:port, because
// that is all SO_ORIGINAL_DST gives atunnel, and it settles who the actor is.
// The destination hostname exists only inside the tunnel, so it is only here —
// after the gateway has terminated the tunneled TLS with a minted leaf — that
// there is a name to police at all.
//
// The two checkpoints are separate handlers rather than one because their
// inputs are separate. Nothing about the actor is re-derived here: a request
// only reaches this leg through a CONNECT the sibling Handler already
// authenticated, and the identity it verified rides across the
// internal-listener hop as filter state Envoy set from the peer certificate.
type MITMHandler struct {
	apiClient ateapipb.ControlClient
}

// NewMITM builds the MITM-leg handler.
func NewMITM(apiClient ateapipb.ControlClient) *MITMHandler {
	return &MITMHandler{apiClient: apiClient}
}

func (h *MITMHandler) Direction() extproc.Direction { return extproc.DirectionEgressMITM }

// HandleRequestHeaders authorizes one request out of an actor's tunnel against
// that actor's current EgressPolicy, fetched on every request so a policy
// change takes effect on the next request rather than on the next tunnel.
//
// It fails closed at every step. An actor whose identity did not survive the
// hop, an actor with no policy at all, a policy no rule of which matches, and a
// control plane that cannot answer are all denials.
func (h *MITMHandler) HandleRequestHeaders(ctx context.Context, md *extproc.RequestMetadata) (extproc.Result, error) {
	actorRef, err := h.actorFromFilterState(md)
	if err != nil {
		// The actor is named by filter state Envoy set from a certificate it
		// verified, so a request that gets here without one is a gateway
		// misconfiguration rather than anything the actor did — but the traffic
		// is still unauthorizable, so it is still refused.
		slog.ErrorContext(ctx, "egress denied: no actor identity on the MITM leg",
			slog.String("attribute", extproc.ActorIdentityFilterStateAttribute),
			slog.Any("err", err))
		return extproc.Result{}, extproc.WrapReqError(envoy_type.StatusCode_Forbidden, err,
			"egress denied: the gateway carried no actor identity for this request")
	}

	dst, err := h.destinationOf(ctx, md)
	if err != nil {
		return extproc.Result{}, extproc.WrapReqError(envoy_type.StatusCode_Forbidden, err,
			"egress denied: the request names no destination this gateway can authorize")
	}

	policy, err := h.apiClient.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{
		Actor: actorRef.ToObjectRef(),
	})
	if err != nil {
		return extproc.Result{}, mapEgressPolicyError(ctx, actorRef, dst, err)
	}

	rule := firstMatchingRule(policy.GetRules(), dst)
	if rule == nil {
		// The denial names the destination the actor asked for and nothing
		// else: which rules exist is the operator's business, not the actor's.
		slog.InfoContext(ctx, "egress denied: no egress policy rule matches",
			slog.Any("actor", actorRef),
			slog.String("destination", dst.String()),
			slog.Int("rules", len(policy.GetRules())))
		return extproc.Result{}, extproc.NewReqError(envoy_type.StatusCode_Forbidden,
			"egress denied: %s is not permitted by the egress policy", dst)
	}

	warnUnappliedEffects(ctx, actorRef, dst, rule)

	slog.DebugContext(ctx, "egress allowed",
		slog.Any("actor", actorRef),
		slog.String("destination", dst.String()),
		slog.String("method", md.Method))

	// Authorized. The request continues unchanged, and above all with its
	// :authority intact: dynamic_forward_proxy resolves the upstream from that
	// header, so rewriting it here would dial a destination other than the one
	// just authorized.
	return extproc.Result{
		Target: dst.String(),
		Response: &extprocv3.HeadersResponse{
			Response: &extprocv3.CommonResponse{},
		},
	}, nil
}

// actorFromFilterState names the actor behind this request from the SPIFFE URI
// SAN the CONNECT leg published as filter state.
//
// That value is not a header and cannot be one. Envoy terminated the CONNECT,
// so the tunneled request is a separate transaction from the one whose
// certificate was verified, and any header inside the tunnel is a channel the
// actor controls end to end — which would let one actor claim another's policy.
// Filter state crosses the internal-listener hop and originates in the peer
// certificate Envoy validated against the actor-identity CA.
func (h *MITMHandler) actorFromFilterState(md *extproc.RequestMetadata) (resources.ActorRef, error) {
	san := md.Attribute(extproc.ActorIdentityFilterStateAttribute)
	if san == "" {
		return resources.ActorRef{}, fmt.Errorf("filter state %q is empty", extproc.ActorIdentityFilterStateKey)
	}
	return parseActorSPIFFEURI(san)
}

// parseActorSPIFFEURI extracts the actor from
// "spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>".
func parseActorSPIFFEURI(raw string) (resources.ActorRef, error) {
	// Envoy joins multiple URI SANs with a comma. An actor certificate carries
	// exactly one, so more than that describes a certificate this gateway
	// cannot attribute to a single actor.
	if strings.Contains(raw, ",") {
		return resources.ActorRef{}, fmt.Errorf("actor identity %q carries more than one URI SAN", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return resources.ActorRef{}, fmt.Errorf("actor identity %q is not a URI: %w", raw, err)
	}
	if u.Scheme != "spiffe" || u.Host != actorSPIFFETrustDomain {
		return resources.ActorRef{}, fmt.Errorf("actor identity %q is not a %s SPIFFE ID", raw, actorSPIFFETrustDomain)
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "atespace" || parts[2] != "actor" {
		return resources.ActorRef{}, fmt.Errorf("actor identity %q is not /atespace/<atespace>/actor/<name>", raw)
	}
	ref := resources.ActorRef{Atespace: parts[1], Name: parts[3]}
	// The CA only ever mints these from control-plane state, so a name that is
	// not a legal resource name means the CA or its inputs are compromised.
	if !resources.IsValidResourceName(ref.Atespace) || !resources.IsValidResourceName(ref.Name) {
		return resources.ActorRef{}, fmt.Errorf("actor identity %q names an invalid actor", raw)
	}
	return ref, nil
}

// destinationOf assembles the destination this request is authorized against:
// the hostname from inside the tunnel, and the address the actor's kernel
// originally dialed, carried over from the CONNECT leg.
func (h *MITMHandler) destinationOf(ctx context.Context, md *extproc.RequestMetadata) (destination, error) {
	hostname, err := parseDestinationHostname(md.Host)
	if err != nil {
		return destination{}, err
	}

	ip, ipErr := parseOriginalDestinationIP(md.Attribute(extproc.EgressDestinationFilterStateAttribute))
	if ipErr != nil {
		// Not fatal on its own: a hostname destination is fully describable
		// without it, and a missing address only means no IPBlockRule can
		// match. It is fatal for a request that named no hostname either, which
		// the check below catches.
		slog.WarnContext(ctx, "egress: no CONNECT destination address for this request; IP block rules cannot match",
			slog.String("attribute", extproc.EgressDestinationFilterStateAttribute),
			slog.Any("err", ipErr))
	}

	if hostname == "" && !ip.IsValid() {
		return destination{}, fmt.Errorf("request authority %q names no hostname and no CONNECT destination was carried: %w", md.Host, ipErr)
	}
	return destination{hostname: hostname, ip: ip}, nil
}

// warnUnappliedEffects reports a matched rule whose effects this gateway cannot
// carry out. The rule still authorizes — effects do not — but a request that
// goes out without the credential the policy meant to attach will fail at the
// origin for a reason nothing else in the path explains.
//
// TODO: apply inject_static_headers once a credential provider exists to
// resolve its substrate-secret:// URIs. Nothing in this repository implements
// one today.
func warnUnappliedEffects(ctx context.Context, actorRef resources.ActorRef, dst destination, rule *ateapipb.EgressRule) {
	injections := rule.GetHostnames().GetEffects().GetInjectStaticHeaders()
	if len(injections) == 0 {
		return
	}
	headers := make([]string, 0, len(injections))
	for _, injection := range injections {
		headers = append(headers, injection.GetHeader())
	}
	slog.WarnContext(ctx, "egress allowed, but the matching rule's credential injection was not applied: no credential provider is implemented",
		slog.Any("actor", actorRef),
		slog.String("destination", dst.String()),
		slog.String("headers", strings.Join(headers, ",")))
}

// mapEgressPolicyError converts a GetActorEgressPolicy failure into a
// client-facing denial.
//
// NotFound is a deny, not a default-allow: an actor with no policy has been
// granted nothing, which is the same answer a policy with no rules gives.
// Transient control-plane failures deny too, as a 503 — the gateway cannot know
// whether the destination was permitted, and the answer it cannot get is not
// "yes".
func mapEgressPolicyError(ctx context.Context, actorRef resources.ActorRef, dst destination, err error) error {
	switch status.Code(err) {
	case codes.NotFound:
		slog.InfoContext(ctx, "egress denied: actor has no egress policy",
			slog.Any("actor", actorRef),
			slog.String("destination", dst.String()))
		return extproc.WrapReqError(envoy_type.StatusCode_Forbidden, err,
			"egress denied: %s is not permitted by the egress policy", dst)
	case codes.Unavailable, codes.DeadlineExceeded:
		slog.ErrorContext(ctx, "egress denied: cannot reach the control plane for the egress policy",
			slog.Any("actor", actorRef),
			slog.String("destination", dst.String()),
			slog.Any("err", err))
		return extproc.WrapReqError(envoy_type.StatusCode_ServiceUnavailable, err,
			"egress policy check unavailable for %s", actorRef)
	default:
		slog.ErrorContext(ctx, "egress denied: egress policy lookup failed",
			slog.Any("actor", actorRef),
			slog.String("destination", dst.String()),
			slog.Any("err", err))
		return extproc.WrapReqError(envoy_type.StatusCode_Forbidden, err,
			"egress denied: the egress policy for %s could not be evaluated", actorRef)
	}
}
