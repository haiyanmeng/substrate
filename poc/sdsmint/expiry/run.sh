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

#
# Does the xDS resource TTL keep a minted leaf fresh?
#
# sdsmint does not rotate and does not push. The only thing that replaces a leaf
# is the per-resource TTL it stamps on every response: Envoy runs a timer per
# resource, drops the secret when it fires, and the next handshake for that name
# re-subscribes. This watches that happen, on a 1m --leaf-cert-ttl (the floor
# validateTTL allows) so a run costs minutes instead of a quarter hour. The TTL
# is derived at half the leaf lifetime, so expect a re-mint every ~30s.
#
# It matters because the failure without it is silent, not loud. Before the TTL
# existed this same script showed one name serving one leaf for 91s past its
# notAfter across 17 completed handshakes, with no error anywhere. README.md
# keeps that run, and the older idle-sweep run, as the before pictures.
#
# Every probe reports two things that have to stay apart: whether the handshake
# completed (what an actor sees, verification off) and whether the leaf would
# have verified (where expiry shows up). See probe/main.go.
#
#   serial changes              the TTL fired and the name was re-minted. This
#                               is the behaviour under test.
#   handshake ok, verify fails  Envoy served an expired leaf and the actor,
#                               being unable to verify, used it anyway. This is
#                               the regression the TTL exists to prevent.
#   handshake fails             Envoy refused to serve anything at all.
#
# The last probe uses a name never seen before, as a control: it separates
# "this one name went bad" from "the gateway fell over".
#
# Not covered here, but measured by hand and recorded in README.md: an idle name
# is dropped and not re-minted, and an sdsmint restart re-mints every name Envoy
# holds at once.
#
# Usage: ./run.sh

set -uo pipefail
cd "$(dirname "$0")" || exit 1

readonly SCENARIO=ttl
readonly PROXY=127.0.0.1:18500
readonly ADMIN=127.0.0.1:19000
readonly REPO=../../..
readonly ENVOY=../__run/envoy

# The TTL every scenario mints at, and how far past it to keep probing. 1m is
# the floor cmd.go's validateTTL accepts. PROBE_UNTIL runs well past 2x so a
# refresh that happens late still lands inside the run.
readonly TTL=1m
readonly PROBE_UNTIL=150
readonly PROBE_EVERY=10

SDS_PID=""
ENVOY_PID=""

cleanup() {
  [[ -n "${SDS_PID}" ]] && kill "${SDS_PID}" 2>/dev/null
  [[ -n "${ENVOY_PID}" ]] && kill "${ENVOY_PID}" 2>/dev/null
  wait 2>/dev/null
  rm -f sdsmint.sock
}
trap cleanup EXIT

build() {
  echo "building..."
  # Into bin/ because ./probe and ./genpool are the source directories.
  mkdir -p bin
  go build -C "${REPO}" -o "$(pwd)/bin/atenet" ./cmd/atenet || return 1
  go build -C "${REPO}" -o "$(pwd)/bin/genpool" ./poc/sdsmint/expiry/genpool || return 1
  go build -C "${REPO}" -o "$(pwd)/bin/probe" ./poc/sdsmint/expiry/probe || return 1
  # Regenerated per run: the pool poc/sdsmint/__run left behind expired on
  # 2026-08-03 and CA.Validate refuses it, and a harness that silently reuses a
  # stale CA is worse than one that has none.
  ./bin/genpool --ca-id mitm --pool-out ./pool.json --cert-out ./ca.pem --lifetime 24h || return 1
  [[ -x "${ENVOY}" ]] || { echo "no envoy binary at ${ENVOY}"; return 1; }
  # grep, not head: envoy --version leads with a blank line.
  echo "envoy: $("${ENVOY}" --version 2>&1 | grep -m1 version)"
}

# start brings up sdsmint and Envoy for the scenario named by $1. Logs go to
# <scenario>-sds.log and <scenario>-envoy.log.
start() {
  local name="$1"

  # Deliberately no pkill: this machine may be running an unrelated Envoy, and
  # a harness that kills every envoy on the box to clean up after itself is not
  # one you can run twice. Only the two PIDs started here are ever signalled,
  # by stop and by the EXIT trap. A stale listener shows up as a bind failure,
  # which is louder and safer than a broad kill.
  rm -f sdsmint.sock

  ./bin/atenet sdsmint \
    --uds-path ./sdsmint.sock \
    --ca-pool-path ./pool.json \
    --ca-id mitm \
    --leaf-cert-ttl "${TTL}" \
    --log-level debug > "${name}-sds.log" 2>&1 &
  SDS_PID=$!

  for _ in $(seq 40); do
    [[ -S ./sdsmint.sock ]] && break
    sleep 0.25
  done
  [[ -S ./sdsmint.sock ]] || { echo "sdsmint never bound its socket:"; tail -5 "${name}-sds.log"; return 1; }

  "${ENVOY}" -c envoy.yaml --log-level warn > "${name}-envoy.log" 2>&1 &
  ENVOY_PID=$!
  for _ in $(seq 60); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' "${ADMIN}/ready")" == "200" ]] && break
    sleep 0.25
  done
  [[ "$(curl -s -o /dev/null -w '%{http_code}' "${ADMIN}/ready")" == "200" ]] \
    || { echo "envoy never became ready:"; tail -5 "${name}-envoy.log"; return 1; }
}

stop() {
  [[ -n "${SDS_PID}" ]] && kill "${SDS_PID}" 2>/dev/null
  [[ -n "${ENVOY_PID}" ]] && kill "${ENVOY_PID}" 2>/dev/null
  wait "${SDS_PID}" "${ENVOY_PID}" 2>/dev/null
  SDS_PID=""
  ENVOY_PID=""
}

# probe_line renders one probe of SNI $1 as a table row. The raw JSON goes to
# <scenario>-probes.jsonl so a run can be re-read without rerunning it.
#
# The wording of the last column is load-bearing. It reports a check made on
# the certificate AFTER the handshake already completed -- see probe/main.go --
# and an earlier version labelled it "VERIFY FAILS", which read like the
# connection had failed and inverted the finding for anyone skimming. Nothing
# in this column is a connection outcome; that is the "connected" at the front
# of the line, and it says connected on every row of a healthy run.
probe_line() {
  local sni="$1" log="$2" json
  json=$(./bin/probe --proxy "${PROXY}" --sni "${sni}" --ca ./ca.pem)
  echo "${json}" >> "${log}"
  jq -r '
    if .handshake then
      "connected | serial \(.serial[0:12]) | " +
      (if .seconds_to_expiry > 0
         then "fresh for \(.seconds_to_expiry)s | leaf is valid"
         else "EXPIRED \(-.seconds_to_expiry)s ago | served anyway, actor accepted it"
       end)
    else
      "NO CONNECTION | failed at \(.stage) | \(.error)"
    end' <<<"${json}"
}

# watch_scenario runs the scenario end to end.
watch_scenario() {
  local name="$1"
  local sni="aged-${RANDOM}.example.com"
  local probes="${name}-probes.jsonl"
  : > "${probes}"

  echo
  echo "############################################################"
  echo "# scenario: ${name}   --leaf-cert-ttl=${TTL}"
  echo "#   SNI ${sni}"
  echo "############################################################"

  start "${name}" || return 1

  local start_ts elapsed
  start_ts=$(date +%s)

  printf '\n  %6s  %s\n' "t" "result"
  printf '  %6s  %s\n' "------" "------"
  while :; do
    elapsed=$(( $(date +%s) - start_ts ))
    (( elapsed > PROBE_UNTIL )) && break
    printf '  %5ds  %s\n' "${elapsed}" "$(probe_line "${sni}" "${probes}")"
    sleep "${PROBE_EVERY}"
  done

  # Control: a name Envoy has never subscribed to. If this one works, the
  # gateway is healthy and everything above is about the aged name alone.
  printf '\n  control (never-before-seen SNI)\n         %s\n' \
    "$(probe_line "control-${RANDOM}.example.com" "${probes}")"

  echo
  echo "  --- verdict ---"
  # One distinct leaf means the TTL never fired and the name is stuck on its
  # first certificate; an expired leaf served at all means it fired too late.
  jq -rs --arg s "${sni}" '
    map(select(.sni == $s and .handshake)) as $p
    | ($p | map(.serial) | unique | length) as $leaves
    | ($p | map(select(.seconds_to_expiry <= 0)) | length) as $stale
    | if $stale > 0 then "    FAIL: an expired leaf was served \($stale) time(s)"
      elif $leaves < 2 then "    FAIL: only \($leaves) leaf over the whole run; nothing re-minted"
      else "    ok: \($leaves) leaves across \($p | length) handshakes, never an expired one"
      end' "${probes}"

  echo "  --- distinct leaves served for ${sni} ---"
  jq -r --arg s "${sni}" 'select(.sni == $s and .handshake) | .serial' "${probes}" \
    | sort -u | sed 's/^/    /'

  echo "  --- what sdsmint did ---"
  printf '    certificates issued: %s\n' "$(grep -c 'certificate issued' "${name}-sds.log")"
  grep -iE 'certificate issued|removed' "${name}-sds.log" | tail -10 | sed 's/^/    /'

  echo "  --- envoy: secrets it is holding ---"
  curl -s "${ADMIN}/certs" | jq -r '
    .certificates[]? | .cert_chain[]? |
    "    \(.subject_alt_names[]?.dns // "?")  days_until_expiration=\(.days_until_expiration // "?")  \(.expiration_time // "?")"
  ' 2>/dev/null | head -20

  echo "  --- ssl / on-demand stats (nonzero only) ---"
  curl -s "${ADMIN}/stats" \
    | grep -iE 'on_demand|ssl\.(handshake|connection_error|fail)' \
    | grep -vE ': 0$' | sed 's/^/    /'

  echo "  --- envoy warnings and errors ---"
  grep -iE '\[(error|critical)\]' "${name}-envoy.log" | tail -5 | sed 's/^/    /'

  stop
}

main() {
  build || exit 1
  watch_scenario "${SCENARIO}"
  echo
  echo "done. logs: *-sds.log, *-envoy.log, *-probes.jsonl"
}

main
