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

// genpool writes a single-CA localca pool plus the root in PEM, for the
// expiry harness next door. The pool the old PoC left behind under
// poc/sdsmint/__run expired on 2026-08-03, and CA.Validate refuses it.
//
// No name constraints, unlike the deployed MITM CA. The harness asks openssl
// to verify the leaf it is served, and a constraint violation and an expired
// leaf both surface there as "verify error" -- leaving the CA unconstrained
// means anything openssl complains about is about expiry, which is the one
// thing being measured.
//
// ECDSA P-256 rather than the localca default of Ed25519, because Envoy is
// built against BoringSSL and this harness is not the place to find out how it
// feels about an Ed25519 issuer.
package main

import (
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agent-substrate/substrate/internal/localca"
)

func main() {
	var (
		id       = flag.String("ca-id", "mitm", "CA ID within the pool")
		poolOut  = flag.String("pool-out", "pool.json", "where to write the marshalled pool")
		certOut  = flag.String("cert-out", "ca.pem", "where to write the root certificate in PEM")
		lifetime = flag.Duration("lifetime", 24*time.Hour, "root certificate lifetime")
	)
	flag.Parse()

	if err := run(*id, *poolOut, *certOut, *lifetime); err != nil {
		fmt.Fprintln(os.Stderr, "genpool:", err)
		os.Exit(1)
	}
}

func run(id, poolOut, certOut string, lifetime time.Duration) error {
	ca, err := localca.GenerateCA(localca.GenerateOptions{
		ID:         id,
		CommonName: "sdsmint expiry harness CA",
		KeyType:    localca.KeyTypeECDSAP256,
		Lifetime:   lifetime,
	})
	if err != nil {
		return fmt.Errorf("generating CA: %w", err)
	}
	// The same check sdsmint makes at startup, made here instead so a bad pool
	// fails at the point it was written rather than as a server that will not
	// boot.
	if err := ca.Validate(); err != nil {
		return fmt.Errorf("generated CA does not validate: %w", err)
	}

	poolBytes, err := localca.Marshal(&localca.Pool{CAs: []*localca.CA{ca}})
	if err != nil {
		return fmt.Errorf("marshalling pool: %w", err)
	}
	// 0600: the pool carries the CA signing key.
	if err := os.WriteFile(poolOut, poolBytes, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", poolOut, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: ca.RootCertificate.Raw,
	})
	if err := os.WriteFile(certOut, certPEM, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", certOut, err)
	}

	fmt.Printf("wrote %s and %s: CA %q valid until %s\n",
		poolOut, certOut, id, ca.RootCertificate.NotAfter.Format(time.RFC3339))
	return nil
}
