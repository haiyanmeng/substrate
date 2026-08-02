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

// Command sdsload drives an Envoy listener that mints certificates per SNI.
//
// Off-the-shelf load generators do not fit this measurement. Three things are
// needed that they do not offer together:
//
//   - A distinct SNI per connection. The subject is how the proxy behaves as
//     the set of live secrets grows, so every connection has to be a new name.
//   - Handshake time isolated from request time. What an on-demand secret
//     costs is paid entirely during certificate selection, and a total-request
//     number buries it.
//   - Open-loop arrival. A fixed-concurrency client cannot show saturation:
//     when the server slows down the client offers less load, and the latency
//     of the requests that were never sent goes unrecorded. That is coordinated
//     omission, and it is exactly what the saturation phase is looking for.
//
// Output is a JSON object on stdout, so a harness can diff two runs.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-substrate/substrate/internal/atunnel"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sdsload:", err)
		os.Exit(1)
	}
}

type options struct {
	target      string
	sniFormat   string
	sniStart    int
	distinct    int
	count       int
	rate        float64
	workers     int
	maxInFlight int
	timeout     time.Duration
	caPath      string
	insecure    bool
	doRequest   bool
	label       string
	kv          bool
	jsonOut     string

	// Egress-gateway mode. Empty connectVia keeps the direct-dial path.
	connectVia        string
	connectServerName string
	connectCredential string
	connectTrust      string
	connectDest       string
	connectAtespace   string
	connectActor      string
	connectVersion    int64

	// tunnel is nil unless --connect-via was given. Safe to share across the
	// worker goroutines; the client holds only a TLS config and a dialer.
	tunnel     *atunnel.Client
	tunnelMeta atunnel.EgressMetadata
}

func run() error {
	var o options
	flag.StringVar(&o.target, "target", "127.0.0.1:18443", "host:port of the listener under test")
	flag.StringVar(&o.sniFormat, "sni-format", "h%d.mitm.example", "printf format for the SNI, given one integer")
	flag.IntVar(&o.sniStart, "sni-start", 0, "first index substituted into --sni-format")
	flag.IntVar(&o.distinct, "distinct", 0, "cycle over this many distinct names; 0 means a brand-new name for every connection")
	flag.IntVar(&o.count, "count", 1000, "how many connections to open")
	flag.Float64Var(&o.rate, "rate", 0, "open-loop arrival rate in connections/sec; 0 means closed loop at --workers")
	flag.IntVar(&o.workers, "workers", 8, "concurrency when --rate is 0")
	flag.IntVar(&o.maxInFlight, "max-inflight", 2048, "cap on concurrent connections; arrivals past it are counted as dropped rather than queued")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Second, "per-connection budget covering dial, handshake and request")
	flag.StringVar(&o.caPath, "ca", "", "PEM file of the MITM CA to verify against")
	flag.BoolVar(&o.insecure, "insecure", false, "skip certificate verification")
	flag.BoolVar(&o.doRequest, "request", false, "also send a GET / and read the response, timed separately")
	flag.StringVar(&o.label, "label", "", "free-form label echoed back in the JSON output")
	flag.BoolVar(&o.kv, "kv", false, "print flat key=value lines instead of JSON, so a shell can read the result without a JSON parser")
	flag.StringVar(&o.jsonOut, "json-out", "", "also write the full JSON result to this file")
	flag.StringVar(&o.connectVia, "connect-via", "", "host:port of a substrate egress gateway to tunnel through with mTLS HTTP CONNECT, instead of dialling --target directly. Note that the reported dial time then also covers the gateway handshake and the CONNECT round-trip, so it is not comparable to a direct-dial baseline; handshake and request times still are.")
	flag.StringVar(&o.connectServerName, "connect-server-name", "", "TLS server name to verify the gateway as; defaults to the host part of --connect-via")
	flag.StringVar(&o.connectCredential, "connect-credential-bundle", "/run/podidentity.podcert.ate.dev/credential-bundle.pem", "client credential bundle presented to the gateway")
	flag.StringVar(&o.connectTrust, "connect-trust-bundle", "/run/servicedns.podcert.ate.dev/trust-bundle.pem", "trust bundle the gateway's serving certificate is verified against")
	flag.StringVar(&o.connectDest, "connect-destination", "192.0.2.1:443", "CONNECT authority, which must be an IP:port because in the real path it comes from SO_ORIGINAL_DST. The gateway re-resolves the destination from the inner SNI, so this value is not what decides where the traffic goes.")
	flag.StringVar(&o.connectAtespace, "connect-atespace", "sdsload", "value of the X-Ate-Atespace header")
	flag.StringVar(&o.connectActor, "connect-actor", "sdsload", "value of the X-Ate-Actor-Name header")
	flag.Int64Var(&o.connectVersion, "connect-actor-version", 1, "value of the X-Ate-Actor-Version header; must be at least 1")
	flag.Parse()

	if o.count <= 0 {
		return errors.New("--count must be positive")
	}
	tlsConfig, err := buildTLSConfig(o)
	if err != nil {
		return err
	}
	if err := buildTunnel(&o); err != nil {
		return err
	}

	r := newRecorder(o.count)
	start := time.Now()
	if o.rate > 0 {
		runOpenLoop(o, tlsConfig, r)
	} else {
		runClosedLoop(o, tlsConfig, r)
	}
	wall := time.Since(start)

	out := r.summarise(o, wall)
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if o.jsonOut != "" {
		if err := os.WriteFile(o.jsonOut, append(body, '\n'), 0o644); err != nil {
			return fmt.Errorf("writing --json-out: %w", err)
		}
	}
	if !o.kv {
		fmt.Println(string(body))
		return nil
	}
	return printKV(body)
}

// printKV flattens the result to one key=value per line. The harness is a
// shell script, and requiring jq or python to read a number out of it would
// put a dependency between whoever wants to run this and the measurement.
func printKV(body []byte) error {
	var tree map[string]any
	if err := json.Unmarshal(body, &tree); err != nil {
		return err
	}
	var lines []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, sub := range t {
				walk(prefix+"_"+k, sub)
			}
		case []any:
			// Only warnings land here, and they are prose. Joined rather than
			// indexed so the key stays stable when the count changes.
			parts := make([]string, 0, len(t))
			for _, e := range t {
				parts = append(parts, fmt.Sprint(e))
			}
			lines = append(lines, prefix+"="+strings.Join(parts, "; "))
		case float64:
			if t == float64(int64(t)) {
				lines = append(lines, fmt.Sprintf("%s=%d", prefix, int64(t)))
			} else {
				lines = append(lines, fmt.Sprintf("%s=%g", prefix, t))
			}
		default:
			lines = append(lines, fmt.Sprintf("%s=%v", prefix, t))
		}
	}
	for k, v := range tree {
		walk(k, v)
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

func buildTLSConfig(o options) (*tls.Config, error) {
	cfg := &tls.Config{
		InsecureSkipVerify: o.insecure, //nolint:gosec // --insecure is an explicit opt-in for measurement runs
		// Session resumption is disabled server-side by design (an on-demand
		// certificate cannot be selected on a resumed connection), so make sure
		// the client is not the one holding a ticket either. Every connection
		// must be a full handshake or the measurement is of the wrong thing.
		ClientSessionCache: nil,
		MinVersion:         tls.VersionTLS12,
	}
	if o.caPath == "" {
		if !o.insecure {
			return nil, errors.New("one of --ca or --insecure is required")
		}
		return cfg, nil
	}
	pem, err := os.ReadFile(o.caPath)
	if err != nil {
		return nil, fmt.Errorf("reading --ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("--ca %s contains no certificates", o.caPath)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// buildTunnel prepares the egress-gateway path, or leaves o.tunnel nil so the
// direct dial stays exactly as it was.
//
// The gateway is substrate's real front door: mTLS, then HTTP/1.1 CONNECT, with
// the actor's identity in X-Ate-* headers. Reusing internal/atunnel rather than
// writing the CONNECT by hand is the point -- a load test that speaks a
// slightly different protocol measures something the actors never do.
func buildTunnel(o *options) error {
	if o.connectVia == "" {
		return nil
	}

	serverName := o.connectServerName
	if serverName == "" {
		host, _, err := net.SplitHostPort(o.connectVia)
		if err != nil {
			return fmt.Errorf("--connect-via %q must be host:port: %w", o.connectVia, err)
		}
		serverName = host
	}
	// atunnel validates this too, but failing here names the flag rather than
	// producing an identical error on every one of --count connections.
	host, _, err := net.SplitHostPort(o.connectDest)
	if err != nil {
		return fmt.Errorf("--connect-destination %q must be ip:port: %w", o.connectDest, err)
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("--connect-destination %q must use an IP address, not a hostname: the gateway takes the destination host from the inner SNI", o.connectDest)
	}
	if o.connectVersion < 1 {
		return errors.New("--connect-actor-version must be at least 1")
	}

	client, err := atunnel.NewClient(atunnel.ClientConfig{
		GatewayAddress:       o.connectVia,
		ServerName:           serverName,
		CredentialBundlePath: o.connectCredential,
		TrustBundlePath:      o.connectTrust,
	})
	if err != nil {
		return fmt.Errorf("building the egress tunnel client: %w", err)
	}
	o.tunnel = client
	o.tunnelMeta = atunnel.EgressMetadata{
		Atespace:     o.connectAtespace,
		ActorName:    o.connectActor,
		ActorVersion: o.connectVersion,
	}
	return nil
}

// sniFor returns the name connection i should ask for. With --distinct set the
// names wrap, which is how the warm-path phase hits secrets Envoy already
// holds; without it every connection is a first contact.
//
// A --sni-format with no verb in it is taken literally, so a single real
// hostname can be driven without Sprintf appending %!(EXTRA int=...) to it.
func sniFor(o options, i int) string {
	if !strings.Contains(o.sniFormat, "%") {
		return o.sniFormat
	}
	n := o.sniStart + i
	if o.distinct > 0 {
		n = o.sniStart + i%o.distinct
	}
	return fmt.Sprintf(o.sniFormat, n)
}

// runOpenLoop issues connection i at start+i/rate regardless of whether
// earlier connections have finished. If the in-flight cap is hit the arrival
// is recorded as dropped rather than delayed: a delay would quietly convert
// server saturation into client backpressure and hide it.
func runOpenLoop(o options, cfg *tls.Config, r *recorder) {
	interval := time.Duration(float64(time.Second) / o.rate)
	sem := make(chan struct{}, o.maxInFlight)
	var wg sync.WaitGroup

	begin := time.Now()
	for i := range o.count {
		due := begin.Add(time.Duration(i) * interval)
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		select {
		case sem <- struct{}{}:
		default:
			r.recordDrop()
			continue
		}
		// Scheduling lag is the client's own confession: if it is large the
		// offered rate was not actually achieved and the run is invalid.
		lag := time.Since(due)
		wg.Add(1)
		go func(i int, lag time.Duration) {
			defer wg.Done()
			defer func() { <-sem }()
			r.record(dial(o, cfg, sniFor(o, i)), lag)
		}(i, lag)
	}
	wg.Wait()
}

// runClosedLoop is the fixed-concurrency mode, for phases that want a steady
// backlog rather than a fixed arrival rate.
func runClosedLoop(o options, cfg *tls.Config, r *recorder) {
	work := make(chan int)
	var wg sync.WaitGroup
	for range o.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				r.record(dial(o, cfg, sniFor(o, i)), 0)
			}
		}()
	}
	for i := range o.count {
		work <- i
	}
	close(work)
	wg.Wait()
}

// attempt is one connection's worth of measurement.
type attempt struct {
	dial      time.Duration
	handshake time.Duration
	request   time.Duration
	class     string // "" on success
}

// open returns the byte stream the TLS handshake will run over: either a plain
// socket to --target, or a CONNECT tunnel through the egress gateway. Both are
// timed as "dial", which is why --connect-via warns that the two are not
// comparable -- the tunnel bucket also contains a TLS handshake and a
// round-trip that the direct one does not.
func open(o options, deadline time.Time) (net.Conn, error) {
	if o.tunnel == nil {
		return net.DialTimeout("tcp", o.target, time.Until(deadline))
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return o.tunnel.DialContext(ctx, o.connectDest, o.tunnelMeta)
}

func dial(o options, cfg *tls.Config, sni string) attempt {
	var a attempt
	deadline := time.Now().Add(o.timeout)

	t0 := time.Now()
	conn, err := open(o, deadline)
	a.dial = time.Since(t0)
	if err != nil {
		a.class = classify(err, "dial")
		return a
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		a.class = "other"
		return a
	}

	// A fresh config per connection so ServerName varies without racing, and
	// so no session state is shared between connections.
	per := cfg.Clone()
	per.ServerName = sni
	tlsConn := tls.Client(conn, per)

	t1 := time.Now()
	err = tlsConn.Handshake()
	a.handshake = time.Since(t1)
	if err != nil {
		a.class = classify(err, "handshake")
		return a
	}

	if !o.doRequest {
		return a
	}

	t2 := time.Now()
	req := "GET / HTTP/1.1\r\nHost: " + sni + "\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tlsConn, req); err != nil {
		a.class = classify(err, "request")
		return a
	}
	if _, err := io.Copy(io.Discard, tlsConn); err != nil {
		a.class = classify(err, "request")
		return a
	}
	a.request = time.Since(t2)
	return a
}

// classify buckets a failure. The classes mean genuinely different things --
// a timeout is the proxy stalling, an alert is the proxy refusing, a refused
// connection is the listener being gone -- and collapsing them into a single
// error count loses the finding.
func classify(err error, phase string) string {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return phase + "-timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.EOF), strings.Contains(err.Error(), "EOF"):
		return phase + "-eof"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "verify"
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return "verify"
	}
	if strings.Contains(err.Error(), "alert") || strings.Contains(err.Error(), "handshake failure") {
		return "alert"
	}
	return phase + "-other"
}

type recorder struct {
	mu         sync.Mutex
	dials      []time.Duration
	handshakes []time.Duration
	requests   []time.Duration
	lags       []time.Duration
	failures   map[string]int
	ok         int
	dropped    int
}

func newRecorder(capacity int) *recorder {
	return &recorder{
		dials:      make([]time.Duration, 0, capacity),
		handshakes: make([]time.Duration, 0, capacity),
		requests:   make([]time.Duration, 0, capacity),
		lags:       make([]time.Duration, 0, capacity),
		failures:   make(map[string]int),
	}
}

func (r *recorder) record(a attempt, lag time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lags = append(r.lags, lag)
	if a.class != "" {
		r.failures[a.class]++
		return
	}
	r.ok++
	r.dials = append(r.dials, a.dial)
	r.handshakes = append(r.handshakes, a.handshake)
	if a.request > 0 {
		r.requests = append(r.requests, a.request)
	}
}

func (r *recorder) recordDrop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped++
}

type summary struct {
	Label string `json:"label,omitempty"`
	// ConnectVia is empty on a direct run. It is recorded so a saved result can
	// never be mistaken for a direct-dial baseline, whose dial numbers mean
	// something different.
	ConnectVia   string           `json:"connect_via,omitempty"`
	Attempted    int              `json:"attempted"`
	OK           int              `json:"ok"`
	Failed       int              `json:"failed"`
	Dropped      int              `json:"dropped"`
	Failures     map[string]int   `json:"failures,omitempty"`
	RateTarget   float64          `json:"rate_target"`
	RateAchieved float64          `json:"rate_achieved"`
	WallSeconds  float64          `json:"wall_s"`
	ClientCPUSec float64          `json:"client_cpu_s"`
	DialUS       map[string]int64 `json:"dial_us"`
	HandshakeUS  map[string]int64 `json:"handshake_us"`
	RequestUS    map[string]int64 `json:"request_us,omitempty"`
	LagUS        map[string]int64 `json:"schedule_lag_us"`
	Warnings     []string         `json:"warnings,omitempty"`
}

func (r *recorder) summarise(o options, wall time.Duration) summary {
	r.mu.Lock()
	defer r.mu.Unlock()

	failed := 0
	for _, n := range r.failures {
		failed += n
	}
	s := summary{
		Label:        o.label,
		ConnectVia:   o.connectVia,
		Attempted:    o.count,
		OK:           r.ok,
		Failed:       failed,
		Dropped:      r.dropped,
		Failures:     r.failures,
		RateTarget:   o.rate,
		RateAchieved: float64(r.ok+failed) / wall.Seconds(),
		WallSeconds:  round3(wall.Seconds()),
		ClientCPUSec: round3(clientCPU()),
		DialUS:       percentiles(r.dials),
		HandshakeUS:  percentiles(r.handshakes),
		LagUS:        percentiles(r.lags),
	}
	if len(r.requests) > 0 {
		s.RequestUS = percentiles(r.requests)
	}

	// Two ways the client invalidates its own numbers. Say so in the output
	// rather than leaving it to whoever reads the table later.
	if s.LagUS["p99"] > 50_000 {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"client fell behind its own schedule by %dms at p99; the offered rate was not achieved", s.LagUS["p99"]/1000))
	}
	if r.dropped > 0 {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"%d arrivals were dropped at the --max-inflight cap of %d", r.dropped, o.maxInFlight))
	}
	return s
}

// percentiles reports microseconds, which is the resolution that matters here:
// a handshake is hundreds of microseconds and a stalled one is seconds.
func percentiles(d []time.Duration) map[string]int64 {
	if len(d) == 0 {
		return map[string]int64{}
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(q float64) int64 {
		i := int(q * float64(len(sorted)-1))
		return sorted[i].Microseconds()
	}
	return map[string]int64{
		"p50": at(0.50),
		"p90": at(0.90),
		"p95": at(0.95),
		"p99": at(0.99),
		"max": sorted[len(sorted)-1].Microseconds(),
	}
}

// clientCPU reports this process's CPU time. Every connection is a full
// handshake with no resumption, so the client pays a P-256 verify each time
// and will saturate before the proxy does if nobody is watching.
func clientCPU() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	sec := func(t syscall.Timeval) float64 {
		return float64(t.Sec) + float64(t.Usec)/1e6
	}
	return sec(ru.Utime) + sec(ru.Stime)
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}
