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

package sdsmint

import (
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"

	"github.com/agent-substrate/substrate/cmd/atenet/internal/sdsmint/certauth"
)

// secretTypeURL is the xDS type URL for SDS resources.
const secretTypeURL = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"

// server implements Envoy's Secret Discovery Service, minting a certificate
// per requested resource name. Because the on-demand certificate selector maps
// SNI to the secret name, "resource name" and "hostname" are the same thing.
//
// The type is unexported -- NewSdsmintCmd is the only way in from outside the
// package -- but DeltaSecrets is not: it is part of
// secretservice.SecretDiscoveryServiceServer, which registration requires.
//
// DeltaSecrets is the only method of that service this server implements.
// State-of-the-world SDS -- StreamSecrets and FetchSecrets -- is deliberately
// left to the embedded Unimplemented, so an Envoy configured with anything
// other than DELTA_GRPC fails immediately and visibly. It used to be
// implemented, and that was worse than not having it: SotW has no removal
// channel, so a refused name could only be expressed by omission and the
// refusal did not exist on that path at all. A misconfigured api_type served
// certificates and silently ran with it switched off.
type server struct {
	secretservice.UnimplementedSecretDiscoveryServiceServer

	minter *minter
	log    *slog.Logger

	// resourceTTL is the xDS TTL stamped on every resource this server sends:
	// how long Envoy holds a secret before dropping it of its own accord. It is
	// derived from the leaf lifetime rather than configured separately, so the
	// two cannot be set into an order that does not work. See pack, which
	// stamps it, for what Envoy does with it.
	resourceTTL time.Duration

	// nonce numbers the responses this server sends. Every xDS response needs
	// one: the client echoes it back as response_nonce, which is what makes a
	// later request recognizable as an ACK or NACK of a specific response
	// rather than a fresh subscription. A response that carries none cannot be
	// ACKed at all.
	//
	// One counter for the whole server rather than one per stream. A client
	// only ever compares a nonce against the last one it received on its own
	// stream, so the sequence being sparse there costs nothing, and this
	// server does not correlate them either -- an incoming response_nonce is
	// read only for the NACK log line in deltaStream.handle. Atomic because
	// streams are served concurrently.
	nonce atomic.Uint64
}

// serverOptions configures newServer.
type serverOptions struct {
	Logger *slog.Logger
}

// newServer builds an SDS server over m.
func newServer(m *minter, opts serverOptions) *server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	// Half the leaf lifetime. Envoy drops the secret when the TTL fires and the
	// next handshake for that name re-subscribes, so a replacement is minted
	// while the leaf it replaces is still valid. A TTL equal to the leaf
	// lifetime would drop the secret at the moment the leaf died, leaving a
	// window for a handshake to land on one already past its notAfter.
	const refreshFraction = 2

	return &server{
		minter:      m,
		log:         opts.Logger,
		resourceTTL: m.ttl / refreshFraction,
	}
}

// nextNonce returns the nonce to stamp on the next response. It increments
// before formatting, so the first response is "1" and none ever goes out with
// the empty nonce.
func (s *server) nextNonce() string {
	return strconv.FormatUint(s.nonce.Add(1), 10)
}

// inlineBytes wraps PEM bytes as an inline Envoy DataSource. Leaf material is
// inlined rather than written to a path because it is per-connection and
// short-lived; putting it on a filesystem would only widen exposure.
func inlineBytes(b []byte) *corev3.DataSource {
	return &corev3.DataSource{
		Specifier: &corev3.DataSource_InlineBytes{InlineBytes: b},
	}
}

// toSecret packs a minted cert into the Secret proto Envoy expects back. The
// secret's name MUST equal the requested resource name (the SNI), or Envoy
// will not match the response to its subscription.
func toSecret(name string, c *certauth.MintedCert) *tlsv3.Secret {
	return &tlsv3.Secret{
		Name: name,
		Type: &tlsv3.Secret_TlsCertificate{
			TlsCertificate: &tlsv3.TlsCertificate{
				CertificateChain: inlineBytes(c.CertChainPEM),
				PrivateKey:       inlineBytes(c.PrivateKeyPEM),
			},
		},
	}
}
