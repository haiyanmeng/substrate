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

package certauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"
	"time"
)

// leafKey is one keypair and its serialized form, immutable once built.
//
// The PEM is rendered once here rather than once per mint, and every
// MintedCert issued against this key hands out the same slice. Nothing may
// write to it.
type leafKey struct {
	signer crypto.Signer
	pem    []byte
}

// leafKeys hands out the keypair that minted leaves are issued against.
//
// Every leaf shares one keypair until it is rotated.
type leafKeys struct {
	// rotateAfter is how long a key is handed out for before it is replaced.
	rotateAfter time.Duration

	mu        sync.Mutex
	current   *leafKey
	generated time.Time
}

// newLeafKeys builds a source that replaces its keypair every rotateAfter.
//
// The first key is generated here rather than on first use, so that a mint
// never pays for one and a keygen that cannot succeed fails at startup.
func newLeafKeys(rotateAfter time.Duration, now time.Time) (*leafKeys, error) {
	if rotateAfter <= 0 {
		return nil, fmt.Errorf("leaf key rotation period must be positive, got %v", rotateAfter)
	}
	key, err := generateLeafKey()
	if err != nil {
		return nil, err
	}
	return &leafKeys{rotateAfter: rotateAfter, current: key, generated: now}, nil
}

// at returns the key to issue with, replacing it if it has reached
// rotateAfter.
func (k *leafKeys) at(now time.Time) (*leafKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if now.Sub(k.generated) < k.rotateAfter {
		return k.current, nil
	}
	key, err := generateLeafKey()
	if err != nil {
		return nil, err
	}
	k.current, k.generated = key, now
	return key, nil
}

func generateLeafKey() (*leafKey, error) {
	signer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating leaf key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		return nil, fmt.Errorf("marshalling leaf key: %w", err)
	}
	return &leafKey{
		signer: signer,
		pem:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
	}, nil
}
