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
// checkpoint: the filter that runs on a tunnelled request after Envoy has
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
//     the workload being policed. A Policy must not read one.
//
// Identity therefore has to cross as filter state (filter_state['ate.actor']),
// which is not wired yet. Until it is, a Policy can decide only on where the
// request is going, not on who is sending it.
//
// The one input here the actor cannot forge is the filter chain name Envoy
// attaches as an attribute. Everything else — including :scheme, which Envoy
// derives from the client's own x-forwarded-proto on a plaintext connection —
// is downstream of bytes the workload wrote. See TLSFilterChainName.
package inner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoy_type "github.com/envoyproxy/go-control-plane/envoy/type/v3"
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

// TLSFilterChainName is mitm_listener's TLS chain: the leg where Envoy minted
// a leaf for the SNI, terminated the actor's TLS, and re-originates over TLS
// through egress_forward_proxy. Must match the filter chain name in
// manifests/ate-install/atenet-egress.yaml, which also has to ask for the
// attribute via request_attributes.
//
// This is the only trustworthy statement of "this request will leave the pod
// encrypted", and credential injection depends on it. The obvious alternative,
// req.Scheme == "https", is not one: on the cleartext chain Envoy derives
// :scheme from x-forwarded-proto, which the actor sets inside its own tunnel.
// Verified against Envoy 1.37.5 — a plaintext request carrying
// "X-Forwarded-Proto: https" arrives at ext_proc as :scheme https, while the
// chain name still reads mitm_cleartext.
//
// Renaming the chain in the manifest without updating this fails closed: the
// name stops matching, and nothing is injected. So does forgetting
// request_attributes, which makes the attribute absent and the name empty.
const TLSFilterChainName = "mitm_tls"

// filterChainNameAttribute is the CEL attribute carrying the chain name. Same
// value as extproc.FilterChainNameAttribute in the CONNECT checkpoint, not
// imported from it: that package is the atenet router's, and a dependency from
// this binary to it would pull the whole ate API client in for one string.
const filterChainNameAttribute = "xds.filter_chain_name"

// authorizationHeader is the header credential injection writes.
const authorizationHeader = "authorization"

// githubToken is the GitHub personal access token extprocd presents to GitHub
// on an actor's behalf, so that the actor never holds it: the workload makes an
// unauthenticated request and the gateway adds the credential on the decrypted
// leg, where the actor cannot read it back.
//
// HARDCODED, AND EMPTY IN THE REPOSITORY. Paste a PAT here to demo it, and do
// not commit that. An empty value is not a broken config — GitHubToken returns
// a no-op injector, so a fresh checkout adds no header at all rather than
// sending "Bearer " to github.com.
//
// This is the wrong place for a real deployment and it is worth saying why,
// because the failure is quiet. A token in the source is in every clone, every
// image layer, and every fork; it cannot be rotated without a rebuild; and it
// is the same token for every actor, so the gateway's access log is the only
// record of who used it. The production answer is a mounted Secret read at
// request time, which rotates in place and can be scoped per actor once
// filter_state['ate.actor'] is wired.
const githubToken = ""

// githubTokenHosts are the only destinations the token is ever sent to.
//
// Exact names, not a suffix match on "github.com". A suffix match would also
// cover pages.github.com and every user-controlled *.github.io style name that
// resolves under it, and the cost of being wrong here is not a failed request,
// it is a PAT handed to a host that asked for one.
var githubTokenHosts = map[string]struct{}{
	"api.github.com": {},
	"github.com":     {},
}

// Request is everything the inner checkpoint can see about one tunnelled
// request. Constructed from the ext_proc RequestHeaders message; a Policy gets
// this rather than the protobuf so it cannot accidentally depend on the parts
// of the message that are empty here (attributes, metadata_context, bodies).
type Request struct {
	// Method is the real verb, not CONNECT.
	Method string

	// Authority is the destination as the tunnelled request itself named it,
	// possibly with a port. Use Host to match on the name alone.
	//
	// This is the name that gets dialled: dynamic_forward_proxy runs after this
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
	Headers map[string]string

	// FilterChain is the mitm_listener chain Envoy accepted this request on,
	// read from the xds.filter_chain_name attribute. Empty when the gateway did
	// not ask for the attribute.
	//
	// The only field here the actor cannot influence, and therefore the only
	// one safe to gate a credential on. Compare against TLSFilterChainName.
	FilterChain string
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

// Decision is a Policy's answer for one request.
type Decision struct {
	// Allow passes the request to dynamic_forward_proxy and then upstream.
	Allow bool

	// Reason explains a denial in the gateway's logs. It is never sent to the
	// actor; see denyBody.
	Reason string
}

// Allow permits a request.
func Allow() Decision { return Decision{Allow: true} }

// Deny refuses a request. reason is logged, not returned to the caller.
func Deny(reason string) Decision { return Decision{Reason: reason} }

// Policy decides whether one tunnelled request may leave.
//
// Evaluate must be fast. The filter is configured failure_mode_allow: false
// with a message timeout, so a Policy that blocks does not delay a request, it
// denies it — and denies every other request in flight behind it if the block
// is shared state.
type Policy interface {
	Evaluate(ctx context.Context, req *Request) Decision
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, req *Request) Decision

// Evaluate implements Policy.
func (f PolicyFunc) Evaluate(ctx context.Context, req *Request) Decision {
	return f(ctx, req)
}

// AllowAll permits every request.
//
// This is the shipped default, and it is not the absence of a policy: the
// gateway already refuses anything that is not a running actor at the CONNECT
// checkpoint, and the TLS leg's destinations are bounded by what the pod's
// network permits. What AllowAll declines to add is a second, hostname-level
// allowlist — so that turning one on is a visible configuration change rather
// than something this binary did by existing.
func AllowAll() Policy {
	return PolicyFunc(func(context.Context, *Request) Decision { return Allow() })
}

// DenyHosts refuses requests whose Host matches one of hosts exactly, and
// permits everything else. Matching is case-insensitive and ignores the port.
//
// A denylist rather than an allowlist on purpose: an allowlist that is easy to
// configure is easy to configure incompletely, and an incomplete one here does
// not fail visibly at startup, it fails as a broken actor days later. When
// hostname allowlisting lands it should be driven by per-actor policy resolved
// from identity, not by a flag on the processor.
func DenyHosts(hosts []string) (Policy, error) {
	denied := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		normalized := strings.ToLower(strings.TrimSpace(host))
		if normalized == "" {
			return nil, fmt.Errorf("empty hostname in denylist")
		}
		if strings.ContainsAny(normalized, "/: ") {
			return nil, fmt.Errorf("hostname %q must be a bare hostname, with no scheme, port, or path", host)
		}
		denied[strings.TrimSuffix(normalized, ".")] = struct{}{}
	}
	return PolicyFunc(func(_ context.Context, req *Request) Decision {
		if _, ok := denied[req.Host()]; ok {
			return Deny("host is in the denylist")
		}
		return Allow()
	}), nil
}

// Header is one request header extprocd sets before the request is forwarded.
type Header struct {
	Name  string
	Value string
}

// Injector adds credentials to a request the Policy already allowed.
//
// Separate from Policy on purpose. A Policy answers whether a request may
// leave; an Injector answers what the gateway adds to it on the way out. Fusing
// them would put the credential on the deny path, where the one thing that must
// never happen is a token reaching a destination the policy just refused.
//
// An Injector must return nothing unless it is certain of the destination AND
// of the leg. See GitHubToken for what "certain" costs.
type Injector interface {
	Inject(ctx context.Context, req *Request) []Header
}

// InjectorFunc adapts a function to Injector.
type InjectorFunc func(ctx context.Context, req *Request) []Header

// Inject implements Injector.
func (f InjectorFunc) Inject(ctx context.Context, req *Request) []Header {
	return f(ctx, req)
}

// NoInjection adds nothing to any request.
func NoInjection() Injector {
	return InjectorFunc(func(context.Context, *Request) []Header { return nil })
}

// GitHubToken returns the injector for the hardcoded githubToken, or
// NoInjection when that constant is empty.
func GitHubToken() Injector { return githubTokenInjector(githubToken) }

// GitHubTokenConfigured reports whether a token is compiled in. Only so the
// startup log can say so: "no Authorization header appeared" and "the constant
// is empty" look identical from the actor's side, and from GitHub's.
func GitHubTokenConfigured() bool { return strings.TrimSpace(githubToken) != "" }

// githubTokenInjector is GitHubToken with the token as a parameter, so tests
// can exercise it without a real credential in the repository.
//
// Two conditions, both required, neither sufficient:
//
//   - The request arrived on TLSFilterChainName. On the cleartext chain the
//     request re-originates through egress_forward_proxy_cleartext, which has
//     no upstream TLS — injecting there would put the PAT on the wire in the
//     clear, to a destination the actor chose.
//   - The destination is in githubTokenHosts, matched on Host so that a port or
//     a difference in case cannot dodge it.
//
// The header is set, not appended, so an Authorization the actor wrote inside
// its own tunnel is replaced rather than joined. Appending would let the actor
// prepend a credential of its own and take its chances on which one GitHub
// reads first.
func githubTokenInjector(token string) Injector {
	token = strings.TrimSpace(token)
	if token == "" {
		return NoInjection()
	}
	value := "Bearer " + token
	return InjectorFunc(func(_ context.Context, req *Request) []Header {
		if req.FilterChain != TLSFilterChainName {
			return nil
		}
		if _, ok := githubTokenHosts[req.Host()]; !ok {
			return nil
		}
		return []Header{{Name: authorizationHeader, Value: value}}
	})
}

// Server is the ext_proc gRPC service extprocd serves to the egress gateway's
// mitm_listener.
type Server struct {
	policy   Policy
	injector Injector
	logger   *slog.Logger
}

// NewServer builds the service. A nil injector injects nothing; a nil logger
// discards.
func NewServer(policy Policy, injector Injector, logger *slog.Logger) *Server {
	if injector == nil {
		injector = NoInjection()
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{policy: policy, injector: injector, logger: logger}
}

// Process implements the ExternalProcessor service.
//
// Envoy opens one stream per intercepted request and, with the headers-only
// processing_mode this filter is configured with, sends exactly one message on
// it. Every message must be answered with a response of the matching kind:
// Envoy treats a mismatched or extra response as a protocol error and fails the
// request with a 500, which under failure_mode_allow: false is a denial.
func (s *Server) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		resp, err := s.respond(ctx, req)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// respond builds the answer to one ProcessingRequest.
func (s *Server) respond(ctx context.Context, req *extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, error) {
	switch msg := req.GetRequest().(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		return s.processRequestHeaders(ctx, msg.RequestHeaders, filterChainName(req)), nil

	// The remaining kinds are all configured SKIP or NONE on the filter, so
	// none of them should arrive. Answer each with the matching empty response
	// anyway rather than falling through to a RequestHeaders response: the
	// mismatch would fail the request, which is a strange way to find out the
	// processing_mode drifted.
	case *extprocv3.ProcessingRequest_ResponseHeaders:
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}},
		}}, nil
	case *extprocv3.ProcessingRequest_RequestBody:
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
		}}, nil
	case *extprocv3.ProcessingRequest_ResponseBody:
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
		}}, nil
	case *extprocv3.ProcessingRequest_RequestTrailers:
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestTrailers{
			RequestTrailers: &extprocv3.TrailersResponse{},
		}}, nil
	case *extprocv3.ProcessingRequest_ResponseTrailers:
		return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extprocv3.TrailersResponse{},
		}}, nil

	default:
		// Ending the stream is the fail-closed answer: Envoy reports a stream
		// closed without a response as a 500, and this is a message kind the
		// server does not know how to answer safely.
		return nil, fmt.Errorf("extprocd: unexpected ext_proc message %T", msg)
	}
}

// processRequestHeaders runs one request past the policy, then past the
// injector.
//
// Policy first. A denied request never reaches the injector, so there is no
// ordering in which a credential is attached to a request that is about to be
// refused.
func (s *Server) processRequestHeaders(ctx context.Context, headers *extprocv3.HttpHeaders, filterChain string) *extprocv3.ProcessingResponse {
	req := requestFromHeaders(headers, filterChain)
	decision := s.policy.Evaluate(ctx, req)

	if !decision.Allow {
		s.logger.InfoContext(ctx, "egress denied",
			slog.String("authority", req.Authority),
			slog.String("method", req.Method),
			slog.String("path", req.Path),
			slog.String("reason", decision.Reason),
		)
		return denyResponse()
	}

	s.logger.DebugContext(ctx, "egress allowed",
		slog.String("authority", req.Authority),
		slog.String("method", req.Method),
		slog.String("path", req.Path),
	)

	common := &extprocv3.CommonResponse{}
	if injected := s.injector.Inject(ctx, req); len(injected) > 0 {
		common.HeaderMutation = &extprocv3.HeaderMutation{SetHeaders: setHeaders(injected)}
		// Names only. The value is the credential, and an access log is a much
		// less guarded artifact than the Secret it came from.
		s.logger.InfoContext(ctx, "credential injected",
			slog.String("host", req.Host()),
			slog.String("chain", req.FilterChain),
			slog.Any("headers", headerNames(injected)),
		)
	}
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{Response: common},
		},
	}
}

// setHeaders converts injected headers to the ext_proc mutation.
//
// OVERWRITE_IF_EXISTS_OR_ADD is load-bearing and is not the default: the
// zero AppendAction is APPEND_IF_EXISTS_OR_ADD, which on a request that already
// carries an actor-written Authorization produces two values rather than one
// and leaves which one the upstream honours up to the upstream.
func setHeaders(headers []Header) []*corev3.HeaderValueOption {
	options := make([]*corev3.HeaderValueOption, 0, len(headers))
	for _, header := range headers {
		options = append(options, &corev3.HeaderValueOption{
			// RawValue rather than Value, matching denyResponse: newer Envoy
			// drops Value.
			Header:       &corev3.HeaderValue{Key: header.Name, RawValue: []byte(header.Value)},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return options
}

// headerNames lists the injected header names for the log.
func headerNames(headers []Header) []string {
	names := make([]string, 0, len(headers))
	for _, header := range headers {
		names = append(names, header.Name)
	}
	return names
}

// requestFromHeaders projects the ext_proc header map onto a Request.
func requestFromHeaders(headers *extprocv3.HttpHeaders, filterChain string) *Request {
	all := make(map[string]string)
	for _, header := range headers.GetHeaders().GetHeaders() {
		all[strings.ToLower(header.GetKey())] = headerValue(header)
	}
	return &Request{
		Method:      all[":method"],
		Authority:   authorityOf(all),
		Path:        all[":path"],
		Scheme:      all[":scheme"],
		Headers:     all,
		FilterChain: filterChain,
	}
}

// filterChainName returns the xds.filter_chain_name attribute Envoy attached,
// or "" when the gateway did not request it. The attributes map is keyed by the
// ext_proc filter's name within the HCM chain, which this binary should not
// have to know, so scan every entry.
func filterChainName(req *extprocv3.ProcessingRequest) string {
	for _, attributes := range req.GetAttributes() {
		if value, ok := attributes.GetFields()[filterChainNameAttribute]; ok {
			return value.GetStringValue()
		}
	}
	return ""
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
