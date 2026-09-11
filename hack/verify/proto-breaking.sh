#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Rejects changes to our protos that break binary wire compatibility.
#
# ateapi resources are persisted as binary protobuf in bytea columns, and the
# storage layer decodes them with proto.UnmarshalOptions{DiscardUnknown: true}
# (cmd/ateapi/internal/store/atepg/atepg.go). Binary protobuf keys every field
# on its field number, so reusing or renumbering a field makes a stored row
# decode into the wrong field, and DiscardUnknown then destroys the original
# bytes on the next read-modify-write. Nothing surfaces at runtime. This check
# is where that has to be caught, at review time.
#
# The rules that matter, and what they permit:
#
#   renumber a field                     rejected
#   delete a field without `reserved`    rejected
#   delete a field with `reserved <n>`   allowed  (the correct retirement)
#   change a field's type or label       rejected
#   rename a field, keeping its number   allowed  (names are not on the wire)
#   rename a message                     rejected (see below)
#
# Renaming a message is wire-compatible in fact, but buf compares fields by
# their fully qualified type name and cannot see that the old and new messages
# are structurally identical, so it reports a type change. Renaming one is a
# deliberate act; take the one-time bypass below and say so in the commit.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

# Deliberate wire breaks are possible before the API is released. Set this to
# land one, and explain the break in the commit message. It stops applying by
# itself: once the change is on main it becomes the baseline.
if [ -n "${ALLOW_PROTO_BREAKING:-}" ]; then
  echo "proto-breaking: skipped, ALLOW_PROTO_BREAKING is set" >&2
  exit 0
fi

# Compare against the point this branch left main, not main's tip, so a
# breaking change that landed on main independently is not reported here as
# though this branch introduced it.
BASELINE_REF="${PROTO_BREAKING_BASELINE:-}"
if [ -z "${BASELINE_REF}" ]; then
  for CANDIDATE in origin/main main; do
    if git rev-parse --verify --quiet "${CANDIDATE}^{commit}" >/dev/null; then
      BASELINE_REF="${CANDIDATE}"
      break
    fi
  done
fi
if [ -z "${BASELINE_REF}" ]; then
  echo "proto-breaking: no baseline; fetch main (git fetch origin main) or set PROTO_BREAKING_BASELINE" >&2
  exit 1
fi

BASELINE="$(git merge-base HEAD "${BASELINE_REF}")"

# The input is the working tree, so an incompatible edit is reported before it
# is ever committed. --against-config points the baseline at the buf.yaml we
# have now, because commits older than that file carry no buf config of their
# own and would otherwise be read with buf's defaults.
BIN="$("${ROOT}"/hack/run-tool.sh --print-bin-path buf)"
exec "${BIN}" breaking --against ".git#ref=${BASELINE}" --against-config buf.yaml
