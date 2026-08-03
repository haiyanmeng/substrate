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

# Sustained-rate stress test against the real signer.
#
# run-scale.sh phase 3 sweeps several rates for five seconds each, which is
# enough to find where things break and not enough to characterise one rate.
# This runs a single rate long enough for the tail percentiles to mean
# something, and repeats it so a number can be checked against its own spread.
#
# Every connection asks for an SNI nothing has seen, so every one is a cold
# fetch that forces a real P-256 mint. That is the expensive case on purpose:
# a warm cache would measure Envoy's secret lookup, not the minting path.
#
# Envoy and sdsmintd are restarted between trials, so each trial starts from an
# empty live set rather than inheriting the previous one's memory.
#
#   ./hack/run-stress.sh                          # 500/s for 30s, 3 trials
#   RATE=1000 DURATION=60 TRIALS=1 ./hack/run-stress.sh
#
# Results land in __run/stress-results.txt.

set -euo pipefail

POC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly POC_DIR
readonly REPO_ROOT="$(cd "${POC_DIR}/../.." && pwd)"
readonly RUN_DIR="${POC_DIR}/__run"
readonly RESULTS="${RUN_DIR}/stress-results.txt"

# shellcheck source=lib.sh
source "${POC_DIR}/hack/lib.sh"

readonly RATE="${RATE:-500}"
readonly DURATION="${DURATION:-30}"
readonly TRIALS="${TRIALS:-3}"
readonly CONCURRENCY="${CONCURRENCY:-2}"
readonly MAX_INFLIGHT="${MAX_INFLIGHT:-2048}"
readonly SNI_FORMAT='s%d.mitm.example'

# DISTINCT=N measures the warm path instead: the trial cycles over N names that
# were fetched before the timer started, so no connection pauses for SDS. Run it
# against a cold trial at the same rate and the difference is what the
# pause-fetch-resume cycle costs.
readonly DISTINCT="${DISTINCT:-0}"

# Names never repeat, across trials or across runs of this script, so a warm
# hit can never be mistaken for a cold one. The offset keeps this clear of the
# range run-scale.sh works in.
SNI_CURSOR=5000000

cleanup() { stop_envoy; stop_sds; }
trap cleanup EXIT

record() { printf '%s\n' "$*" >>"${RESULTS}"; note "$*"; }

declare -A LOAD
load() {
  local label="$1" count="$2" rate="$3"; shift 3
  local k
  for k in "${!LOAD[@]}"; do unset 'LOAD[${k}]'; done
  for k in handshake_us_p50 handshake_us_p90 handshake_us_p95 handshake_us_p99 \
           handshake_us_max dial_us_p50 dial_us_p99 schedule_lag_us_p99 \
           ok failed dropped attempted rate_achieved client_cpu_s; do
    LOAD["${k}"]=0
  done

  local line
  while IFS= read -r line; do
    LOAD["${line%%=*}"]="${line#*=}"
  done < <("${RUN_DIR}/sdsload" \
    --target "127.0.0.1:${LISTEN_PORT}" \
    --sni-format "${SNI_FORMAT}" \
    --ca "${RUN_DIR}/ca.pem" \
    --count "${count}" \
    --rate "${rate}" \
    --sni-start "${SNI_CURSOR}" \
    --max-inflight "${MAX_INFLIGHT}" \
    --label "${label}" \
    --kv \
    --json-out "${RUN_DIR}/stress-${label}.json" \
    "$@")
  SNI_CURSOR=$((SNI_CURSOR + count))

  # An if rather than `[[ ]] && ...`: as the last command in a function that
  # form returns 1 when the test is false, and under `set -e` the caller dies.
  if [[ -n "${LOAD[warnings]:-}" ]]; then
    finding "load ${label}: ${LOAD[warnings]}"
  fi
}

start_sds_real() {
  "${RUN_DIR}/sdsmintd" \
    --uds "${RUN_DIR}/sdsmint.sock" \
    --ca-pool "${RUN_DIR}/ca-pool.json" \
    --ca-cert-out "${RUN_DIR}/ca.pem" \
    --ca-name-constraint 'mitm.example' \
    --allow '*.mitm.example' \
    --cache-cap 200000 \
    --ttl 30m \
    --metrics-addr "127.0.0.1:${METRICS_PORT}" \
    --log-level warn \
    >>"${RUN_DIR}/sds.log" 2>&1 &
  SDS_PID=$!
  local i
  for i in $(seq 1 120); do
    if ! kill -0 "${SDS_PID}" 2>/dev/null; then
      echo "sdsmintd exited during startup; see ${RUN_DIR}/sds.log" >&2
      return 1
    fi
    [[ -S "${RUN_DIR}/sdsmint.sock" ]] \
      && curl -s --max-time 1 "127.0.0.1:${METRICS_PORT}/healthz" >/dev/null 2>&1 \
      && return 0
    sleep 0.25
  done
  echo "sdsmintd never came up; see ${RUN_DIR}/sds.log" >&2
  return 1
}

sds_metric() {
  curl -s --max-time 5 "127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null \
    | sed -n "s/^ *\"$1\": \(-\?[0-9]*\),\?$/\1/p" | tail -1
}

################################################################################
COUNT=$((RATE * DURATION))

bold "=== sustained stress: ${RATE}/s of distinct SNIs for ${DURATION}s, ${TRIALS} trial(s) ==="
mkdir -p "${RUN_DIR}"
: >"${RESULTS}"
: >"${RUN_DIR}/sds.log"

ensure_envoy
info "Building sdsmintd and sdsload"
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsmintd" ./cmd/sdsmintd )
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsload" ./poc/sdsmint/cmd/sdsload )

if [[ ! -s "${RUN_DIR}/ca.pem" ]]; then
  info "Generating a throwaway CA"
  start_sds_real
  stop_sds
fi

record "config rate=${RATE}/s duration=${DURATION}s count=${COUNT} trials=${TRIALS} envoy-concurrency=${CONCURRENCY} max-inflight=${MAX_INFLIGHT} distinct=${DISTINCT}"
record "host cores=$(nproc) loadavg-before='$(cut -d' ' -f1-3 /proc/loadavg)'"

declare -a P50S P95S
for trial in $(seq 1 "${TRIALS}"); do
  bold ""
  bold "--- trial ${trial}/${TRIALS} ---"

  require_clean_ports
  start_sds_real
  start_envoy envoy-scale.yaml "${CONCURRENCY}"

  # A short warmup so the measured window excludes process startup, JIT of the
  # first few handshakes, and Envoy's initial SDS connection. In warm mode it
  # doubles as the fill: it fetches exactly the names the trial will reuse, so
  # the measured window contains no cold fetch at all.
  warm_start="${SNI_CURSOR}"
  fill=$(( DISTINCT > 0 ? DISTINCT : 200 ))
  load "warmup-${trial}" "${fill}" 100
  note "warmup: ok=${LOAD[ok]} failed=${LOAD[failed]} p50=${LOAD[handshake_us_p50]}us"

  before_mints="$(sds_metric mints_issued)"
  if [[ "${DISTINCT}" -gt 0 ]]; then
    load "t${trial}" "${COUNT}" "${RATE}" --sni-start "${warm_start}" --distinct "${DISTINCT}"
  else
    load "t${trial}" "${COUNT}" "${RATE}"
  fi
  after_mints="$(sds_metric mints_issued)"

  sign_avg_us=$(( $(sds_metric sign_nanos_avg) / 1000 ))
  active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  alloc="$(stat_value server.memory_allocated)"
  rss="$(rss_kb "${ENVOY_PID}")"

  record "trial${trial} offered=${RATE}/s achieved=${LOAD[rate_achieved]}/s ok=${LOAD[ok]}/${LOAD[attempted]} failed=${LOAD[failed]} dropped=${LOAD[dropped]}"
  record "trial${trial} handshake p50=${LOAD[handshake_us_p50]}us p90=${LOAD[handshake_us_p90]}us p95=${LOAD[handshake_us_p95]}us p99=${LOAD[handshake_us_p99]}us max=${LOAD[handshake_us_max]}us"
  record "trial${trial} mints=+$((after_mints - before_mints)) sign_avg=${sign_avg_us}us cert_active=${active} envoy_rss=${rss}KB allocated=${alloc}B"
  record "trial${trial} client schedule_lag_p99=${LOAD[schedule_lag_us_p99]}us cpu=${LOAD[client_cpu_s]}s dial_p50=${LOAD[dial_us_p50]}us"
  if [[ -n "${LOAD[failures_alert]:-}${LOAD[failures_handshake_timeout]:-}" ]]; then
    record "trial${trial} failure classes: alert=${LOAD[failures_alert]:-0} handshake-timeout=${LOAD[failures_handshake_timeout]:-0} handshake-eof=${LOAD[failures_handshake_eof]:-0}"
  fi

  P50S+=("${LOAD[handshake_us_p50]}")
  P95S+=("${LOAD[handshake_us_p95]}")

  # The client's own confession. A large scheduling lag means the generator,
  # not the server, was the thing that ran out of room -- and the percentiles
  # above are then measuring this laptop rather than the PoC.
  if [[ "${LOAD[schedule_lag_us_p99]}" -gt 50000 ]]; then
    bad "trial ${trial}: client scheduling lag p99 is ${LOAD[schedule_lag_us_p99]}us; the offered rate was not really delivered"
  elif [[ "${LOAD[failed]}" -gt 0 || "${LOAD[dropped]}" -gt 0 ]]; then
    bad "trial ${trial}: ${LOAD[failed]} failed, ${LOAD[dropped]} dropped"
  else
    ok "trial ${trial}: every connection served at ${LOAD[rate_achieved]}/s"
  fi

  # Confirm the load was what it claimed to be. A cold trial must mint once per
  # connection; a warm trial must not mint at all. Either way the assertion
  # matters, because both failure modes look like a pleasingly fast run.
  minted=$((after_mints - before_mints))
  if [[ "${DISTINCT}" -gt 0 ]]; then
    if [[ "${minted}" -ne 0 ]]; then
      bad "trial ${trial}: warm trial minted ${minted} times -- names were not all warm"
    fi
  elif [[ "${minted}" -lt "${COUNT}" ]]; then
    bad "trial ${trial}: only ${minted} mints for ${COUNT} connections -- some names were warm"
  fi

  stop_envoy
  stop_sds
done

bold ""
bold "=== summary ==="
record "loadavg-after='$(cut -d' ' -f1-3 /proc/loadavg)'"
record "p50 across trials: ${P50S[*]}us"
record "p95 across trials: ${P95S[*]}us"
bold ""
bold "results: ${RESULTS}    ${PASS} passed, ${FAIL} failed"
[[ "${FAIL}" -eq 0 ]]
