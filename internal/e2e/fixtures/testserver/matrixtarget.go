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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"

	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
)

// matrixtarget is the origin the egress protocol matrix dials. One port serves
// every shape in the matrix, because the port is sniffed: a TLS ClientHello
// goes to the TLS listener, an HTTP/2 client preface to an h2c server, and
// anything else to an HTTP/1.1 server. One address for every case keeps the
// EgressPolicy under test identical across them -- the same hostname, the same
// dialed address -- so a difference in outcome is a difference in protocol and
// nothing else.

// h2Preface is the HTTP/2 connection preface a prior-knowledge (h2c) client
// sends before anything else. Sniffing it is how the cleartext port tells h2c
// from HTTP/1.1 without an Upgrade.
const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// sniffTimeout bounds the wait for the bytes that classify a connection. A
// client that says nothing is served as HTTP/1.1, which is what a stray health
// check or port scan gets.
const sniffTimeout = 5 * time.Second

// wsProtocol is the :protocol value of an RFC 8441 extended CONNECT carrying a
// WebSocket. The HTTP/2 server only sees it when the process runs with
// GODEBUG=http2xconnect=1; x/net/http2 keeps extended CONNECT off otherwise and
// never advertises SETTINGS_ENABLE_CONNECT_PROTOCOL.
const wsProtocol = "websocket"

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// echoBody is what GET /echo answers with: how the request arrived, as the
// server saw it. The probe reports these back, so a case that says "HTTP/2 over
// TLS" is confirmed by the origin rather than assumed from the client's config.
type echoBody struct {
	Proto string `json:"proto"`
	TLS   bool   `json:"tls"`
	ALPN  string `json:"alpn,omitempty"`
	Host  string `json:"host"`
	Path  string `json:"path"`
}

func newMatrixTargetCmd() *cobra.Command {
	var listenAddress string
	cmd := &cobra.Command{
		Use:   "matrixtarget",
		Short: "Serve every protocol shape the egress matrix probes on one sniffed port.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveMatrixTarget(cmd.Context(), listenAddress)
		},
	}
	cmd.Flags().StringVar(&listenAddress, "listen", ":8080", "Address the sniffed port listens on.")
	return cmd
}

func serveMatrixTarget(ctx context.Context, listenAddress string) error {
	certificate, err := selfSignedCertificate()
	if err != nil {
		return fmt.Errorf("generating the serving certificate: %w", err)
	}

	handler := matrixTargetHandler()
	h2 := &http2.Server{}
	h1Server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	tlsServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"h2", "http/1.1"},
			MinVersion:   tls.VersionTLS12,
		},
	}
	if err := http2.ConfigureServer(tlsServer, h2); err != nil {
		return fmt.Errorf("configuring HTTP/2 over TLS: %w", err)
	}

	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listenAddress, err)
	}
	defer listener.Close()

	// Each classification gets a listener of its own that the sniffer feeds, so
	// every server below is an ordinary net/http server over a net.Listener and
	// behaves exactly as it would on a port of its own.
	plain := newFedListener(listener.Addr())
	secure := newFedListener(listener.Addr())
	go func() { _ = h1Server.Serve(plain) }()
	go func() { _ = tlsServer.Serve(tls.NewListener(secure, tlsServer.TLSConfig)) }()

	log.Printf("testserver matrixtarget: listening on %s (TLS, h2c and HTTP/1.1 on the same port)", listenAddress)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting: %w", err)
		}
		go classify(conn, plain, secure, h2, handler)
	}
}

// classify reads just enough of conn to decide which server owns it: TLS to the
// TLS listener, an HTTP/2 preface straight to an h2c server, everything else to
// the HTTP/1.1 listener.
func classify(conn net.Conn, plain, secure *fedListener, h2 *http2.Server, handler http.Handler) {
	buffered := bufio.NewReader(conn)
	if err := conn.SetReadDeadline(time.Now().Add(sniffTimeout)); err != nil {
		conn.Close()
		return
	}
	first, err := buffered.Peek(1)
	if err != nil {
		conn.Close()
		return
	}
	// 0x16 is the TLS record type for a handshake, which is the only thing a
	// client may open a TLS connection with.
	if first[0] == 0x16 {
		resetDeadline(conn)
		secure.feed(&peekedConn{Conn: conn, reader: buffered})
		return
	}
	// Peek the whole preface. A shorter read is an HTTP/1.1 request line, and
	// Peek returns what it has along with the error.
	preface, _ := buffered.Peek(len(h2Preface))
	resetDeadline(conn)
	wrapped := &peekedConn{Conn: conn, reader: buffered}
	if bytes.Equal(preface, []byte(h2Preface)) {
		h2.ServeConn(wrapped, &http2.ServeConnOpts{Handler: handler})
		return
	}
	plain.feed(wrapped)
}

func resetDeadline(conn net.Conn) {
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		log.Printf("testserver matrixtarget: clearing the read deadline: %v", err)
	}
}

func matrixTargetHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body := echoBody{Proto: r.Proto, Host: r.Host, Path: r.URL.Path}
		if r.TLS != nil {
			body.TLS = true
			body.ALPN = r.TLS.NegotiatedProtocol
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/ws", serveWebsocket)

	// A tunneling CONNECT carries no :path, so it never reaches the mux.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect && r.Header.Get(":protocol") == "" {
			serveConnectProxy(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// serveWebsocket answers both WebSocket handshakes: the HTTP/1.1 Upgrade, and
// the RFC 8441 extended CONNECT that carries one over HTTP/2. Both echo "PING"
// as "PONG", the first in RFC 6455 frames and the second as raw stream bytes --
// the framing is the client's business, and what the matrix is asking is
// whether the handshake survived the egress path at all.
func serveWebsocket(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect && r.Header.Get(":protocol") == wsProtocol {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		echoRawStream(w, r.Body)
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("testserver matrixtarget: websocket upgrade: %v", err)
		return
	}
	defer conn.Close()
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := conn.WriteMessage(messageType, pong(message)); err != nil {
			return
		}
	}
}

// echoRawStream answers each read with its PONG, flushing so a client that
// waits for an answer before sending more is not deadlocked by buffering.
func echoRawStream(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(pong(buf[:n])); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func pong(message []byte) []byte {
	if bytes.Equal(bytes.TrimSpace(message), []byte("PING")) {
		return []byte("PONG")
	}
	return message
}

// serveConnectProxy is the forward proxy half: it dials the authority the
// CONNECT named and splices the two streams. HTTP/1.1 needs the connection
// hijacked; HTTP/2 does not, because the request body and the response writer
// already are the two halves of the stream.
func serveConnectProxy(w http.ResponseWriter, r *http.Request) {
	authority := r.Host
	if authority == "" {
		authority = r.URL.Host
	}
	upstream, err := net.DialTimeout("tcp", authority, 10*time.Second)
	if err != nil {
		log.Printf("testserver matrixtarget: CONNECT to %s: %v", authority, err)
		http.Error(w, "dialing the CONNECT authority failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	if r.ProtoMajor >= 2 {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		splice(upstream, r.Body, w)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT needs a hijackable connection", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		log.Printf("testserver matrixtarget: hijacking for CONNECT: %v", err)
		return
	}
	defer client.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	splice(upstream, buffered, client)
}

// splice copies in both directions and returns when either one ends.
//
// The write side is flushed after every copy, which matters for the HTTP/2
// half: there the client's end of the tunnel is an http.ResponseWriter, and
// one that is not flushed holds the bytes until its buffer fills or the
// handler returns. A tunnel whose first answer never arrives looks exactly
// like a tunnel the egress path dropped.
func splice(upstream net.Conn, clientReader io.Reader, clientWriter io.Writer) {
	var once sync.WaitGroup
	once.Add(1)
	go func() {
		defer once.Done()
		_, _ = io.Copy(upstream, clientReader)
		if conn, ok := upstream.(*net.TCPConn); ok {
			_ = conn.CloseWrite()
		}
	}()
	_, _ = io.Copy(flushAfterWrite(clientWriter), upstream)
	once.Wait()
}

// flushAfterWrite wraps a writer that buffers, so each Write reaches the peer.
func flushAfterWrite(w io.Writer) io.Writer {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return w
	}
	return writerFunc(func(p []byte) (int, error) {
		n, err := w.Write(p)
		flusher.Flush()
		return n, err
	})
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// peekedConn is a connection whose first bytes have already been buffered by
// the sniffer, handing them back to whichever server ends up owning it.
type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// fedListener is a net.Listener whose connections arrive from the sniffer
// instead of from an accept loop of its own.
type fedListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newFedListener(addr net.Addr) *fedListener {
	return &fedListener{addr: addr, conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *fedListener) feed(conn net.Conn) {
	select {
	case l.conns <- conn:
	case <-l.closed:
		conn.Close()
	}
}

func (l *fedListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fedListener) Addr() net.Addr { return l.addr }

// selfSignedCertificate is the TLS identity the sniffed port serves. The probe
// does not verify it: this gateway does not terminate TLS, so what the origin
// presents is not what the matrix is measuring.
func selfSignedCertificate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "matrixtarget"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"matrixtarget", "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
