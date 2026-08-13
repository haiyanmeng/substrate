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
# The two questions run.sh cannot answer, because both need traffic to stop.
#
#   idle       Does an idle name go on being re-minted? run.sh probes every 10s,
#              which keeps a name hot, so its steady 30s cadence is equally
#              consistent with Envoy re-requesting on its own. If it does, the
#              resource TTL is a minting treadmill and cost stops following
#              traffic.
#
#   reconnect  Does the TTL survive an sdsmint restart? Envoy reconnects and
#              replays what it holds. If the timer were lost with the old
#              stream, the secret would become permanent and the silent-expiry
#              failure would come back on every deploy.
#
# Both run against --leaf-cert-ttl=1m, so the resource TTL is 30s. Results are
# in README.md. Run ./run.sh first: this reuses the binaries and CA it builds.
#
# Usage: ./by-hand.sh [idle|reconnect]     (default: both)

set -uo pipefail
cd "$(dirname "$0")" || exit 1

readonly PROXY=127.0.0.1:18500
readonly ADMIN=127.0.0.1:19000
readonly ENVOY=../__run/envoy
readonly TTL=1m

SDS_PID=""
ENVOY_PID=""

# No pkill, for the reason run.sh gives: an unrelated Envoy may be running on
# this machine. Only the PIDs started here are ever signalled.
cleanup() {
  [[ -n "${SDS_PID}" ]] && kill "${SDS_PID}" 2>/dev/null
  [[ -n "${ENVOY_PID}" ]] && kill "${ENVOY_PID}" 2>/dev/null
  wait 2>/dev/null
  rm -f sdsmint.sock
}
trap cleanup EXIT

start_sds() {
  local log="$1"
  ./bin/atenet sdsmint \
    --uds-path ./sdsmint.sock \
    --ca-pool-path ./pool.json \
    --ca-id mitm \
    --leaf-cert-ttl "${TTL}" \
    --log-level debug >> "${log}" 2>&1 &
  SDS_PID=$!
  for _ in $(seq 40); do
    [[ -S ./sdsmint.sock ]] && return 0
    sleep 0.25
  done
  echo "sdsmint never bound its socket"; return 1
}

start_envoy() {
  "${ENVOY}" -c envoy.yaml --log-level warn > "$1" 2>&1 &
  ENVOY_PID=$!
  for _ in $(seq 60); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' "${ADMIN}/ready")" == "200" ]] && return 0
    sleep 0.5
  done
  echo "envoy never became ready"; return 1
}

# serial_of prints the 12-char leaf serial one handshake to SNI $1 is served.
serial_of() {
  ./bin/probe --proxy "${PROXY}" --sni "$1" --ca ./ca.pem \
    | jq -r 'if .handshake then .serial[0:12] else "NO HANDSHAKE: \(.error)" end'
}

mints_for() {
  grep -c "certificate issued.*host=$1 " "$2"
}

# idle probes a name once, then leaves it alone for well over three TTL periods.
# One mint means Envoy re-subscribes lazily, at handshake time, and an idle name
# costs nothing. Several means it re-requests on a timer of its own.
idle() {
  local log=idle-sds.log sni="idle-${RANDOM}.example.com"
  : > "${log}"
  start_sds "${log}" || return 1
  start_envoy idle-envoy.log || return 1

  echo "  idle: ${sni}   (--leaf-cert-ttl=${TTL} -> resource ttl 30s)"
  echo "    one probe, then silence : $(serial_of "${sni}")"
  echo "    ...110s of no traffic, nearly four TTL periods..."
  sleep 110
  echo "    mints during that window: $(mints_for "${sni}" "${log}")   <- 1 means idle names are dropped, not re-minted"
  echo "    probe again             : $(serial_of "${sni}")   <- a new serial means the drop happened and this re-subscribed"

  cleanup
  SDS_PID=""; ENVOY_PID=""
}

# reconnect kills sdsmint mid-life and watches whether the name keeps being
# refreshed afterwards.
reconnect() {
  local log=reconnect-sds.log sni="rc-${RANDOM}.example.com"
  : > "${log}"
  start_sds "${log}" || return 1
  start_envoy reconnect-envoy.log || return 1

  echo "  reconnect: ${sni}   (--leaf-cert-ttl=${TTL} -> resource ttl 30s)"
  echo "    t=0s  before restart : $(serial_of "${sni}")"

  kill "${SDS_PID}"; wait "${SDS_PID}" 2>/dev/null; rm -f sdsmint.sock
  sleep 2
  start_sds "${log}" || return 1
  # Long enough for Envoy to reconnect, short of the 30s TTL. A new serial here
  # is not the TTL firing; it is the reconnect itself re-subscribing.
  sleep 8
  echo "    t=~12s after restart : $(serial_of "${sni}")   <- ttl cannot have fired yet"
  sleep 40
  echo "    t=~52s past the ttl  : $(serial_of "${sni}")   <- a new serial means the cadence resumed"
  sleep 35
  echo "    t=~87s               : $(serial_of "${sni}")"
  echo "    total mints          : $(mints_for "${sni}" "${log}")"

  cleanup
  SDS_PID=""; ENVOY_PID=""
}

main() {
  [[ -x ./bin/probe && -f ./pool.json ]] || { echo "run ./run.sh first: this reuses its binaries and CA"; exit 1; }
  case "${1:-both}" in
    idle) idle ;;
    reconnect) reconnect ;;
    both) idle; echo; reconnect ;;
    *) echo "usage: $0 [idle|reconnect]"; exit 1 ;;
  esac
}

main "$@"
