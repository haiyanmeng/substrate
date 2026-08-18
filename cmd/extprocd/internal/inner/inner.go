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

// Package inner implements the ext_proc server for the egress gateway's inner
// checkpoint: the filter that runs on a tunneled request after Envoy has
// minted a leaf for the SNI, terminated the actor's TLS, and decrypted. The
// real hostname, method, and path are visible here and nowhere earlier.
//
// The CONNECT checkpoint (cmd/atenet/internal/router/egress) answers a
// different question — "is this a running actor" — against a different input:
// atunnel takes the destination from SO_ORIGINAL_DST and validateDestination
// rejects hostnames, so that filter sees an IP and a port. Hostname policy has
// to live here or nowhere.
//
// Two things this checkpoint does NOT have, both by construction:
//
//   - No client certificate. mitm_listener has no validation_context, so there
//     is no XFCC on this leg and no identity in the headers.
//   - No trustworthy x-ate-* headers. The actor controls the bytes inside its
//     own tunnel, so anything self-identifying that arrives here was written by
//     the workload being policed. Evaluate must not read one.
//
// Identity therefore crosses as filter state (filter_state['ate.actor']),
// which this server reads into Request.Actor. The gateway captures it on the
// CONNECT listener, where the actor's certificate is still present, as that
// certificate's SPIFFE URI SAN:
//
//	spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>
//
// Evaluate gets it verbatim, and gets it split into Request.Atespace and
// Request.ActorName. Two failure modes to know about, because neither produces
// an error anywhere. Actor is empty if the
// gateway's set_filter_state filter, mitm_internal's internal_upstream
// transport socket, or the inner ext_proc's request_attributes entry is
// missing or renamed — see manifests/ate-install/atenet-egress.yaml. And it is
// empty, legitimately, on any request whose downstream presented no
// certificate. Evaluate must treat "" as unidentified, never as a match.
package inner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// DenyDetails is the response_code_details a denial carries. It populates
// %RESPONSE_CODE_DETAILS% in the gateway's access log, which is what makes an
// extprocd denial distinguishable from the CONNECT checkpoint's 403 and from
// the cleartext chain's direct_response — all three are otherwise just a 403
// on a request the actor believed it was allowed to make.
const DenyDetails = "extprocd_egress_policy_denied"

// denyBody is what the actor is told. Deliberately the same string for every
// denial: the reason is a property of the gateway's policy, and echoing it to
// the workload turns the deny path into a way to enumerate that policy.
const denyBody = "egress denied: destination is not permitted by egress policy\n"

// Request is everything the inner checkpoint can see about one tunneled
// request. Constructed from the ext_proc RequestHeaders message; a Policy gets
// this rather than the protobuf so it cannot accidentally depend on the parts
// of the message that are empty here (attributes, metadata_context, bodies).
type Request struct {
	// Method is the real verb, not CONNECT.
	Method string

	// Authority is the destination as the tunneled request itself named it,
	// possibly with a port. Use Host to match on the name alone.
	//
	// This is the name that gets dialed: dynamic_forward_proxy runs after this
	// filter and resolves from this same header, so a name allowed here is the
	// name the socket goes to.
	Authority string

	// Path and Scheme are present on this leg. They are absent at the CONNECT
	// checkpoint, which has no path and no scheme to speak of.
	Path   string
	Scheme string

	// Headers is every request header, keys lower-cased. Includes the x-ate-*
	// names the actor may have set inside its own tunnel — see the package
	// comment before reading one.
	Headers Headers

	// Actor is the calling actor as the gateway asserted it in filter state,
	// or "" when the gateway published nothing. Unlike everything else on this
	// struct it is not derived from bytes the workload controls, so it is the
	// only field a Policy may use to decide who is calling.
	//
	// It is the SPIFFE URI SAN from the actor's certificate, verbatim:
	//
	//	spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>
	//
	// Empty is the case to get right. It does not mean "no actor", it means
	// "not asserted" — an unwired listener and a spoofed one look identical
	// from here — so a Policy keyed on Actor must fail closed on "".
	Actor string

	// Atespace and ActorName are Actor split into its two parts, both "" when
	// Actor is "" or does not parse. They are set together, so testing either
	// one is enough.
	//
	// Prefer these over matching on Actor with a prefix or a substring: the
	// trust domain and the /atespace/.../actor/... shape are checked during
	// the split, so a policy comparing these is comparing values that were
	// verified to be an actor identity rather than merely to contain one.
	Atespace  string
	ActorName string
}

// Headers is one request's headers, keys lower-cased.
//
// A named type only so it can carry LogValue: header sets are logged whole,
// and the rendering should live in one place rather than at each call site.
type Headers map[string]string

// redactedHeaders are logged by name with their value withheld. The gateway
// logs the full header set, and an actor's tunneled request can carry its own
// upstream credentials — so without this, turning on debug logging copies
// every actor's secrets into the cluster's log pipeline, where they outlive
// the request and are readable by anyone with log access.
//
// Presence is still recorded, because "did a credential arrive" is usually the
// question being asked; only the bytes are dropped.
var redactedHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

// LogValue renders the headers as a sorted group, one attribute per header.
//
// Sorted because an unordered map makes two logs of the same request
// impossible to diff. A group rather than a single formatted string so that a
// JSON handler emits a real object and a filter can select one header.
//
// This is deliberately a LogValuer and not a pre-built attribute: the work
// happens only if the record is actually handled, so the common case of a
// gateway running at info level pays nothing for the debug-level header dump.
func (h Headers) LogValue() slog.Value {
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	attrs := make([]slog.Attr, 0, len(keys))
	for _, key := range keys {
		if redactedHeaders[key] {
			attrs = append(attrs, slog.String(key, fmt.Sprintf("<redacted, %d bytes>", len(h[key]))))
			continue
		}
		attrs = append(attrs, slog.String(key, h[key]))
	}
	return slog.GroupValue(attrs...)
}

// Host is Authority with any port removed and the name lower-cased, which is
// the form hostname policy should match on. DNS names are case-insensitive, so
// a policy comparing Authority directly would be bypassed by an uppercase Host
// header.
func (r *Request) Host() string {
	authority := r.Authority
	if host, _, err := net.SplitHostPort(authority); err == nil {
		authority = host
	}
	return strings.ToLower(strings.TrimSuffix(authority, "."))
}

// Decision is Evaluate's answer for one request.
type Decision struct {
	// Allow passes the request to dynamic_forward_proxy and then upstream.
	Allow bool

	// Reason explains a denial in the gateway's logs. It is never sent to the
	// actor; see denyBody.
	Reason string

	// Credential is a header to set on the request on its way upstream, which
	// is how the gateway hands an actor a secret the actor never held. This is
	// the seam the whole MITM exists for: the leg is decrypted here, so a
	// header set here reaches the upstream inside the re-originated TLS
	// session and is never visible to the workload that made the request.
	//
	// Nil means inject nothing, which is the shipped behavior. It is honored
	// only on an allow — a denial is answered by Envoy itself and must never
	// carry a credential, because the actor would then see it in its own 403.
	Credential *Credential
}

// Credential is one header the gateway sets on an outbound request.
type Credential struct {
	// Name is the header name, e.g. "authorization". It must not be a
	// pseudo-header: rewriting :authority would send the credential to a name
	// the SNI was never policed for, which is the one mutation that turns
	// injection into exfiltration.
	Name string

	// Value is the secret. Never log it, and note that it survives no further
	// than the upstream request — Envoy applies the mutation after this filter
	// returns and nothing downstream of that reads it back.
	Value string
}

// Allow permits a request, injecting nothing.
func Allow() Decision { return Decision{Allow: true} }

// AllowWithCredential permits a request and sets one header on it upstream.
func AllowWithCredential(name, value string) Decision {
	return Decision{Allow: true, Credential: &Credential{Name: name, Value: value}}
}

// Deny refuses a request. reason is logged, not returned to the caller.
func Deny(reason string) Decision { return Decision{Reason: reason} }

// Server is the ext_proc gRPC service extprocd serves to the egress gateway's
// mitm_listener. It logs through slog's default logger, which cmd/extprocd
// configures at startup.
type Server struct{}

// NewServer builds the service.
func NewServer() *Server { return &Server{} }

// Evaluate decides whether one tunneled request may leave. This is the one
// place egress policy lives; everything around it is plumbing.
//
// It permits everything today, and that is not the absence of a policy: the
// gateway already refuses anything that is not a running actor at the CONNECT
// checkpoint, and the TLS leg's destinations are bounded by what the pod's
// network permits. What this declines to add is a second, hostname-level
// allowlist — so that turning one on is a visible change here rather than
// something this binary did by existing.
//
// Keep it fast. The filter is configured failure_mode_allow: false with a
// message timeout, so an Evaluate that blocks does not delay a request, it
// denies it — and denies every other request in flight behind it if the block
// is on shared state.
func (s *Server) Evaluate(_ context.Context, _ *Request) Decision {
	return Allow()
}

// Process implements the ExternalProcessor service.
//
// Envoy opens one stream per intercepted request and, with the headers-only
// processing_mode this filter is configured with, sends exactly one message on
// it. Every message must be answered with a response of the matching kind:
// Envoy treats a mismatched or extra response as a protocol error and fails the
// request with a 500, which under failure_mode_allow: false is a denial.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		var resp *extprocv3.ProcessingResponse

		switch reqType := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			resp = s.authorize(stream.Context(), req.Attributes, reqType.RequestHeaders)
		default:
			// No modification for other processing states,
			// but log because this should not be called.
			slog.ErrorContext(stream.Context(), "unexpected ext_proc message kind",
				slog.String("message_kind", fmt.Sprintf("%T", reqType)))
			resp = &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extprocv3.HeadersResponse{
						Response: &extprocv3.CommonResponse{},
					},
				},
			}
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *Server) authorize(ctx context.Context, attr map[string]*structpb.Struct, reqHeaders *extprocv3.HttpHeaders) *extprocv3.ProcessingResponse {
	req := requestFromHeaders(reqHeaders)
	req.Actor = actorFromAttributes(attr)
	req.Atespace, req.ActorName = parseActor(req.Actor)

	decision := s.Evaluate(ctx, req)

	if !decision.Allow {
		// Headers at info level, unlike the allow path: a denial is the case
		// someone has to explain afterwards, and the header set is most of the
		// explanation. This is the one log line here whose volume tracks
		// misbehavior rather than traffic.
		slog.InfoContext(ctx, "egress denied",
			slog.String("authority", req.Authority),
			slog.String("method", req.Method),
			slog.String("path", req.Path),
			slog.String("actor", req.Actor),
			slog.String("atespace", req.Atespace),
			slog.String("actor_name", req.ActorName),
			slog.String("reason", decision.Reason),
			slog.Any("headers", req.Headers),
		)
		return denyResponse()
	}

	slog.DebugContext(ctx, "egress allowed",
		slog.String("authority", req.Authority),
		slog.String("method", req.Method),
		slog.String("path", req.Path),
		slog.String("actor", req.Actor),
		slog.String("atespace", req.Atespace),
		slog.String("actor_name", req.ActorName),
		// The header name, never the value. An injected credential that is
		// also in the gateway's logs is a credential in the log pipeline.
		slog.String("credential_header", decision.Credential.header()),
		// These are the headers as they arrived, before any injection: the
		// mutation is applied by Envoy after this returns, so a credential
		// this request is about to be given cannot appear here.
		slog.Any("headers", req.Headers),
	)
	return s.allowResponse(ctx, decision)
}

// header is the credential's header name, or "" for no credential. Defined on
// the pointer so a nil Credential -- the shipped case -- reads without a guard
// at every call site.
func (c *Credential) header() string {
	if c == nil {
		return ""
	}
	return c.Name
}

// allowResponse passes the request upstream, carrying whatever credential the
// decision asked to inject.
func (s *Server) allowResponse(ctx context.Context, decision Decision) *extprocv3.ProcessingResponse {
	common := &extprocv3.CommonResponse{}

	// Anything other than a plain header mutation is dropped rather than sent.
	// The filter's mutation rules do permit ordinary header mutations -- that
	// headroom is what makes this the credential-injection seam -- so a
	// mutation returned here is a mutation Envoy actually applies.
	if name := decision.Credential.header(); name != "" {
		if strings.HasPrefix(name, ":") {
			// Envoy's default mutation rules already refuse this, and
			// allow_all_routing is false on the filter, but that refusal is a
			// silent no-op by default. Catch it here so a policy that tries to
			// rewrite :authority fails loudly instead of appearing to work.
			slog.ErrorContext(ctx, "refusing to inject a credential into a pseudo-header",
				slog.String("credential_header", name))
		} else {
			common.HeaderMutation = &extprocv3.HeaderMutation{
				SetHeaders: []*corev3.HeaderValueOption{{
					// RawValue rather than Value: newer Envoy drops Value.
					Header: &corev3.HeaderValue{Key: name, RawValue: []byte(decision.Credential.Value)},
					// OVERWRITE, not the APPEND_IF_EXISTS_OR_ADD default. The
					// actor sets its own headers inside its own tunnel, so an
					// append would leave the actor's forged value beside the
					// real one and let the upstream pick.
					AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
				}},
			}
		}
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{Response: common},
		},
	}
}

// requestFromHeaders projects the ext_proc header map onto a Request.
func requestFromHeaders(headers *extprocv3.HttpHeaders) *Request {
	all := make(map[string]string)
	for _, header := range headers.GetHeaders().GetHeaders() {
		all[strings.ToLower(header.GetKey())] = headerValue(header)
	}
	return &Request{
		Method:    all[":method"],
		Authority: authorityOf(all),
		Path:      all[":path"],
		Scheme:    all[":scheme"],
		Headers:   all,
	}
}

// actorFilterStateName is the filter-state object the gateway publishes the
// calling actor under. Filter state, not a header: the actor writes the bytes
// inside its own tunnel, so a header naming the caller is an assertion by the
// thing being policed.
const actorFilterStateName = "ate.actor"

// actorAttribute is the CEL expression the gateway's ext_proc filter has to
// list under request_attributes, and also the key the value comes back under —
// Envoy keys the attributes struct by each expression string exactly as
// configured, which is why the constant carries the brackets and quotes.
const actorAttribute = "filter_state['" + actorFilterStateName + "']"

// actorFromAttributes returns the actor the gateway asserted in filter state,
// or "" when it asserted none.
//
// The outer map is keyed by the ext_proc filter's name within the HCM chain.
// Hardcoding that name would couple this server to the gateway's Envoy config,
// so scan every entry instead — the same reasoning as filterChainName in
// cmd/atenet/internal/router/extproc.
//
// The bare name is accepted alongside the bracketed expression only so that
// changing the listener's request_attributes to an equivalent spelling does not
// silently start returning "". The bracketed form is the one mitm_listener
// actually sends, verified end to end against Envoy 1.37 on a live gateway.
func actorFromAttributes(attr map[string]*structpb.Struct) string {
	for _, attrs := range attr {
		fields := attrs.GetFields()
		if v, ok := fields[actorAttribute]; ok {
			return v.GetStringValue()
		}
		if v, ok := fields[actorFilterStateName]; ok {
			return v.GetStringValue()
		}
	}
	return ""
}

// actorURIPrefix is everything before the atespace in an actor's SPIFFE ID.
// The trust domain is part of it on purpose: it is what separates an actor
// certificate from every other identity in the cluster, so matching without it
// would accept a name-shaped path under some other authority. Built to match
// cmd/ateapi/internal/actoridentity, which mints these.
const actorURIPrefix = "spiffe://substrate-actor.local/atespace/"

// actorSeparator divides the atespace from the actor name.
const actorSeparator = "/actor/"

// parseActor splits an actor's SPIFFE ID into its atespace and name, returning
// "", "" for anything that is not exactly one.
//
// Deliberately not url.Parse: this is a fixed shape, not a URL to interpret,
// and cutting two constant strings is both ~20x cheaper per request and
// stricter — url.Parse accepts percent-encoding, query strings, and userinfo
// that would make two different strings compare equal after decoding.
//
// Rejecting rather than truncating a path with extra segments matters. An
// identity this does not fully understand must not be reduced to one it does,
// because the caller's next move is to decide policy on the result.
func parseActor(uri string) (atespace, name string) {
	rest, ok := strings.CutPrefix(uri, actorURIPrefix)
	if !ok {
		return "", ""
	}
	atespace, name, ok = strings.Cut(rest, actorSeparator)
	if !ok || atespace == "" || name == "" {
		return "", ""
	}
	// Kubernetes object names cannot contain a slash, so anything further down
	// the path is a shape this does not know how to read.
	if strings.Contains(atespace, "/") || strings.Contains(name, "/") {
		return "", ""
	}
	return atespace, name
}

// authorityOf prefers :authority and falls back to host.
//
// The fallback is defensive, not load-bearing. Envoy's HTTP/1 codec folds Host
// into :authority before any http filter runs, so on both mitm_listener chains
// ext_proc sees :authority and no separate host header — verified against
// Envoy 1.37.5 with a cleartext HTTP/1.1 request. What the fallback buys is a
// failure mode: if a future gateway config or Envoy version ever delivers only
// host, this reads the destination instead of reading an empty string, and a
// hostname policy matching an empty destination allows everything.
func authorityOf(headers map[string]string) string {
	if authority := headers[":authority"]; authority != "" {
		return authority
	}
	return headers["host"]
}

// headerValue reads a header value, preferring raw_value.
//
// Recent Envoy sends values in HeaderValue.raw_value (bytes) and leaves
// HeaderValue.value (string) empty. A processor that reads only .Value gets an
// empty string for every header — which, for a policy keyed on the authority,
// is an empty destination rather than a visible error.
func headerValue(header *corev3.HeaderValue) string {
	if raw := header.GetRawValue(); len(raw) > 0 {
		return string(raw)
	}
	return header.GetValue()
}

// denyResponse tells Envoy to answer the request itself with a 403.
func denyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &envoy_type.HttpStatus{Code: envoy_type.StatusCode_Forbidden},
				Body:    []byte(denyBody),
				Details: DenyDetails,
				Headers: &extprocv3.HeaderMutation{
					SetHeaders: []*corev3.HeaderValueOption{{
						// RawValue rather than Value: newer Envoy drops Value.
						Header: &corev3.HeaderValue{Key: "content-type", RawValue: []byte("text/plain")},
					}},
				},
			},
		},
	}
}
