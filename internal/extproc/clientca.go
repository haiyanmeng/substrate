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
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// ClientCABundler concatenates the gateway's client-authentication CAs into one
// PEM file for Envoy.
//
// This is here rather than in its own component for a reason worth stating,
// because it is not obviously the egress PEP's job. The gateway must accept two
// client CAs -- podidentity, for pod tooling such as the e2e egressprobe, and
// the actor CA that signs the certificates this package authorizes -- and
// Envoy's CertificateValidationContext.trusted_ca is a single DataSource with no
// repeated form, so the two PEMs have to arrive already joined. The pod is
// distroless, so there is no shell for a `cat` sidecar; a one-shot init
// container would go stale when the podidentity CA rotates; and extprocd is
// already an always-restart sidecar ordered ahead of Envoy exactly as sdsmintd
// is. If this ever reads wrong, the alternative is a third small sidecar, not
// moving it into Envoy.
//
// Envoy watches the output directory and reloads on change, so rotation
// propagates without restarting anything.
type ClientCABundler struct {
	// Inputs are PEM files to concatenate, in order. A missing or unparseable
	// input is fatal at startup and non-fatal afterwards: losing an input during
	// a rotation must not narrow the trust set to whatever is still readable.
	Inputs []string
	// Output is the concatenated bundle Envoy reads.
	Output string
	// Interval is how often Inputs are re-read. Polling rather than inotify
	// because a projected volume update replaces the directory symlink, which is
	// the case inotify watchers most often miss.
	Interval time.Duration

	// last is the bundle currently on disk, so Run can skip identical writes and
	// so a failed rebuild has something to fall back to.
	last []byte
}

// RunOnce writes the bundle a single time.
//
// Call it synchronously at startup: Envoy's front door cannot load its
// validation context until this file exists, and a missing or malformed input
// should stop the pod here, with the offending path named, rather than surface
// later as an Envoy config-load failure.
func (b *ClientCABundler) RunOnce() error {
	out, err := b.build()
	if err != nil {
		return err
	}
	// 0644: this is public CA material, and Envoy runs as a different user in
	// the same pod.
	if err := os.WriteFile(b.Output, out, 0o644); err != nil {
		return fmt.Errorf("writing the client-CA bundle to %s: %w", b.Output, err)
	}
	b.last = out
	slog.Info("Wrote the gateway client-CA bundle",
		slog.String("path", b.Output),
		slog.Int("inputs", len(b.Inputs)),
		slog.Int("certificates", countCertificates(out)))
	return nil
}

// Run keeps the bundle current until ctx is cancelled. RunOnce must have
// succeeded first.
func (b *ClientCABundler) Run(ctx context.Context) error {
	interval := b.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			next, err := b.build()
			if err != nil {
				// Keep serving the last good bundle. A transient read failure
				// mid-rotation must not drop a CA and start rejecting handshakes.
				slog.Error("Could not rebuild the client-CA bundle; keeping the last good one",
					slog.Any("err", err))
				continue
			}
			if bytes.Equal(next, b.last) {
				continue
			}
			if err := os.WriteFile(b.Output, next, 0o644); err != nil {
				slog.Error("Could not write the client-CA bundle", slog.Any("err", err))
				continue
			}
			b.last = next
			slog.Info("Refreshed the gateway client-CA bundle",
				slog.String("path", b.Output),
				slog.Int("certificates", countCertificates(next)))
		}
	}
}

// build reads every input and re-encodes the certificates it finds.
//
// Re-encoding rather than concatenating raw bytes normalizes trailing newlines
// between files -- two PEM files joined without one produce a "-----END
// CERTIFICATE----------BEGIN CERTIFICATE-----" run that Envoy parses as a
// single truncated block, silently trusting only the first CA.
func (b *ClientCABundler) build() ([]byte, error) {
	var out bytes.Buffer
	for _, path := range b.Inputs {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading client CA %s: %w", path, err)
		}
		n := 0
		for rest := data; ; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return nil, fmt.Errorf("parsing a certificate in client CA %s: %w", path, err)
			}
			if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}); err != nil {
				return nil, fmt.Errorf("re-encoding client CA %s: %w", path, err)
			}
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("client CA %s holds no certificates", path)
		}
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("no client CAs configured")
	}
	return out.Bytes(), nil
}

func countCertificates(pemBytes []byte) int {
	n := 0
	for rest := pemBytes; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return n
		}
		if block.Type == "CERTIFICATE" {
			n++
		}
	}
}
