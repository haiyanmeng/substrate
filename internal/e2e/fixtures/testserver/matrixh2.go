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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// h2Client is one HTTP/2 connection carrying one stream, framed by hand.
//
// The matrix needs two things no HTTP/2 client in the standard library will
// do. The first is an RFC 8441 extended CONNECT: net/http validates outgoing
// headers and rejects the ":protocol" pseudo-header before it reaches the
// wire, and since Go 1.27 x/net/http2 is a wrapper over net/http, so it
// rejects it too. The second is the exact pseudo-header set: a tunneling
// CONNECT must carry neither ":scheme" nor ":path", an extended one must carry
// both, and which of the two the egress gateway sees is the thing under test.
//
// It is deliberately minimal. One stream, no flow-control accounting beyond
// returning the window as data arrives, no priority, no trailers -- enough to
// open a stream, read the response headers and carry a few bytes each way.
type h2Client struct {
	conn   net.Conn
	framer *http2.Framer

	// writeMu serializes frame writes, which come both from the caller and
	// from the read loop answering a SETTINGS or a PING.
	writeMu   sync.Mutex
	encoder   *hpack.Encoder
	headerBuf bytes.Buffer

	// settings closes once the peer's first SETTINGS frame has been read, at
	// which point extendedConnect says whether it advertised support for
	// extended CONNECT.
	settings        chan struct{}
	settingsOnce    sync.Once
	extendedConnect bool

	// headers carries the response HEADERS of the single stream.
	headers chan *http2.MetaHeadersFrame

	// done closes when the connection fails, with err saying how. A caller
	// blocked on a response rather than on the stream learns about it here.
	done     chan struct{}
	doneOnce sync.Once
	err      error

	// reader and writer turn the stream's DATA frames into an io.Reader.
	reader *io.PipeReader
	writer *io.PipeWriter

	streamID uint32
}

// settingEnableConnectProtocol is SETTINGS_ENABLE_CONNECT_PROTOCOL (RFC 8441),
// which a server sets to 1 to say it accepts extended CONNECT. x/net/http2
// does not export the identifier.
const settingEnableConnectProtocol http2.SettingID = 0x8

// hpackTableSize is the decoder's dynamic table size, matching the HTTP/2
// default the peer assumes until told otherwise.
const hpackTableSize = 4096

// newH2Client sends the connection preface and the client's SETTINGS, then
// starts reading. conn is not closed by the client; its owner closes it.
func newH2Client(conn net.Conn) (*h2Client, error) {
	client := &h2Client{
		conn:     conn,
		framer:   http2.NewFramer(conn, conn),
		settings: make(chan struct{}),
		headers:  make(chan *http2.MetaHeadersFrame, 1),
		done:     make(chan struct{}),
		streamID: 1,
	}
	client.framer.ReadMetaHeaders = hpack.NewDecoder(hpackTableSize, nil)
	client.encoder = hpack.NewEncoder(&client.headerBuf)
	client.reader, client.writer = io.Pipe()

	if _, err := io.WriteString(conn, h2Preface); err != nil {
		return nil, fmt.Errorf("writing the HTTP/2 preface: %w", err)
	}
	if err := client.framer.WriteSettings(); err != nil {
		return nil, fmt.Errorf("writing the client SETTINGS: %w", err)
	}
	go client.readLoop()
	return client, nil
}

// openStream writes the request headers and waits for the response headers.
// It waits for the peer's SETTINGS first, which is what RFC 8441 requires of a
// client that is about to send an extended CONNECT and is free either way.
func (c *h2Client) openStream(ctx context.Context, fields []hpack.HeaderField) (*http2.MetaHeadersFrame, error) {
	select {
	case <-c.settings:
	case <-c.done:
		return nil, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.writeMu.Lock()
	c.headerBuf.Reset()
	var err error
	for _, field := range fields {
		if err = c.encoder.WriteField(field); err != nil {
			break
		}
	}
	if err == nil {
		err = c.framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      c.streamID,
			BlockFragment: c.headerBuf.Bytes(),
			EndHeaders:    true,
		})
	}
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("writing the request headers: %w", err)
	}

	select {
	case frame := <-c.headers:
		return frame, nil
	case <-c.done:
		return nil, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Read returns the stream's DATA, and Write sends some.
func (c *h2Client) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *h2Client) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.framer.WriteData(c.streamID, false, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close ends the stream locally and unblocks the read loop, which may be
// parked writing DATA nobody is going to read.
func (c *h2Client) Close() error {
	_ = c.reader.CloseWithError(net.ErrClosed)
	_ = c.writer.Close()
	c.fail(net.ErrClosed)
	return nil
}

func (c *h2Client) readLoop() {
	for {
		frame, err := c.framer.ReadFrame()
		if err != nil {
			c.fail(err)
			return
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			value, ok := f.Value(settingEnableConnectProtocol)
			c.settingsOnce.Do(func() {
				c.extendedConnect = ok && value == 1
				close(c.settings)
			})
			c.writeFrame(c.framer.WriteSettingsAck)
		case *http2.PingFrame:
			if !f.IsAck() {
				c.writeFrame(func() error { return c.framer.WritePing(true, f.Data) })
			}
		case *http2.MetaHeadersFrame:
			if f.StreamID != c.streamID {
				continue
			}
			select {
			case c.headers <- f:
			default:
			}
			if f.StreamEnded() {
				_ = c.writer.Close()
			}
		case *http2.DataFrame:
			if f.StreamID != c.streamID {
				continue
			}
			if data := f.Data(); len(data) > 0 {
				if _, err := c.writer.Write(data); err != nil {
					c.fail(err)
					return
				}
				c.returnWindow(uint32(len(data)))
			}
			if f.StreamEnded() {
				_ = c.writer.Close()
			}
		case *http2.RSTStreamFrame:
			c.fail(fmt.Errorf("the peer reset the stream: %v", f.ErrCode))
			return
		case *http2.GoAwayFrame:
			c.fail(fmt.Errorf("the peer sent GOAWAY %v: %s", f.ErrCode, f.DebugData()))
			return
		}
	}
}

// returnWindow credits back what was just consumed, on the stream and on the
// connection, so a peer with more to say is not stalled by our not asking.
func (c *h2Client) returnWindow(n uint32) {
	c.writeFrame(func() error {
		if err := c.framer.WriteWindowUpdate(0, n); err != nil {
			return err
		}
		return c.framer.WriteWindowUpdate(c.streamID, n)
	})
}

func (c *h2Client) writeFrame(write func() error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = write()
}

// fail records the first fatal error and wakes everyone waiting on one.
func (c *h2Client) fail(err error) {
	c.doneOnce.Do(func() {
		if err == nil {
			err = errors.New("the HTTP/2 connection ended")
		}
		c.err = err
		_ = c.writer.CloseWithError(err)
		close(c.done)
		c.settingsOnce.Do(func() { close(c.settings) })
	})
}
