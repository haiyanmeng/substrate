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
	"net/netip"
	"strings"

	"github.com/agent-substrate/substrate/internal/atunnel"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// Header names, lowercased for map lookups. The three X-Ate-* names are taken
// from internal/atunnel rather than restated, so they cannot drift from what
// atunnel actually sends.
var (
	hdrAtespace     = strings.ToLower(atunnel.ActorAtespaceHeader)
	hdrActorName    = strings.ToLower(atunnel.ActorNameHeader)
	hdrActorVersion = strings.ToLower(atunnel.ActorVersionHeader)
)

const (
	// EgressModeHeader carries the CONNECT checkpoint's routing decision. Envoy's
	// route table matches on it.
	//
	// The value is always overwritten, never appended, so an actor that sets this
	// header itself cannot choose its own route. See setHeader.
	EgressModeHeader = "x-ate-egress-mode"

	// ActorKeyHeader carries "<atespace>/<name>" from the CONNECT checkpoint
	// onward, so the inner checkpoint does not have to re-derive the actor.
	//
	// It does NOT survive a CONNECT tunnel on its own: the tunnel body is opaque
	// bytes and the CONNECT's own headers are not replayed into it. Crossing that
	// boundary needs set_filter_state with shared_with_upstream: TRANSITIVE, which
	// egress-authn.md lists as the open runtime question. The header is set anyway
	// because it is free, it is correct on any non-tunneled hop, and the PoC
	// measures whether it arrives.
	ActorKeyHeader = "x-ate-actor-key"
)

// requestInfo is the parsed view of one ProcessingRequest's headers.
type requestInfo struct {
	headers   map[string]string
	method    string
	authority string
	// actor is the identity claimed by the request. It is a *claim*: see the
	// warning on Server.resolveActor.
	actor ActorKey
	// actorVersion is carried for logging only. It cannot signal a policy change,
	// because Egress.Activate freezes EgressMetadata once per activation
	// (internal/atunnel/egress.go:109-119), so every CONNECT from one activation
	// repeats the same value.
	actorVersion string
}

// newRequestInfo flattens Envoy's header list.
//
// Both Value and RawValue must be read: Envoy populates RawValue on newer
// builds and Value on older ones, and a filter that reads only one silently
// sees empty strings. cmd/atenet/internal/router/extproc_in.go:31-34 handles the
// same case.
func newRequestInfo(hs []*corev3.HeaderValue) *requestInfo {
	m := make(map[string]string, len(hs))
	for _, h := range hs {
		v := h.GetValue()
		if v == "" && len(h.GetRawValue()) > 0 {
			v = string(h.GetRawValue())
		}
		m[strings.ToLower(h.GetKey())] = v
	}

	authority := m[":authority"]
	if authority == "" {
		authority = m["host"]
	}

	return &requestInfo{
		headers:   m,
		method:    m[":method"],
		authority: authority,
		actor: ActorKey{
			Atespace: strings.TrimSpace(m[hdrAtespace]),
			Name:     strings.TrimSpace(m[hdrActorName]),
		},
		actorVersion: m[hdrActorVersion],
	}
}

// destination parses the CONNECT authority as ip:port.
//
// atunnel's validateDestination already requires an IP literal
// (internal/atunnel/client.go:197-213), so a hostname here is a protocol
// violation and the caller must fail closed rather than resolve it. Resolving
// would hand the actor a DNS-rebinding primitive against the policy check.
func (r *requestInfo) destination() (netip.AddrPort, bool) {
	ap, err := netip.ParseAddrPort(r.authority)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

// actorKeyFromHeader parses the "<atespace>/<name>" form set by the CONNECT
// checkpoint.
func actorKeyFromHeader(v string) (ActorKey, bool) {
	atespace, name, ok := strings.Cut(strings.TrimSpace(v), "/")
	if !ok {
		return ActorKey{}, false
	}
	k := ActorKey{Atespace: strings.TrimSpace(atespace), Name: strings.TrimSpace(name)}
	if k.Zero() {
		return ActorKey{}, false
	}
	return k, true
}

// setHeader appends an overwrite-or-add mutation.
//
// OVERWRITE_IF_EXISTS_OR_ADD is pinned rather than left to the default: the
// default is split across the deprecated `append` field and `append_action`,
// and an appended x-ate-egress-mode would let a caller-supplied value sit
// alongside ours and influence route matching.
func setHeader(mut *extprocv3.HeaderMutation, key, value string) {
	mut.SetHeaders = append(mut.SetHeaders, &corev3.HeaderValueOption{
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		Header: &corev3.HeaderValue{
			Key:      key,
			RawValue: []byte(value),
		},
	})
}

// removeHeader appends a removal, skipping empty names and pseudo-headers.
// Envoy ignores attempts to remove ":"-prefixed headers and host; filtering
// here keeps that from looking like it worked.
func removeHeader(mut *extprocv3.HeaderMutation, key string) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || strings.HasPrefix(key, ":") || key == "host" {
		return
	}
	mut.RemoveHeaders = append(mut.RemoveHeaders, key)
}

// continueResponse builds an allow with the given mutation.
//
// clearRouteCache is load-bearing at the CONNECT checkpoint and must be true
// whenever the mutation affects route selection. Envoy picks the route before
// applying ext_proc's mutation, so without it the x-ate-egress-mode match never
// fires and every request takes the fallback route — with a 200, no error, and
// no stat. Measured; see "Verified against Envoy 1.37" in egress-authn.md.
func continueResponse(mut *extprocv3.HeaderMutation, clearRouteCache bool) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					Status:          extprocv3.CommonResponse_CONTINUE,
					HeaderMutation:  mut,
					ClearRouteCache: clearRouteCache,
				},
			},
		},
	}
}

// denyResponse builds a 403 that terminates the request without establishing a
// tunnel. Verified to work on a CONNECT: the caller sees "403 Forbidden" and no
// tunnel is created.
func denyResponse(reason string) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
				Body:    []byte("egress denied: " + reason + "\n"),
				Details: sanitizeDetails(reason),
				Headers: &extprocv3.HeaderMutation{
					SetHeaders: []*corev3.HeaderValueOption{{
						Header: &corev3.HeaderValue{
							Key:      "content-type",
							RawValue: []byte("text/plain"),
						},
					}},
				},
			},
		},
	}
}

// sanitizeDetails makes a reason safe for %RESPONSE_CODE_DETAILS%, which is
// emitted into access logs as a bare token and must not contain whitespace.
func sanitizeDetails(reason string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, reason)
}
