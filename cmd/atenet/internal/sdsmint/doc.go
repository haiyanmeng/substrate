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

// Package sdsmint implements `atenet sdsmint`, a minting SDS server: an Envoy
// Secret Discovery Service that mints a TLS leaf certificate on demand for
// whatever hostname Envoy asks for, where the requested SDS resource name is
// the SNI from the client hello.
//
// The point of the design is that the MITM CA private key lives here, in a
// dedicated service, rather than in every data-plane proxy. Only short-lived
// leaf keys ever transit to Envoy, over a local-only channel (UDS).
//
// This runs as its own container alongside the egress gateway's Envoy rather
// than inside the ext_proc one, even though both are now the same image. The
// separation is what keeps the MITM signing key off the data plane, and what
// lets the pod give the minter its own memory limit.
//
// cmd.go is the cobra wrapper; everything else is the server itself. They are
// one package because the wrapper is the only caller, and splitting them only
// bought an import alias.
package sdsmint
