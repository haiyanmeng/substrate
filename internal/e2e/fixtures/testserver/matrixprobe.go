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

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e/fixtures/testserver/matrixapi"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// matrixprobe runs inside the actor and is the client half of the egress
// protocol matrix. It takes one case per request -- a protocol shape and a
// target -- makes exactly that call, and reports what came back, including the
// status code and body of a denial. Driving it over HTTP rather than baking the
// matrix into the binary is what lets the suite re-run the same cases against a
// changed EgressPolicy without redeploying the actor.

// probeDefaultTimeout bounds a case that neither succeeds nor fails outright.
// It has to outlast a denial (immediate) and a refused connection (immediate)
// while still catching a silent drop, which is what UDP looks like from here.
const probeDefaultTimeout = 10 * time.Second

// bodyLimit truncates what a probe reports. Denial bodies are a few words;
// anything long is an origin's page, which is not what the matrix records.
const bodyLimit = 512

func newMatrixProbeCmd() *cobra.Command {
	var listenAddress string
	cmd := &cobra.Command{
		Use:   "matrixprobe",
		Short: "Run one egress protocol case per request and report what came back.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveMatrixProbe(cmd.Context(), listenAddress)
		},
	}
	cmd.Flags().StringVar(&listenAddress, "listen", ":80", "Address the probe API listens on.")
	return cmd
}

func serveMatrixProbe(ctx context.Context, listenAddress string) error {
	mux := http.NewServeMux()
	// Both names: an actor template's readiness probe asks for /readyz, and the
	// shared server manifest asks for /healthz.
	ready := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}
	mux.HandleFunc("/readyz", ready)
	mux.HandleFunc("/healthz", ready)
	mux.HandleFunc("/probe", handleProbe)

	server := &http.Server{Addr: listenAddress, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	log.Printf("testserver matrixprobe: listening on %s", listenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving the probe API: %w", err)
	}
	return nil
}

func handleProbe(w http.ResponseWriter, r *http.Request) {
	var request matrixapi.ProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "decoding the probe request: "+err.Error(), http.StatusBadRequest)
		return
	}
	timeout := probeDefaultTimeout
	if request.TimeoutSeconds > 0 {
		timeout = time.Duration(request.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	started := time.Now()
	result, err := runProbe(ctx, request)
	result.ElapsedMs = time.Since(started).Milliseconds()
	if err != nil {
		result.OK = false
		result.Error = err.Error()
		result.ErrorType = fmt.Sprintf("%T", errors.Unwrap(err))
		if result.ErrorType == "<nil>" {
			result.ErrorType = fmt.Sprintf("%T", err)
		}
	}
	log.Printf("testserver matrixprobe: %s %s http/%s tls=%v -> ok=%v status=%d body=%q err=%q",
		request.Kind, request.Target, request.HTTP, request.TLS, result.OK, result.Status, result.Body, result.Error)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func runProbe(ctx context.Context, request matrixapi.ProbeRequest) (matrixapi.ProbeResult, error) {
	switch request.Kind {
	case matrixapi.KindHTTP:
		return probeHTTP(ctx, request)
	case matrixapi.KindConnect:
		return probeConnect(ctx, request)
	case matrixapi.KindWebsocket:
		return probeWebsocket(ctx, request)
	case matrixapi.KindUDP:
		return probeUDP(ctx, request)
	case matrixapi.KindDNS:
		return probeDNS(ctx, request)
	default:
		return matrixapi.ProbeResult{}, fmt.Errorf("unknown probe kind %q", request.Kind)
	}
}

// probeHTTP makes a plain request, which is the case the egress gateway has the
// most to say about: a cleartext one is a request the policy sees whole, and a
// TLS one is an opaque stream it only knows the address of.
func probeHTTP(ctx context.Context, request matrixapi.ProbeRequest) (matrixapi.ProbeResult, error) {
	path := request.Path
	if path == "" {
		path = "/echo"
	}
	scheme := "http"
	if request.TLS {
		scheme = "https"
	}
	target := &url.URL{Scheme: scheme, Host: request.Target, Path: path}

	client := &http.Client{Transport: transportFor(request)}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	defer response.Body.Close()

	body := readLimited(response.Body)
	return matrixapi.ProbeResult{
		OK:     response.StatusCode == http.StatusOK,
		Status: response.StatusCode,
		Proto:  response.Proto,
		Body:   body,
		Detail: body,
	}, nil
}

// transportFor builds the client for a plain request in the shape the case
// asks for. HTTP/2 without TLS is prior-knowledge h2c: no Upgrade dance, the
// preface straight down the connection, which is the only form of cleartext
// HTTP/2 the gateway's inner listener accepts.
func transportFor(request matrixapi.ProbeRequest) http.RoundTripper {
	if request.HTTP == "2" {
		transport := &http2.Transport{TLSClientConfig: clientTLSConfig("h2")}
		if !request.TLS {
			transport.AllowHTTP = true
			transport.DialTLSContext = func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
		}
		return transport
	}
	return &http.Transport{
		TLSClientConfig:   clientTLSConfig("http/1.1"),
		ForceAttemptHTTP2: false,
		Proxy:             nil,
	}
}

// probeConnect sends a tunneling CONNECT of its own to the origin's forward
// proxy. This is not the CONNECT atunnel sends to the egress gateway -- that
// one is below the actor and invisible to it -- but a CONNECT made by the
// workload, nested inside the tunnel, and so a request the gateway's per-request
// policy hook may or may not accept.
func probeConnect(ctx context.Context, request matrixapi.ProbeRequest) (matrixapi.ProbeResult, error) {
	authority := request.Tunnel
	if authority == "" {
		authority = request.Target
	}
	conn, err := dial(ctx, request)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if request.HTTP == "2" {
		return connectOverHTTP2(ctx, conn, request, authority, "")
	}
	return connectOverHTTP1(ctx, conn, authority)
}

// connectOverHTTP1 writes the CONNECT by hand rather than through
// http.Transport, because the status line and body of a refusal are the point:
// a Transport turns both into one opaque error.
func connectOverHTTP1(_ context.Context, conn net.Conn, authority string) (matrixapi.ProbeResult, error) {
	line := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", authority, authority)
	if _, err := io.WriteString(conn, line); err != nil {
		return matrixapi.ProbeResult{}, err
	}
	buffered := bufio.NewReader(conn)
	response, err := http.ReadResponse(buffered, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	result := matrixapi.ProbeResult{Status: response.StatusCode, Proto: response.Proto}
	if response.StatusCode != http.StatusOK {
		result.Body = readLimited(response.Body)
		return result, nil
	}
	result.OK = true
	result.Detail = probeThroughTunnel(buffered, conn, authority)
	return result, nil
}

// connectOverHTTP2 opens a CONNECT stream. With protocol empty this is an
// ordinary tunnel, whose pseudo-headers are ":method" and ":authority" alone;
// with protocol set it is the RFC 8441 extended CONNECT that carries a
// WebSocket, which adds ":scheme", ":path" and ":protocol".
func connectOverHTTP2(ctx context.Context, conn net.Conn, request matrixapi.ProbeRequest, authority, protocol string) (matrixapi.ProbeResult, error) {
	client, err := newH2Client(conn)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	defer client.Close()

	fields := []hpack.HeaderField{
		{Name: ":method", Value: http.MethodConnect},
		{Name: ":authority", Value: authority},
	}
	if protocol != "" {
		path := request.Path
		if path == "" {
			path = "/ws"
		}
		scheme := "http"
		if request.TLS {
			scheme = "https"
		}
		fields = append(fields,
			hpack.HeaderField{Name: ":scheme", Value: scheme},
			hpack.HeaderField{Name: ":path", Value: path},
			hpack.HeaderField{Name: ":protocol", Value: protocol},
		)
	}

	// Whether the peer advertised extended CONNECT is half the answer for a
	// WebSocket over HTTP/2: a hop that never sets the setting has already
	// decided the case before the stream is opened, so it is reported whether
	// the stream went on to succeed or not.
	advertised := ""

	headers, err := client.openStream(ctx, fields)
	if protocol != "" {
		advertised = fmt.Sprintf("peer advertised SETTINGS_ENABLE_CONNECT_PROTOCOL=%v; ", client.extendedConnect)
	}
	if err != nil {
		return matrixapi.ProbeResult{Detail: advertised}, err
	}
	status, err := strconv.Atoi(headers.PseudoValue("status"))
	if err != nil {
		return matrixapi.ProbeResult{}, fmt.Errorf("the peer answered with no usable :status: %w", err)
	}

	result := matrixapi.ProbeResult{Status: status, Proto: "HTTP/2.0", Detail: advertised}
	if status != http.StatusOK {
		result.Body = readLimited(client)
		return result, nil
	}
	result.OK = true
	if protocol != "" {
		result.Detail += echoOverStream(client, client)
		return result, nil
	}
	result.Detail = probeThroughTunnel(bufio.NewReader(client), client, authority)
	return result, nil
}

// probeThroughTunnel proves an established tunnel actually carries traffic, by
// asking the far end for /healthz and reporting its status line. A CONNECT that
// is answered 200 and then carries nothing would otherwise read as a success.
func probeThroughTunnel(reader *bufio.Reader, writer io.Writer, authority string) string {
	request := fmt.Sprintf("GET /healthz HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", authority)
	if _, err := io.WriteString(writer, request); err != nil {
		return "writing through the tunnel: " + err.Error()
	}
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return "reading through the tunnel: " + err.Error()
	}
	return "tunnel carried: " + strings.TrimSpace(statusLine)
}

// probeWebsocket runs the handshake and one echo. Over HTTP/1.1 that is the
// RFC 6455 Upgrade; over HTTP/2 it is an extended CONNECT, which the origin has
// to be running with GODEBUG=http2xconnect=1 to accept.
func probeWebsocket(ctx context.Context, request matrixapi.ProbeRequest) (matrixapi.ProbeResult, error) {
	if request.HTTP == "2" {
		conn, err := dial(ctx, request)
		if err != nil {
			return matrixapi.ProbeResult{}, err
		}
		defer conn.Close()
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		return connectOverHTTP2(ctx, conn, request, request.Target, wsProtocol)
	}

	path := request.Path
	if path == "" {
		path = "/ws"
	}
	scheme := "ws"
	if request.TLS {
		scheme = "wss"
	}
	dialer := &websocket.Dialer{
		TLSClientConfig:  clientTLSConfig("http/1.1"),
		HandshakeTimeout: probeDefaultTimeout,
	}
	target := &url.URL{Scheme: scheme, Host: request.Target, Path: path}
	conn, response, err := dialer.DialContext(ctx, target.String(), nil)
	if err != nil {
		// A rejected handshake comes back as an error plus the response that
		// rejected it, and that response is the denial the matrix is after.
		if response != nil {
			defer response.Body.Close()
			return matrixapi.ProbeResult{
				Status: response.StatusCode,
				Proto:  response.Proto,
				Body:   readLimited(response.Body),
			}, err
		}
		return matrixapi.ProbeResult{}, err
	}
	defer conn.Close()

	result := matrixapi.ProbeResult{OK: true, Status: response.StatusCode, Proto: response.Proto}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("PING")); err != nil {
		return result, err
	}
	_, message, err := conn.ReadMessage()
	if err != nil {
		return result, err
	}
	result.Detail = "websocket echoed: " + string(message)
	return result, nil
}

// echoOverStream sends PING and reads the answer on a raw bidirectional stream,
// which is what an extended CONNECT gives once the handshake is done.
func echoOverStream(writer io.Writer, reader io.Reader) string {
	if _, err := io.WriteString(writer, "PING"); err != nil {
		return "writing to the stream: " + err.Error()
	}
	buf := make([]byte, 64)
	n, err := reader.Read(buf)
	if n == 0 && err != nil {
		return "reading from the stream: " + err.Error()
	}
	return "stream echoed: " + string(buf[:n])
}

// probeUDP sends one datagram and waits for an answer. There is no QUIC client
// here, and none is needed: a QUIC handshake begins with a UDP datagram, so
// whether the datagram leaves the actor's network namespace at all settles the
// question ahead of anything the handshake would do next.
func probeUDP(ctx context.Context, request matrixapi.ProbeRequest) (matrixapi.ProbeResult, error) {
	payload := []byte(request.Payload)
	if len(payload) == 0 {
		var err error
		if payload, err = quicInitialDatagram(); err != nil {
			return matrixapi.ProbeResult{}, err
		}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", request.Target)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(payload); err != nil {
		return matrixapi.ProbeResult{}, err
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	return matrixapi.ProbeResult{OK: true, Detail: fmt.Sprintf("read %d bytes back", n)}, nil
}

// quicInitialDatagram is a QUIC v1 long-header Initial packet: the first thing
// a QUIC client puts on the wire, padded to the 1200 bytes the spec requires so
// it is the same size as the real thing. It carries no CRYPTO frame, so a QUIC
// server will not complete a handshake with it -- what it establishes is
// whether a datagram of this shape and size is forwarded at all.
func quicInitialDatagram() ([]byte, error) {
	const datagramSize = 1200
	datagram := make([]byte, datagramSize)
	// Header form 1, fixed bit 1, Initial packet type, 4-byte packet number.
	datagram[0] = 0xc3
	// Version 1.
	datagram[1], datagram[2], datagram[3], datagram[4] = 0x00, 0x00, 0x00, 0x01
	// An 8-byte destination connection ID, chosen at random as a real client's
	// first one is, then a zero-length source connection ID.
	datagram[5] = 0x08
	if _, err := rand.Read(datagram[6:14]); err != nil {
		return nil, fmt.Errorf("generating a connection ID: %w", err)
	}
	datagram[14] = 0x00
	return datagram, nil
}

// probeDNS resolves a name, optionally against a named resolver and over a
// named transport. UDP and TCP are worth asking separately: they leave the
// actor by different paths, and only one of them passes a policy check.
func probeDNS(ctx context.Context, request matrixapi.ProbeRequest) (matrixapi.ProbeResult, error) {
	name := request.Name
	if name == "" {
		name = "www.google.com"
	}
	resolver := &net.Resolver{PreferGo: true}
	if request.Target != "" || request.Network != "" {
		resolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			if request.Network != "" {
				network = request.Network
			}
			if request.Target != "" {
				address = request.Target
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
	}
	addresses, err := resolver.LookupHost(ctx, name)
	if err != nil {
		return matrixapi.ProbeResult{}, err
	}
	return matrixapi.ProbeResult{OK: true, Detail: name + " resolved to " + strings.Join(addresses, ", ")}, nil
}

// dial opens the connection a case needs, wrapping it in TLS with the ALPN its
// HTTP version implies. The certificate is not verified; see matrixapi.ProbeRequest.TLS.
func dial(ctx context.Context, request matrixapi.ProbeRequest) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", request.Target)
	if err != nil {
		return nil, err
	}
	if !request.TLS {
		return conn, nil
	}
	alpn := "http/1.1"
	if request.HTTP == "2" {
		alpn = "h2"
	}
	tlsConn := tls.Client(conn, clientTLSConfig(alpn))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func clientTLSConfig(alpn string) *tls.Config {
	return &tls.Config{
		//nolint:gosec // The origin is a fixture with a self-signed certificate,
		// and this egress path does not terminate TLS, so verifying it would
		// only test the fixture.
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
		MinVersion:         tls.VersionTLS12,
	}
}

func readLimited(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, bodyLimit))
	if err != nil {
		return "reading the body: " + err.Error()
	}
	return strings.TrimSpace(string(raw))
}
