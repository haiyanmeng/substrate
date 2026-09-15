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

// Package matrixapi is the wire contract between the egress protocol matrix
// suite and the probe it drives inside an actor: one case in, one outcome back.
// It is its own package so both ends share the types rather than each keeping a
// copy that can drift -- a mismatched field name would otherwise decode to a
// zero value and read as a protocol result rather than as a typo.
package matrixapi

// Kinds of case the probe knows how to run.
const (
	// KindHTTP makes a plain request.
	KindHTTP = "http"
	// KindConnect sends a tunneling CONNECT of the workload's own, nested
	// inside the tunnel atunnel already opened.
	KindConnect = "connect"
	// KindWebsocket runs a WebSocket handshake and one echo.
	KindWebsocket = "ws"
	// KindUDP sends one datagram and waits for an answer.
	KindUDP = "udp"
	// KindDNS resolves a name, optionally against a named resolver.
	KindDNS = "dns"
)

// ProbeRequest is one case of the matrix.
type ProbeRequest struct {
	// Kind picks the protocol shape; see the Kind constants.
	Kind string `json:"kind"`
	// Target is the address to dial, as host:port. For KindDNS it is the
	// resolver to query, and empty means the actor's configured resolver.
	Target string `json:"target"`
	// Tunnel is the authority a KindConnect request asks for, which is the
	// address the origin's forward proxy would dial on the probe's behalf.
	// Empty means Target.
	Tunnel string `json:"tunnel"`
	// HTTP is the version to speak: "1.1" or "2". For KindConnect and
	// KindWebsocket this is the version of the connection carrying the tunnel,
	// so "2" means an HTTP/2 CONNECT stream -- and for a WebSocket, the RFC
	// 8441 extended CONNECT.
	HTTP string `json:"http"`
	// TLS wraps the connection, with ALPN set from HTTP.
	TLS bool `json:"tls"`
	// Path is the request path, defaulting to /echo for KindHTTP and /ws for
	// KindWebsocket.
	Path string `json:"path"`
	// Network forces a transport for KindDNS: "udp" or "tcp".
	Network string `json:"network"`
	// Name is the hostname a KindDNS case resolves.
	Name string `json:"name"`
	// Payload is the datagram a KindUDP case sends. Empty sends a QUIC Initial
	// packet, which is what a real QUIC client opens with.
	Payload string `json:"payload"`
	// TimeoutSeconds bounds the case. Zero takes the probe's default.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// ProbeResult is what one case produced, in the terms the report is written in:
// a status code and a body when something on the egress path answered, an error
// when it refused or dropped.
type ProbeResult struct {
	// OK is whether the case got all the way through to the origin.
	OK bool `json:"ok"`
	// Status is the HTTP status the probe saw, which for a denied case is the
	// gateway's rather than the origin's. Zero when nothing answered.
	Status int `json:"status,omitempty"`
	// Proto is the protocol the response arrived on.
	Proto string `json:"proto,omitempty"`
	// Body is the response body, truncated. For a denial this is the message.
	Body string `json:"body,omitempty"`
	// Detail carries whatever else the case proved: the origin's echo of how
	// the request reached it, the addresses a lookup returned, the first line
	// read back through an established tunnel.
	Detail string `json:"detail,omitempty"`
	// Error is the client-side failure, and ErrorType its Go type, which is how
	// a timeout is told from a reset without parsing prose.
	Error     string `json:"error,omitempty"`
	ErrorType string `json:"errorType,omitempty"`
	// ElapsedMs separates an immediate refusal from a drop that timed out.
	ElapsedMs int64 `json:"elapsedMs"`
}
