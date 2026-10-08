#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Checks the fidelity ledger of the benchmark fake data plane.
# tools/fidelity-ledger finds every call the real data plane makes across the
# control-plane boundary, and fails when the ledger lists a different set or
# when an entry or a gap is incomplete.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

LEDGER="docs/benchmarking/fake-dataplane-fidelity.yaml"

if ! go -C tools/fidelity-ledger run . -root "${ROOT}" "${LEDGER}"; then
  cat >&2 <<EOF

FAIL: the fidelity ledger of the fake data plane does not hold.

  If a change added, removed or moved a call between the data plane and
  ate-api-server, classify each new call in ${LEDGER}:
  say how the fakes in cmd/benchmarking/isolate emulate it, name the gap
  that they do not, or say why the benchmark needs no stand-in. Then run
  this script again.
EOF
  exit 1
fi
