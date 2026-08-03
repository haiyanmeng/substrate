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

// Command egressprobe stands in for atunnel's egress client. It opens a CONNECT
// tunnel to the gateway with a chosen actor identity and destination, then
// optionally sends one HTTP request inside the tunnel with a chosen Host.
//
// curl cannot drive this: through a proxy it will not let the CONNECT authority
// and the inner Host be set independently, and the whole point of the two
// checkpoints is that they read exactly those two different values. The probe
// speaks the exchange by hand and prints a JSON result the harness can assert
// on.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

var (
	gateway   = pflag.String("gateway", "127.0.0.1:18500", "Egress gateway address to dial.")
	connectTo = pflag.String("connect-to", "127.0.0.1:19602", "CONNECT authority. atunnel only ever sends an ip:port literal here.")

	atespace     = pflag.String("atespace", "acme-prod", "X-Ate-Atespace value.")
	actor        = pflag.String("actor", "", "X-Ate-Actor-Name value. Empty omits the header, which must fail closed.")
	actorVersion = pflag.String("actor-version", "7", "X-Ate-Actor-Version value.")

	connectHeaders = pflag.StringArray("connect-header", nil, "Extra CONNECT header, \"name: value\". Repeatable. Use it to try forging x-ate-egress-mode.")

	innerHost    = pflag.String("inner-host", "", "Host for the request sent inside the tunnel. Empty stops after the CONNECT.")
	innerPath    = pflag.String("inner-path", "/echo", "Path for the request sent inside the tunnel.")
	innerHeaders = pflag.StringArray("inner-header", nil, "Extra inner-request header, \"name: value\". Repeatable.")

	timeout = pflag.Duration("timeout", 10*time.Second, "Deadline for the whole exchange.")
)

// result is the probe's stdout contract. Every field is filled on every run so
// the harness can key on presence rather than parsing prose.
type result struct {
	ConnectStatus int               `json:"connectStatus"`
	ConnectBody   string            `json:"connectBody"`
	InnerStatus   int               `json:"innerStatus"`
	InnerBody     string            `json:"innerBody"`
	InnerHeaders  map[string]string `json:"innerHeaders,omitempty"`
	Error         string            `json:"error,omitempty"`
}

func main() {
	pflag.Parse()

	res := probe()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)

	// Exit 0 even on a policy denial: a 403 is a successful measurement, not a
	// probe failure. Only a transport error is non-zero, so the harness can tell
	// "the gateway said no" from "the gateway was not there".
	if res.Error != "" {
		os.Exit(1)
	}
}

func probe() result {
	var res result

	conn, err := net.DialTimeout("tcp", *gateway, *timeout)
	if err != nil {
		res.Error = fmt.Sprintf("dialing gateway: %v", err)
		return res
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(*timeout))

	if err := writeConnect(conn); err != nil {
		res.Error = fmt.Sprintf("sending CONNECT: %v", err)
		return res
	}

	br := bufio.NewReader(conn)
	// ReadResponse needs the request method: a 2xx to a CONNECT has no body and
	// the bytes that follow belong to the tunnel, not to the response.
	connectResp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		res.Error = fmt.Sprintf("reading CONNECT response: %v", err)
		return res
	}
	res.ConnectStatus = connectResp.StatusCode

	// Read the body ONLY on a rejection. A 2xx to a CONNECT has no body, and Go
	// reports its length as unknown, so resp.Body is the tunnel itself -- reading
	// it consumes the bytes the inner exchange needs and blocks until the
	// deadline. The symptom is a probe that hangs for exactly --timeout and then
	// fails on the write, which reads like a gateway fault and is not one.
	if connectResp.StatusCode/100 != 2 {
		res.ConnectBody = readBody(connectResp)
		return res
	}
	if *innerHost == "" {
		return res
	}

	if err := writeInner(conn); err != nil {
		res.Error = fmt.Sprintf("sending the inner request: %v", err)
		return res
	}

	innerResp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		res.Error = fmt.Sprintf("reading the inner response: %v", err)
		return res
	}
	res.InnerStatus = innerResp.StatusCode
	res.InnerBody = readBody(innerResp)
	res.InnerHeaders = map[string]string{}
	for k, v := range innerResp.Header {
		if len(v) > 0 {
			res.InnerHeaders[k] = v[0]
		}
	}
	return res
}

func writeConnect(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\n", *connectTo)
	fmt.Fprintf(&b, "Host: %s\r\n", *connectTo)
	fmt.Fprintf(&b, "X-Ate-Atespace: %s\r\n", *atespace)
	if *actor != "" {
		fmt.Fprintf(&b, "X-Ate-Actor-Name: %s\r\n", *actor)
	}
	fmt.Fprintf(&b, "X-Ate-Actor-Version: %s\r\n", *actorVersion)
	writeExtra(&b, *connectHeaders)
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeInner(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", *innerPath)
	fmt.Fprintf(&b, "Host: %s\r\n", *innerHost)
	// Close the connection after one response so the probe never has to guess
	// whether more is coming.
	b.WriteString("Connection: close\r\n")
	writeExtra(&b, *innerHeaders)
	b.WriteString("\r\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func writeExtra(b *strings.Builder, headers []string) {
	for _, h := range headers {
		name, value, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		fmt.Fprintf(b, "%s: %s\r\n", strings.TrimSpace(name), strings.TrimSpace(value))
	}
}

func readBody(resp *http.Response) string {
	defer resp.Body.Close()
	// Bound the read: a misbehaving upstream must not hang the harness.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return string(body)
}
