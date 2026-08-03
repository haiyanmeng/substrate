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
#
# Scalability and performance measurement for Envoy's on_demand_secret
# certificate selector. run-poc.sh proves the design works; this asks how far
# it goes.
#
# Phases:
#   0  control          static certificate, same everything else -- the floor
#   1  bytes/secret     memory against the number of live secrets
#   2  lookup           does first contact slow down as the live set grows?
#   3  saturation       ramp new-SNI arrival rate against the real signer
#   4  warm path        per-connection cost of the selector on a cache hit
#   5  rotation storm   cost of a rotation tick at N live names
#   6  idle reclamation does memory come back, with and without --idle?
#   7  reconnect        cost of Envoy replaying its live set after SDS restarts
#   8  realism          one run through dynamic_forward_proxy (needs internet)
#   9  hold at NAMES    CPU and memory of both processes at NAMES live secrets
#  10  rotate at NAMES  the same, with --rotate --ttl 5m running underneath
#
# Phases 0-7 run the null minter, which serves pre-signed leaves so that what
# is measured is Envoy and not a P-256 signing loop -- a mint costs ~375us,
# which would swamp everything else. Phase 3 is the exception: the signer is
# its subject. Phases 8, 9 and 10 are opt-in and not in the default set.
#
# Phases 9 and 10 are the only ones that measure sdsmintd rather than Envoy, and
# the only ones that read CPU. They are sized by $NAMES (default 100000) instead
# of by --full, and at that size they need several GB of RAM and about 15
# minutes for the pair.
#
# Usage:
#   ./poc/sdsmint/hack/run-scale.sh                  # quick sweep, phases 0-7
#   ./poc/sdsmint/hack/run-scale.sh --phases 0,1,6   # a subset
#   ./poc/sdsmint/hack/run-scale.sh --full           # production-scale N, 30m idle
#   ./poc/sdsmint/hack/run-scale.sh --keep           # leave the processes up
#   NAMES=100000 ./poc/sdsmint/hack/run-scale.sh --phases 9,10
#
# Environment overrides for phases 9 and 10:
#   NAMES=100000        live secrets to hold
#   FILL_RATE=500       new SNIs per second during the fill
#   ROTATE_WATCH=420    seconds to watch rotation in phase 10

set -o errexit
set -o nounset
set -o pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly POC_DIR="$(dirname "${SCRIPT_DIR}")"
readonly REPO_ROOT="$(cd "${POC_DIR}/../.." && pwd)"
readonly RUN_DIR="${POC_DIR}/__run"

source "${SCRIPT_DIR}/lib.sh"

FULL=false
KEEP=false
PHASES="0,1,2,3,4,5,6,7"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --full) FULL=true ;;
    --keep) KEEP=true ;;
    --phases) PHASES="$2"; shift ;;
    --phases=*) PHASES="${1#*=}" ;;
    # The header comment IS the help text. Printed by matching the block rather
    # than by a line range, so adding a phase to the list above does not
    # silently truncate --help.
    -h|--help) sed -n '/^# Scalability/,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^#\( \|$\)//'; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
  shift
done

# The quick sweep is meant to finish in a few minutes and still show the shape
# of every curve. --full is the one whose numbers are worth quoting.
if [[ "${FULL}" == true ]]; then
  N_STEPS=(1000 5000 20000 50000)
  RATES=(10 50 100 250 500 1000)
  ROTATE_NS=(1000 5000)
  IDLE_SECONDS=1800
  IDLE_TIMEOUT=300s
  STEADY_COUNT=20000
else
  N_STEPS=(200 1000 3000)
  RATES=(50 200 500)
  ROTATE_NS=(500)
  IDLE_SECONDS=120
  IDLE_TIMEOUT=30s
  STEADY_COUNT=3000
fi

# Phases 9 and 10 are sized by NAMES rather than by --full, because they are not
# part of the sweep: they answer one question at one number, and that number is
# the argument. 100000 fills in about 3.5 minutes at 500/s and is projected to
# cost several GB of Envoy -- override it downward for a smoke run.
NAMES="${NAMES:-100000}"
FILL_RATE="${FILL_RATE:-500}"
# Two full rotation intervals. At --ttl 5m a stream rotates at 2/3 TTL = 200s,
# so 420s guarantees every stream has ticked at least twice regardless of where
# in the fill window it opened.
ROTATE_WATCH="${ROTATE_WATCH:-420}"

readonly RESULTS="${RUN_DIR}/scale-results.txt"
readonly SNI_FORMAT='h%d.mitm.example'
# Every phase walks a disjoint slice of the synthetic host space, so a name is
# never accidentally warm because an earlier phase touched it.
SNI_CURSOR=0

wants() { [[ ",${PHASES}," == *",$1,"* ]]; }

cleanup() {
  if [[ "${KEEP}" == true ]]; then
    bold "--keep set; leaving sdsmintd (pid ${SDS_PID:-none}) and envoy (pid ${ENVOY_PID:-none}) running."
    note "envoy admin: http://127.0.0.1:${ADMIN_PORT}   sds metrics: http://127.0.0.1:${METRICS_PORT}/metrics"
    return
  fi
  stop_envoy
  stop_sds
}
trap cleanup EXIT

record() { printf '%s\n' "$*" >>"${RESULTS}"; note "$*"; }

# sds_metric KEY reads one counter out of sdsmintd's /metrics. The document is
# flat integers by construction, which is why a sed line is enough and the
# harness needs no JSON parser.
sds_metric() {
  local v
  v="$(curl -s --max-time 5 "127.0.0.1:${METRICS_PORT}/metrics" 2>/dev/null \
    | sed -n "s/^ *\"$1\": \(-\?[0-9]*\),\?$/\1/p" | tail -1)"
  if [[ "${v}" =~ ^-?[0-9]+$ ]]; then printf '%s\n' "${v}"; else printf '0\n'; fi
}

# start_sds_null brings up the measurement server: pre-signed leaves, no
# per-request signing, counters on, and the audit log silenced because at a
# thousand requests a second the logging is itself a bottleneck.
start_sds_null() {
  "${RUN_DIR}/sdsmintd" \
    --uds "${RUN_DIR}/sdsmint.sock" \
    --ca-pool "${RUN_DIR}/ca-pool.json" \
    --ca-cert-out "${RUN_DIR}/ca.pem" \
    --ca-name-constraint 'mitm.example,example.com' \
    --allow '*.mitm.example' \
    --unsafe-null-minter \
    --null-host '*.mitm.example' \
    --metrics-addr "127.0.0.1:${METRICS_PORT}" \
    --log-level warn \
    "$@" \
    >>"${RUN_DIR}/sds.log" 2>&1 &
  SDS_PID=$!
  wait_for_sds
}

# start_sds_real is phase 3's server: the actual signer, with a cache far above
# the working set so that a re-signed name is a real finding rather than an
# artefact of the default 256-entry cap.
start_sds_real() {
  "${RUN_DIR}/sdsmintd" \
    --uds "${RUN_DIR}/sdsmint.sock" \
    --ca-pool "${RUN_DIR}/ca-pool.json" \
    --ca-cert-out "${RUN_DIR}/ca.pem" \
    --ca-name-constraint 'mitm.example,example.com' \
    --allow '*.mitm.example' \
    --allow 'example.com' \
    --cache-cap 200000 \
    --ttl 30m \
    --metrics-addr "127.0.0.1:${METRICS_PORT}" \
    --log-level warn \
    "$@" \
    >>"${RUN_DIR}/sds.log" 2>&1 &
  SDS_PID=$!
  wait_for_sds
}

# wait_for_sds blocks until the server started by the caller is serving.
#
# The liveness check on SDS_PID is the important half. Without it, a leftover
# server from an earlier run answers /healthz on the same port and this returns
# success for a process that has already exited -- and the caller goes on to
# SIGTERM whatever pid it recorded, which at that moment may be a brand-new
# server halfway through writing the control leaf. That is not hypothetical:
# it is how this harness produced a zero-byte leaf.pem.
wait_for_sds() {
  local i
  for i in $(seq 1 120); do
    if ! kill -0 "${SDS_PID}" 2>/dev/null; then
      echo "sdsmintd (pid ${SDS_PID}) exited during startup; see ${RUN_DIR}/sds.log" >&2
      tail -20 "${RUN_DIR}/sds.log" >&2
      return 1
    fi
    if [[ -S "${RUN_DIR}/sdsmint.sock" ]] \
      && curl -s --max-time 1 "127.0.0.1:${METRICS_PORT}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  echo "sdsmintd never came up; see ${RUN_DIR}/sds.log" >&2
  tail -20 "${RUN_DIR}/sds.log" >&2
  return 1
}

# load LABEL COUNT RATE [extra sdsload flags...] drives the listener and leaves
# the flattened result in LOAD_<field> variables.
#
# It advances SNI_CURSOR by COUNT unless the caller pinned --distinct, so
# successive calls hit names Envoy has never seen. Callers that want warm names
# pass --sni-start themselves.
declare -A LOAD
declare -A COLD_P50
load() {
  local label="$1" count="$2" rate="$3"; shift 3
  local start="${SNI_CURSOR}"
  local args=(--target "127.0.0.1:${LISTEN_PORT}"
              --sni-format "${SNI_FORMAT}"
              --ca "${RUN_DIR}/ca.pem"
              --count "${count}"
              --label "${label}"
              --kv
              --json-out "${RUN_DIR}/load-${label}.json")
  [[ "${rate}" != "0" ]] && args+=(--rate "${rate}")
  # A caller-supplied --sni-start wins; otherwise take the next free slice.
  if [[ "$*" != *"--sni-start"* ]]; then
    args+=(--sni-start "${start}")
    SNI_CURSOR=$((SNI_CURSOR + count))
  fi

  # Pre-seed the keys the phases read. sdsload omits the latency percentiles
  # entirely when nothing succeeded, and under `set -u` an unset key would abort
  # the run at exactly the moment the interesting failure happened.
  local k
  for k in "${!LOAD[@]}"; do unset 'LOAD[${k}]'; done
  for k in handshake_us_p50 handshake_us_p90 handshake_us_p99 handshake_us_max \
           dial_us_p50 dial_us_p99 request_us_p50 schedule_lag_us_p99 \
           ok failed dropped attempted rate_achieved client_cpu_s; do
    LOAD["${k}"]=0
  done

  local line key value
  while IFS= read -r line; do
    key="${line%%=*}"
    value="${line#*=}"
    LOAD["${key}"]="${value}"
  done < <("${RUN_DIR}/sdsload" "${args[@]}" "$@")

  if [[ -n "${LOAD[warnings]:-}" ]]; then
    finding "load ${label}: ${LOAD[warnings]}"
  fi
}

# expect_all_served aborts a phase whose setup silently failed.
#
# This exists because of a real miss: before the SDS cluster's circuit breakers
# were raised, everything past the 1024th name failed its handshake, the ramp
# reported a *flatter* memory curve as a result, and it read as good news. A
# fill step that did not fill invalidates every number derived from it, so it
# has to be an assertion and not a log line.
expect_all_served() {
  local what="$1"
  if [[ "${LOAD[failed]}" -eq 0 && "${LOAD[dropped]}" -eq 0 ]]; then
    return 0
  fi
  bad "${what}: ${LOAD[failed]} of ${LOAD[attempted]} connections failed (${LOAD[dropped]} dropped) -- numbers below this line are not trustworthy"
  local k
  for k in "${!LOAD[@]}"; do
    [[ "${k}" == failures_* ]] && note "    ${k#failures_} = ${LOAD[${k}]}"
  done
  return 1
}

# sds_pprof_int PROFILE PATTERN reads one integer out of a pprof debug=1
# document. Used for the two things /metrics does not carry and cannot
# reasonably be made to carry: the goroutine count and the Go runtime's own
# memory accounting.
#
# This matters at 100k names because the per-stream handler in server.go
# allocates three goroutines and up to two tickers PER STREAM, so the runtime's
# own overhead is a first-order term rather than a rounding error.
sds_pprof_int() {
  local v
  v="$(curl -s --max-time 10 "127.0.0.1:${METRICS_PORT}/debug/pprof/$1?debug=1" 2>/dev/null \
    | grep -m1 -oP "$2" || true)"
  if [[ "${v}" =~ ^[0-9]+$ ]]; then printf '%s\n' "${v}"; else printf '0\n'; fi
}
sds_goroutines() { sds_pprof_int goroutine 'goroutine profile: total \K\d+'; }
sds_heap_sys()   { sds_pprof_int heap '^# Sys = \K\d+'; }
sds_heap_alloc() { sds_pprof_int heap '^# HeapAlloc = \K\d+'; }

# big_sample prints one whitespace-separated row describing both processes at
# this instant:
#
#   envoy_rss_kb envoy_alloc_b envoy_cpu_ms sds_rss_kb sds_sys_b sds_cpu_ms
#   goroutines cert_active names_live streams_live mints_issued
#
# Read as a row rather than as eleven separate calls because each admin and
# pprof fetch costs a round trip, and at 100k live secrets a /stats scrape is
# not instant -- sampling them one at a time would smear a "sample" across
# seconds of a moving target.
big_sample() {
  local stats
  stats="$(curl -s --max-time 30 "127.0.0.1:${ADMIN_PORT}/stats" 2>/dev/null || true)"
  local ecpu scpu
  ecpu="$(cpu_ms "${ENVOY_PID}")"
  scpu="$(cpu_ms "${SDS_PID}")"
  local pick
  pick() {
    local v
    v="$(printf '%s\n' "${stats}" | awk -F': ' -v n="$1" '$1 == n {print $2}' | tail -1)"
    [[ "${v}" =~ ^[0-9]+$ ]] && printf '%s' "${v}" || printf '0'
  }
  printf '%s %s %s %s %s %s %s %s %s %s %s\n' \
    "$(rss_kb "${ENVOY_PID}")" "$(pick server.memory_allocated)" "${ecpu}" \
    "$(rss_kb "${SDS_PID}")" "$(sds_heap_sys)" "${scpu}" \
    "$(sds_goroutines)" \
    "$(pick listener.mitm.on_demand_secret.cert_active)" \
    "$(sds_metric names_live)" "$(sds_metric streams_live)" \
    "$(sds_metric mints_issued)"
}

# envoy_mem prints "rss_kb allocated_bytes" as one line.
envoy_mem() {
  printf '%s %s\n' "$(rss_kb "${ENVOY_PID}")" "$(stat_value server.memory_allocated)"
}

fresh_envoy() {
  local config="$1" concurrency="${2:-2}"
  stop_envoy
  start_envoy "${config}" "${concurrency}"
}

################################################################################
bold "=== on_demand_secret scalability: Envoy-side phases ==="
mkdir -p "${RUN_DIR}"
: >"${RESULTS}"
: >"${RUN_DIR}/sds.log"

# Every connection is a full handshake with no resumption and the client holds
# them open only briefly, but at a thousand a second the ephemeral port range
# and the fd limit both come into play. Raise what we can and say what we got.
ulimit -n 65536 2>/dev/null || true
note "open file limit: $(ulimit -n)   ephemeral ports: $(cat /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null || echo unknown)"
require_clean_ports

info "Building sdsmintd and sdsload"
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsmintd" ./cmd/sdsmintd )
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsload" ./poc/sdsmint/cmd/sdsload )

info "Ensuring Envoy ${ENVOY_VERSION}"
ensure_envoy
cp "${POC_DIR}"/testdata/envoy-*.yaml "${RUN_DIR}/"
rm -f "${RUN_DIR}/ca.pem" "${RUN_DIR}/ca-pool.json"

# One CA and one pre-signed leaf for the whole run, so the control and the
# on-demand arm serve certificates that are byte-identical.
info "Generating the CA and the control leaf"
rm -f "${RUN_DIR}/leaf.pem" "${RUN_DIR}/leaf-key.pem"
start_sds_null --null-cert-out "${RUN_DIR}"
stop_sds
if [[ ! -s "${RUN_DIR}/leaf.pem" || ! -s "${RUN_DIR}/leaf-key.pem" ]]; then
  echo "the control leaf was not written; see ${RUN_DIR}/sds.log" >&2
  exit 1
fi
note "ca: ${RUN_DIR}/ca.pem   control leaf: ${RUN_DIR}/leaf.pem"

BASE_HS_P50=0
BASE_HS_P99=0
BASE_RSS=0

################################################################################
if wants 0; then
  bold ""
  bold "--- Phase 0: control (static certificate, no selector) ---"
  note "Everything below is the difference from this. A handshake number on its"
  note "own is mostly TLS; only the delta is attributable to on-demand SDS."

  fresh_envoy envoy-static.yaml 2
  idle_rss="$(rss_kb "${ENVOY_PID}")"

  # Deliberately the same shape as phase 4's warm run -- same count, same rate,
  # same --distinct, same --request. An earlier version drove the control at a
  # different rate and the two were not comparable: the control came out slower
  # than the on-demand arm, which read as the selector having negative cost.
  load control "${STEADY_COUNT}" 500 --distinct 500 --request
  BASE_HS_P50="${LOAD[handshake_us_p50]}"
  BASE_HS_P99="${LOAD[handshake_us_p99]}"
  BASE_RSS="$(rss_kb "${ENVOY_PID}")"

  record "phase0 ok=${LOAD[ok]}/${LOAD[attempted]} failed=${LOAD[failed]} rate=${LOAD[rate_achieved]}/s"
  record "phase0 handshake p50=${BASE_HS_P50}us p90=${LOAD[handshake_us_p90]}us p99=${BASE_HS_P99}us max=${LOAD[handshake_us_max]}us"
  record "phase0 rss idle=${idle_rss}KB after=${BASE_RSS}KB  client_cpu=${LOAD[client_cpu_s]}s"

  # Every connection is a full handshake, so the client pays a P-256 verify per
  # connection and can be the slower party. If it is, differences of a few
  # hundred microseconds between phases are client scheduling, not Envoy.
  cpu_per_conn_us=$(( $(awk "BEGIN{printf \"%d\", ${LOAD[client_cpu_s]} * 1000000}") / (LOAD[ok] > 0 ? LOAD[ok] : 1) ))
  record "phase0 client cost: ${cpu_per_conn_us}us CPU per connection"
  if [[ "${cpu_per_conn_us}" -gt $((BASE_HS_P50 / 2)) ]]; then
    finding "the load generator spends ${cpu_per_conn_us}us of CPU per connection against a ${BASE_HS_P50}us handshake -- differences below ~1ms between phases are client noise, not signal"
  fi
  if [[ "${LOAD[failed]}" -eq 0 ]]; then
    ok "the control served every connection"
  else
    bad "the control itself failed ${LOAD[failed]} connections (${LOAD[failures_alert]:-0} alerts); later phases are not interpretable"
  fi
fi

################################################################################
# Phases 1 and 2 share one ramp: 2 asks how first contact behaves at each of
# the live-set sizes 1 builds up, and rebuilding the state twice would double
# the run for nothing.
if wants 1 || wants 2; then
  bold ""
  bold "--- Phases 1 and 2: memory and lookup cost against the live secret count ---"

  start_sds_null
  fresh_envoy envoy-scale.yaml 2

  empty_rss="$(rss_kb "${ENVOY_PID}")"
  empty_alloc="$(stat_value server.memory_allocated)"
  record "phase1 baseline live=0 rss=${empty_rss}KB allocated=${empty_alloc}B"

  prev_n=0
  prev_rss="${empty_rss}"
  prev_alloc="${empty_alloc}"
  for n in "${N_STEPS[@]}"; do
    step=$((n - prev_n))
    load "ramp-${n}" "${step}" 200
    expect_all_served "phase1 fill to ${n}" || break
    sleep 2

    active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
    requested="$(stat_value listener.mitm.on_demand_secret.cert_requested)"
    live="$(sds_metric names_live)"
    streams="$(sds_metric streams_live)"
    read -r rss alloc <<<"$(envoy_mem)"

    per_rss=$(( (rss - prev_rss) * 1024 / step ))
    per_alloc=$(( (alloc - prev_alloc) / step ))
    record "phase1 live=${n} cert_active=${active} cert_requested=${requested} sds_names_live=${live} sds_streams_live=${streams}"
    record "phase1 live=${n} rss=${rss}KB allocated=${alloc}B  marginal: ${per_rss}B/secret rss, ${per_alloc}B/secret allocated"

    if wants 2; then
      # A slice of names nothing has touched, so this is genuine first contact
      # at a live-set size of n.
      load "cold-at-${n}" 40 10
      record "phase2 live=${n} cold handshake p50=${LOAD[handshake_us_p50]}us p99=${LOAD[handshake_us_p99]}us failed=${LOAD[failed]}"
      COLD_P50[${n}]="${LOAD[handshake_us_p50]}"
    fi

    prev_n="${n}"
    prev_rss="${rss}"
    prev_alloc="${alloc}"
  done

  final_active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  final_streams="$(sds_metric streams_live)"
  if [[ "${final_active}" -ge "${N_STEPS[-1]}" ]]; then
    ok "Envoy is holding all ${final_active} secrets at once, so the footprint above is the real one"
  else
    finding "cert_active is ${final_active} but ${N_STEPS[-1]} names were touched -- something is evicting, which contradicts the code"
  fi

  # The subscription model was assumed, wrongly, to be one stream carrying many
  # names. It is not, and almost every other prediction in this file followed
  # from that assumption, so it is checked rather than described.
  if [[ "${final_streams}" -ge "${final_active}" && "${final_active}" -gt 1 ]]; then
    finding "ANSWER: Envoy opens ONE DELTA_GRPC stream PER SECRET (${final_streams} streams for ${final_active} secrets), all on one connection"
    finding "  => the live secret count is a concurrent-request count against the SDS cluster; see circuit_breakers in envoy-scale.yaml"
  else
    finding "ANSWER: ${final_streams} SDS streams carry ${final_active} secrets -- subscriptions are multiplexed"
  fi

  if wants 2; then
    first="${COLD_P50[${N_STEPS[0]}]}"
    last="${COLD_P50[${N_STEPS[-1]}]}"
    record "phase2 cold p50 at ${N_STEPS[0]} live = ${first}us; at ${N_STEPS[-1]} live = ${last}us"
    if [[ "${first}" -gt 0 && "${last}" -lt $((first * 2)) ]]; then
      finding "ANSWER: first-contact cost is flat in the live-secret count (${first}us -> ${last}us)"
    else
      finding "ANSWER: first contact DEGRADES with the live set (${first}us -> ${last}us) -- lookup is not O(1)"
    fi
  fi
fi

################################################################################
if wants 3; then
  bold ""
  bold "--- Phase 3: mint saturation (the real signer) ---"
  note "The only phase that runs the actual CA. Everything else would be"
  note "measuring a P-256 sign, which at ~375us dwarfs anything Envoy does."

  stop_envoy
  stop_sds
  start_sds_real
  start_envoy envoy-scale.yaml 2

  knee=""
  p99_knee=""
  first_p99=0
  for rate in "${RATES[@]}"; do
    count=$((rate * 5))
    [[ "${count}" -lt 200 ]] && count=200
    before_mints="$(sds_metric mints_issued)"
    load "mint-${rate}" "${count}" "${rate}"
    after_mints="$(sds_metric mints_issued)"
    sign_avg_us=$(( $(sds_metric sign_nanos_avg) / 1000 ))

    record "phase3 rate=${rate}/s achieved=${LOAD[rate_achieved]}/s ok=${LOAD[ok]} failed=${LOAD[failed]} dropped=${LOAD[dropped]}"
    record "phase3 rate=${rate}/s handshake p50=${LOAD[handshake_us_p50]}us p99=${LOAD[handshake_us_p99]}us max=${LOAD[handshake_us_max]}us mints=+$((after_mints - before_mints)) sign_avg=${sign_avg_us}us"
    [[ -n "${LOAD[failures_alert]:-}${LOAD[failures_handshake_timeout]:-}" ]] &&
      record "phase3 rate=${rate}/s failure classes: alert=${LOAD[failures_alert]:-0} handshake-timeout=${LOAD[failures_handshake_timeout]:-0} handshake-eof=${LOAD[failures_handshake_eof]:-0}"

    # The throughput knee is the first rate the client could not actually
    # deliver, or the first one that produced failures. Both mean the same
    # thing: past here the offered load is no longer being served.
    if [[ -z "${knee}" ]]; then
      if [[ "${LOAD[failed]}" -gt 0 || "${LOAD[dropped]}" -gt 0 ]]; then
        knee="${rate}"
      elif awk "BEGIN{exit !(${LOAD[rate_achieved]} < ${rate} * 0.8)}"; then
        knee="${rate}"
      fi
    fi

    # Throughput is the wrong thing to watch alone. A queue absorbs offered load
    # long after it has stopped serving it promptly, so latency degrades well
    # before the connection count does -- and for a TLS handshake, latency IS
    # the service. Track where the tail departs from the lightest-load tail.
    if [[ "${first_p99}" -eq 0 ]]; then
      first_p99="${LOAD[handshake_us_p99]}"
    elif [[ -z "${p99_knee}" && "${LOAD[handshake_us_p99]}" -gt $((first_p99 * 4)) ]]; then
      p99_knee="${rate}"
      p99_knee_us="${LOAD[handshake_us_p99]}"
    fi
  done

  if [[ -n "${knee}" ]]; then
    finding "ANSWER: new-SNI throughput knees at ~${knee}/s on this box"
  else
    finding "ANSWER: every offered rate up to ${RATES[-1]}/s was fully served with no failures"
  fi
  if [[ -n "${p99_knee}" ]]; then
    finding "ANSWER: but the tail goes first -- handshake p99 is ${first_p99}us at ${RATES[0]}/s and ${p99_knee_us}us at ${p99_knee}/s. Capacity planning should use the p99 knee, not the throughput one."
  else
    finding "ANSWER: handshake p99 stayed within 4x of its light-load value across the whole ramp"
  fi
fi

################################################################################
if wants 4; then
  bold ""
  bold "--- Phase 4: warm-path overhead (cache hit, no SDS traffic) ---"
  note "What the selector costs per connection once the secret is already held."
  note "Concurrency is swept because the secret cache is shared across workers,"
  note "so this is where contention on it would show."

  stop_envoy
  stop_sds
  start_sds_null

  for conc in 1 4; do
    start_envoy envoy-scale.yaml "${conc}"
    warm_start="${SNI_CURSOR}"
    load "warm-fill-c${conc}" 500 200
    expect_all_served "phase4 warm fill (concurrency ${conc})" || { stop_envoy; continue; }
    load "warm-c${conc}" "${STEADY_COUNT}" 500 --distinct 500 --sni-start "${warm_start}" --request

    record "phase4 concurrency=${conc} ok=${LOAD[ok]} failed=${LOAD[failed]} rate=${LOAD[rate_achieved]}/s"
    record "phase4 concurrency=${conc} warm handshake p50=${LOAD[handshake_us_p50]}us p90=${LOAD[handshake_us_p90]}us p99=${LOAD[handshake_us_p99]}us"
    if [[ "${BASE_HS_P50}" -gt 0 ]]; then
      d50=$((LOAD[handshake_us_p50] - BASE_HS_P50))
      d99=$((LOAD[handshake_us_p99] - BASE_HS_P99))
      record "phase4 concurrency=${conc} overhead vs control: p50 ${d50}us, p99 ${d99}us"
      # A negative or near-zero delta does not mean the selector is free; it
      # means the difference is smaller than what this harness can resolve.
      # Reporting "-278us of overhead" as though it were a measurement would be
      # worse than reporting nothing.
      if [[ "${d50}" -lt 200 && "${d50}" -gt -200 ]]; then
        finding "ANSWER: warm-path overhead at concurrency ${conc} is below the harness's resolution (p50 delta ${d50}us, both arms ~${BASE_HS_P50}us) -- a cache hit costs nothing measurable"
      else
        finding "ANSWER: warm-path overhead at concurrency ${conc} is p50 +${d50}us over a static certificate"
      fi
    else
      note "no phase 0 baseline in this run, so the overhead column is missing; add phase 0"
    fi
    stop_envoy
  done
fi

################################################################################
if wants 5; then
  bold ""
  bold "--- Phase 5: rotation storm ---"
  note "rotateAll re-mints every name a stream is subscribed to and pushes it."
  note "Phase 1 showed a stream carries exactly one name, so a tick is N separate"
  note "single-resource pushes rather than one large batch: the cost is N stream"
  note "wakeups and N signatures, not response size. Cost per tick still scales"
  note "with every host ever contacted, not with the working set."

  for n in "${ROTATE_NS[@]}"; do
    stop_envoy
    stop_sds
    # A 15s TTL rotates every 10s. The pre-signed pool lives for 24h
    # regardless, so this is a push-rate experiment and not an expiry one.
    start_sds_null --rotate --ttl 15s
    start_envoy envoy-scale.yaml 2

    load "rot-fill-${n}" "${n}" 500
    expect_all_served "phase5 fill to ${n}" || continue
    sleep 2
    updated_before="$(stat_value listener.mitm.on_demand_secret.cert_updated)"

    # Serve while the ticks happen: the question is not only what a rotation
    # costs but what it does to connections in flight during one.
    note "holding ${n} live names through ~2 rotation ticks while serving..."
    warm_at="$((SNI_CURSOR - n))"
    load "rot-serve-${n}" 500 25 --distinct "${n}" --sni-start "${warm_at}"

    updated_after="$(stat_value listener.mitm.on_demand_secret.cert_updated)"
    rot_max_ms=$(( $(sds_metric rotation_nanos_max) / 1000000 ))
    rot_avg_ms=$(( $(sds_metric rotation_nanos_avg) / 1000000 ))
    resp_max="$(sds_metric response_bytes_max)"
    rotations="$(sds_metric rotations)"
    nacks="$(sds_metric nacks)"

    rot_us=$(( $(sds_metric rotation_nanos_max) / 1000 ))
    record "phase5 live=${n} rotations=${rotations} resources=$(sds_metric rotation_resources) cert_updated +$((updated_after - updated_before)) nacks=${nacks}"
    record "phase5 live=${n} per-stream rotation cost avg=${rot_avg_ms}ms max=${rot_max_ms}ms (${rot_us}us)  largest response=${resp_max}B"
    record "phase5 live=${n} serving during rotation: p50=${LOAD[handshake_us_p50]}us p99=${LOAD[handshake_us_p99]}us failed=${LOAD[failed]}"

    # With one name per stream the 4MB gRPC ceiling is not the constraint the
    # design doc worried about; the per-tick sign-and-push count is. Report
    # both so the record shows which one actually binds.
    if [[ "${resp_max}" -gt 4194304 ]]; then
      finding "a rotation response reached ${resp_max}B, past the 4MB gRPC default -- rotation must be chunked"
    else
      finding "largest rotation response is ${resp_max}B -- one secret per message, so the 4MB gRPC ceiling is not reachable this way"
    fi
    if [[ "${rotations}" -gt 0 ]]; then
      per_tick=$(( rotations / (n > 0 ? n : 1) ))
      finding "a rotation tick costs ${n} independent pushes (${rotations} total over the run, ~${per_tick} per name); at 50k live names that is 50k signatures per tick"
    fi
    if [[ "$((updated_after - updated_before))" -eq 0 ]]; then
      bad "no rotation reached the data plane at ${n} live names"
    else
      ok "rotation reached the data plane at ${n} live names"
    fi
  done
fi

################################################################################
if wants 6; then
  bold ""
  bold "--- Phase 6: idle reclamation ---"
  note "Two arms over the same fill, differing only in --idle. The control is"
  note "what this phase measured before withdrawal existed: Envoy has no expiry"
  note "of its own for an on-demand secret and never says it is finished with a"
  note "name, so a host contacted once is held for the life of the stream."
  note "The second arm has the server withdraw names the proxy has stopped"
  note "asking about. Whether Envoy actually gives the memory back is the"
  note "measurement -- the docs say a removal cancels the subscription, which"
  note "is not the same claim."

  # idle_watch LABEL SECONDS samples the memory and live-set curve, leaving the
  # last reading in IDLE_END_*. Both arms use it so the two curves are directly
  # comparable rather than being two differently-shaped observations.
  IDLE_END_RSS=0
  IDLE_END_ALLOC=0
  IDLE_END_ACTIVE=0
  idle_watch() {
    local label="$1" seconds="$2"
    local interval=30
    [[ "${seconds}" -lt 120 ]] && interval=15
    local elapsed=0 rss alloc
    while [[ "${elapsed}" -lt "${seconds}" ]]; do
      sleep "${interval}"
      elapsed=$((elapsed + interval))
      read -r rss alloc <<<"$(envoy_mem)"
      record "phase6 ${label} t+${elapsed}s rss=${rss}KB allocated=${alloc}B cert_active=$(stat_value listener.mitm.on_demand_secret.cert_active) sds_names_live=$(sds_metric names_live) withdrawn=$(sds_metric idle_withdrawals)"
    done
    read -r IDLE_END_RSS IDLE_END_ALLOC <<<"$(envoy_mem)"
    IDLE_END_ACTIVE="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  }

  n="${N_STEPS[-1]}"

  # --- Arm A: no withdrawal (the behaviour before --idle existed) ----------
  stop_envoy
  stop_sds
  start_sds_null
  start_envoy envoy-scale.yaml 2

  load "idle-hold-fill" "${n}" 500
  expect_all_served "phase6 arm A fill to ${n}"
  sleep 2
  hold_peak_rss="$(rss_kb "${ENVOY_PID}")"
  hold_peak_active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  record "phase6 hold peak live=${hold_peak_active} rss=${hold_peak_rss}KB allocated=$(stat_value server.memory_allocated)B"

  idle_watch hold "${IDLE_SECONDS}"
  hold_end_rss="${IDLE_END_RSS}"
  hold_end_active="${IDLE_END_ACTIVE}"
  record "phase6 hold end live=${hold_end_active} rss=${hold_end_rss}KB allocated=${IDLE_END_ALLOC}B"

  # --- Arm B: withdraw idle names ------------------------------------------
  stop_envoy
  stop_sds
  start_sds_null --idle "${IDLE_TIMEOUT}"
  start_envoy envoy-scale.yaml 2

  fill_at="${SNI_CURSOR}"
  load "idle-drop-fill" "${n}" 500
  expect_all_served "phase6 arm B fill to ${n}"
  sleep 2
  drop_peak_rss="$(rss_kb "${ENVOY_PID}")"
  drop_peak_alloc="$(stat_value server.memory_allocated)"
  drop_peak_active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  record "phase6 drop peak live=${drop_peak_active} rss=${drop_peak_rss}KB allocated=${drop_peak_alloc}B idle_timeout=${IDLE_TIMEOUT}"

  idle_watch drop "${IDLE_SECONDS}"
  drop_end_rss="${IDLE_END_RSS}"
  drop_end_alloc="${IDLE_END_ALLOC}"
  drop_end_active="${IDLE_END_ACTIVE}"
  withdrawn="$(sds_metric idle_withdrawals)"
  sweeps="$(sds_metric idle_sweeps)"
  record "phase6 drop end live=${drop_end_active} rss=${drop_end_rss}KB allocated=${drop_end_alloc}B withdrawn=${withdrawn} over ${sweeps} sweeps"

  # One sweep per name withdrawn is the one-stream-per-secret finding showing
  # up from the other side: the sweep is per-stream, so a stream holding a
  # single name can only ever withdraw that one.
  if [[ "${withdrawn}" -gt 0 && "${sweeps}" -ge "${withdrawn}" ]]; then
    finding "${withdrawn} names came back over ${sweeps} sweeps -- one per sweep, because each stream holds exactly one name"
  fi

  # --- Did the data plane act on the withdrawals? --------------------------
  # cert_active is Envoy's own count, so it answers the protocol question
  # independently of the allocator. RSS answers the one that matters for
  # capacity planning, and the two can disagree: tcmalloc is under no
  # obligation to hand pages back just because Envoy freed the objects.
  if [[ "${drop_peak_active}" -gt 0 && "${drop_end_active}" -le $((drop_peak_active / 10)) ]]; then
    ok "withdrawal reached the data plane: cert_active ${drop_peak_active} -> ${drop_end_active}"
  elif [[ "${drop_end_active}" -lt "${drop_peak_active}" ]]; then
    finding "withdrawal only partly reached the data plane: cert_active ${drop_peak_active} -> ${drop_end_active} after ${withdrawn} withdrawals"
  else
    bad "the server withdrew ${withdrawn} names and Envoy's cert_active did not move (${drop_peak_active} -> ${drop_end_active})"
  fi

  # 5% is well inside the noise of an allocator that does not promptly return
  # pages; anything smaller would report reclamation that did not happen.
  hold_floor=$((hold_peak_rss * 95 / 100))
  drop_floor=$((drop_peak_rss * 95 / 100))
  if [[ "${hold_end_rss}" -lt "${hold_floor}" ]]; then
    finding "the control arm reclaimed memory on its own (${hold_peak_rss}KB -> ${hold_end_rss}KB); the comparison below is not attributable to withdrawal"
  else
    finding "ANSWER (control): nothing is reclaimed without withdrawal. ${hold_peak_rss}KB -> ${hold_end_rss}KB over ${IDLE_SECONDS}s, cert_active ${hold_peak_active} -> ${hold_end_active}."
  fi
  # How much Envoy considers freed, and what that works out to per secret --
  # the number to compare against phase 1's bytes-per-secret on the way up.
  alloc_freed=$((drop_peak_alloc - drop_end_alloc))
  per_secret_freed=0
  if [[ "${withdrawn}" -gt 0 && "${alloc_freed}" -gt 0 ]]; then
    per_secret_freed=$((alloc_freed / withdrawn))
  fi

  if [[ "${drop_end_rss}" -lt "${drop_floor}" ]]; then
    finding "ANSWER (--idle ${IDLE_TIMEOUT}): memory IS returned, all the way to the OS. RSS ${drop_peak_rss}KB -> ${drop_end_rss}KB, allocated ${drop_peak_alloc}B -> ${drop_end_alloc}B (${per_secret_freed}B per secret), cert_active ${drop_peak_active} -> ${drop_end_active}."
  elif [[ "${alloc_freed}" -gt $((drop_peak_alloc / 2)) ]]; then
    finding "ANSWER (--idle ${IDLE_TIMEOUT}): Envoy DOES release the memory -- allocated ${drop_peak_alloc}B -> ${drop_end_alloc}B, ${per_secret_freed}B per withdrawn secret, cert_active ${drop_peak_active} -> ${drop_end_active}."
    finding "  => but RSS stayed at ${drop_end_rss}KB (peak ${drop_peak_rss}KB): tcmalloc kept the pages. The heap is reusable by the next ${withdrawn} hosts, so this bounds GROWTH, not the pod's memory limit. Size the limit for the peak live set regardless."
  else
    finding "ANSWER (--idle ${IDLE_TIMEOUT}): the subscriptions went away (cert_active ${drop_peak_active} -> ${drop_end_active}) but the memory did not -- allocated ${drop_peak_alloc}B -> ${drop_end_alloc}B, RSS ${drop_peak_rss}KB -> ${drop_end_rss}KB. Withdrawal is cancelling the subscription without freeing what backed it."
  fi

  # --- The safety property -------------------------------------------------
  # Withdrawal has to cost a re-fetch and nothing more. If a withdrawn host
  # cannot be reached again, this is not reclamation, it is an outage with a
  # schedule -- and it would be invisible in the memory curve above, which is
  # exactly why it is asserted here rather than assumed.
  load "idle-refetch" 40 20 --distinct "${n}" --sni-start "${fill_at}"
  record "phase6 re-fetch after withdrawal: ok=${LOAD[ok]}/${LOAD[attempted]} failed=${LOAD[failed]} p50=${LOAD[handshake_us_p50]}us p99=${LOAD[handshake_us_p99]}us"
  if [[ "${LOAD[failed]}" -eq 0 && "${LOAD[ok]}" -gt 0 ]]; then
    ok "every withdrawn host was served again on the next connection"
    if [[ -n "${COLD_P50[${N_STEPS[-1]}]:-}" ]]; then
      finding "a withdrawn host costs one cold handshake to get back: ${LOAD[handshake_us_p50]}us here against ${COLD_P50[${N_STEPS[-1]}]}us for first contact in phase 2"
    fi
  else
    bad "${LOAD[failed]} of ${LOAD[attempted]} connections to withdrawn hosts failed -- withdrawal is breaking hosts, not reclaiming them"
  fi
fi

################################################################################
if wants 7; then
  bold ""
  bold "--- Phase 7: reconnect resync ---"
  note "When SDS goes away, every one of Envoy's N per-secret streams breaks."
  note "On reconnect it reopens them and replays each name in that stream's"
  note "initial_resource_versions -- so a restart is N reconnects and N replays,"
  note "arriving over some interval rather than as one request."

  stop_envoy
  stop_sds
  start_sds_null
  start_envoy envoy-scale.yaml 2

  n="${N_STEPS[-1]}"
  fill_at="${SNI_CURSOR}"
  load "resync-fill" "${n}" 500
  expect_all_served "phase7 fill to ${n}"
  sleep 2
  held="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  record "phase7 holding ${held} secrets before the restart"

  stop_sds
  # Names Envoy already holds must keep serving with SDS gone -- that is the
  # run-poc.sh finding this phase is checking still holds at scale.
  load "resync-down" 200 50 --distinct "${n}" --sni-start "${fill_at}"
  record "phase7 with SDS down, warm names: ok=${LOAD[ok]} failed=${LOAD[failed]} p99=${LOAD[handshake_us_p99]}us"

  restart_ns="$(date +%s%N)"
  start_sds_null

  # Wait for the replay to SETTLE, not merely to start.
  #
  # The reconnect is N independent streams racing back, so the counter climbs
  # for as long as they keep arriving. An earlier version broke out of this loop
  # the moment resync_names went above zero and reported "169 of 3000 replayed"
  # as a finding about Envoy, when it was a finding about the sampling. Poll
  # until the count has been unchanged for a few consecutive reads.
  resync_names=0
  first_ms=0
  stable=0
  for _ in $(seq 1 240); do
    latest="$(sds_metric resync_names)"
    if [[ "${latest}" -gt 0 && "${first_ms}" -eq 0 ]]; then
      first_ms=$(( ($(date +%s%N) - restart_ns) / 1000000 ))
    fi
    if [[ "${latest}" -eq "${resync_names}" && "${latest}" -gt 0 ]]; then
      stable=$((stable + 1))
      [[ "${stable}" -ge 8 ]] && break
    else
      stable=0
    fi
    resync_names="${latest}"
    sleep 0.25
  done
  resync_ms=$(( ($(date +%s%N) - restart_ns) / 1000000 ))
  resync_reqs="$(sds_metric resync_requests)"

  record "phase7 resync: ${resync_names} names over ${resync_reqs} requests; first at +${first_ms}ms, settled by +${resync_ms}ms"
  # 95% rather than all of them: a handful of streams can still be reconnecting
  # when the count goes quiet, and calling that a data-loss finding would be
  # wrong.
  if [[ "${resync_names}" -ge $((held * 95 / 100)) ]]; then
    finding "ANSWER: Envoy replays its ENTIRE live set on reconnect -- ${resync_names} names across ${resync_reqs} separate requests, one per stream, all within ${resync_ms}ms"
    finding "  => an SDS restart is an N-stream thundering herd; with a real signer that is N mints in a burst, not N cache hits"
    ok "the replay carried everything Envoy was holding"
  elif [[ "${resync_names}" -gt 0 ]]; then
    finding "ANSWER: the replay carried ${resync_names} of ${held} held names over ${resync_reqs} requests"
  else
    bad "no resync was observed within 60s of the restart"
  fi

  # A cold name after the restart proves the new stream is actually usable and
  # not just connected.
  load "resync-cold" 40 10
  record "phase7 cold fetch after resync: ok=${LOAD[ok]} failed=${LOAD[failed]} p50=${LOAD[handshake_us_p50]}us"
  if [[ "${LOAD[failed]}" -eq 0 ]]; then
    ok "minting works again on the reconnected stream"
  else
    bad "${LOAD[failed]} cold fetches failed after the resync"
  fi
fi

################################################################################
if wants 8; then
  bold ""
  bold "--- Phase 8: realism (dynamic_forward_proxy, needs outbound internet) ---"
  note "Kept out of the default set and out of the comparisons: real DNS and a"
  note "real origin add variance that would pollute every number above. The"
  note "question here is only whether anything changes qualitatively."

  stop_envoy
  stop_sds
  start_sds_real
  start_envoy envoy-bootstrap-fwdproxy.yaml 2

  load "realism" 50 5 --sni-format 'example.com' --distinct 1 --request
  record "phase8 through dynamic_forward_proxy: ok=${LOAD[ok]} failed=${LOAD[failed]} handshake p50=${LOAD[handshake_us_p50]}us request p50=${LOAD[request_us_p50]:-0}us"
  if [[ "${LOAD[ok]}" -gt 0 ]]; then
    ok "MITM re-origination works under load"
  else
    bad "no connection completed through the forward proxy (network?)"
  fi
fi

################################################################################
if wants 9; then
  bold ""
  bold "--- Phase 9: ${NAMES} live secrets, rotation off -- the cost of merely holding them ---"
  note "Phase 1 measures the same curve but stops at ${N_STEPS[-1]} and takes no CPU"
  note "reading at all. This one runs the REAL signer and watches both processes,"
  note "because the sdsmintd side is not the certificates: server.go opens three"
  note "goroutines and up to two tickers per stream, and Envoy opens one stream"
  note "per secret. At ${NAMES} names that is the runtime, not the crypto."

  stop_envoy
  stop_sds
  # --cache-cap above NAMES so a re-sign is a finding and not the cache evicting
  # under us; no --rotate and no --idle, so nothing moves once the fill is done.
  start_sds_real --cache-cap $((NAMES + 50000))
  start_envoy envoy-scale.yaml 2

  # The baseline has to be taken after Envoy is serving but before any secret
  # exists, or the per-secret figures carry Envoy's fixed footprint in them.
  read -r e_rss0 e_alloc0 e_cpu0 s_rss0 s_sys0 s_cpu0 g0 _ _ _ m0 < <(big_sample)
  record "phase9 baseline (0 secrets): envoy rss=$((e_rss0 / 1024))MB alloc=$((e_alloc0 / 1048576))MB | sds rss=$((s_rss0 / 1024))MB sys=$((s_sys0 / 1048576))MB goroutines=${g0}"

  step=$((NAMES / 4))
  fill_ok=true
  prev_e_rss="${e_rss0}"; prev_s_rss="${s_rss0}"
  prev_e_cpu="${e_cpu0}"; prev_s_cpu="${s_cpu0}"
  prev_n=0
  for i in 1 2 3 4; do
    load "hold-${i}" "${step}" "${FILL_RATE}"
    expect_all_served "phase9 fill step ${i} (+${step} names)" || { fill_ok=false; break; }
    # Envoy acknowledges a secret slightly after the handshake that asked for
    # it completes, so a sample taken the instant sdsload returns undercounts
    # cert_active by however many were still in flight.
    sleep 3
    read -r e_rss e_alloc e_cpu s_rss s_sys s_cpu g active names streams mints < <(big_sample)

    d_n=$((active - prev_n))
    [[ "${d_n}" -lt 1 ]] && d_n=1
    e_per=$(( (e_rss - prev_e_rss) * 1024 / d_n ))
    s_per=$(( (s_rss - prev_s_rss) * 1024 / d_n ))
    e_cpu_per=$(( (e_cpu - prev_e_cpu) * 1000 / d_n ))
    s_cpu_per=$(( (s_cpu - prev_s_cpu) * 1000 / d_n ))

    record "phase9 live=${active} envoy rss=$((e_rss / 1024))MB alloc=$((e_alloc / 1048576))MB cpu=$((e_cpu / 1000))s | sds rss=$((s_rss / 1024))MB sys=$((s_sys / 1048576))MB cpu=$((s_cpu / 1000))s goroutines=${g}"
    record "phase9 live=${active} sds names=${names} streams=${streams} mints=${mints} | marginal: envoy ${e_per}B/secret ${e_cpu_per}us/secret, sds ${s_per}B/secret ${s_cpu_per}us/secret"
    record "phase9 live=${active} fill achieved ${LOAD[rate_achieved]}/s handshake p50=${LOAD[handshake_us_p50]}us p99=${LOAD[handshake_us_p99]}us"
    [[ "${i}" -eq 1 ]] && FIRST_FILL_P50="${LOAD[handshake_us_p50]}"

    prev_e_rss="${e_rss}"; prev_s_rss="${s_rss}"
    prev_e_cpu="${e_cpu}"; prev_s_cpu="${s_cpu}"
    prev_n="${active}"
  done

  if [[ "${fill_ok}" == true ]]; then
    read -r e_rss e_alloc _ s_rss s_sys _ g active _ streams _ < <(big_sample)
    tot_n="${active}"; [[ "${tot_n}" -lt 1 ]] && tot_n=1
    ok "held ${active} live secrets with every handshake served"
    finding "ANSWER (memory): ${active} live secrets cost envoy $(( (e_rss - e_rss0) / 1024 ))MB and sdsmintd $(( (s_rss - s_rss0) / 1024 ))MB over baseline"
    finding "  => $(( (e_rss - e_rss0) * 1024 / tot_n ))B/secret in Envoy, $(( (s_rss - s_rss0) * 1024 / tot_n ))B/secret in sdsmintd, ${g} goroutines for ${streams} streams"

    # The measurement nothing else in this harness takes: with the fill over and
    # rotation off, is holding the set free? Every stream is still open and every
    # goroutine is still scheduled, so "nothing is happening" is a claim about
    # the runtime that has to be checked rather than assumed.
    note "30s of complete quiet -- measuring what ${active} idle streams cost..."
    q_e0="$(cpu_ms "${ENVOY_PID}")"; q_s0="$(cpu_ms "${SDS_PID}")"
    q_rss0="${s_rss}"
    sleep 30
    q_e1="$(cpu_ms "${ENVOY_PID}")"; q_s1="$(cpu_ms "${SDS_PID}")"
    q_rss1="$(rss_kb "${SDS_PID}")"
    QUIET_SDS_MS=$((q_s1 - q_s0))
    QUIET_ENVOY_MS=$((q_e1 - q_e0))
    envoy_per_s=$((QUIET_ENVOY_MS / 30))
    sds_per_s=$((QUIET_SDS_MS / 30))
    record "phase9 quiet 30s at ${active} live: envoy ${QUIET_ENVOY_MS}ms cpu, sds ${QUIET_SDS_MS}ms cpu, sds rss $((q_rss0 / 1024))MB -> $((q_rss1 / 1024))MB"
    # Milliseconds of CPU per wall-second, not a percentage: the expected answer
    # is a small fraction of one core, and "0%" would hide the difference
    # between "nearly free" and "actually free".
    finding "ANSWER (idle CPU): holding ${active} secrets costs ${envoy_per_s}ms of CPU per wall-second in Envoy and ${sds_per_s}ms in sdsmintd, with nothing arriving"
    if [[ "${QUIET_SDS_MS}" -lt 3000 ]]; then
      ok "sdsmintd is genuinely idle: ${active} open streams and their goroutines cost it nothing measurable"
    else
      finding "sdsmintd spends $((QUIET_SDS_MS * 100 / 30000))% of a core doing nothing at ${active} streams -- the per-stream goroutines are not free"
    fi
    # Checked separately from sdsmintd because the two are not symmetric: the Go
    # server parks its goroutines on channels and costs nothing, while Envoy
    # pays per open stream whether or not anything is on it.
    if [[ "${QUIET_ENVOY_MS}" -ge 1500 ]]; then
      finding "Envoy spends $((QUIET_ENVOY_MS * 100 / 30000))% of a core holding ${active} idle streams -- open SDS streams are not free on the data plane side, and this is the floor a real workload sits on top of"
    else
      ok "Envoy holds ${active} idle streams for under 5% of a core"
    fi

    # The fill latency is a result, not instrumentation. Phase 2 asks the same
    # question but only to ${N_STEPS[-1]} names; the last fill step here asks it
    # at ${active}, and the answer at that size is not a small multiple.
    if [[ -n "${FIRST_FILL_P50:-}" && "${FIRST_FILL_P50}" -gt 0 ]]; then
      ratio=$(( LOAD[handshake_us_p50] / FIRST_FILL_P50 ))
      record "phase9 first contact: p50 ${FIRST_FILL_P50}us at $((NAMES / 4)) live -> ${LOAD[handshake_us_p50]}us at ${active} (${ratio}x)"
      # A ratio this size is a curve; anything smaller, at a fill this short, is
      # as likely to be the box as the server, and phase 0's noise-floor marker
      # already says so. Reporting it either way, but only claiming it when the
      # margin is well past what the harness can produce by accident.
      if [[ "${ratio}" -ge 10 ]]; then
        finding "ANSWER (first-contact latency): ${ratio}x slower at ${active} live than at $((NAMES / 4)), at an unchanged ${FILL_RATE}/s arrival rate -- first contact does degrade with the size of the live set, which phase 2 does not see because it stops at ${N_STEPS[-1]}"
      fi
    fi
  else
    finding "phase9 stopped at ${prev_n} live secrets -- that is the ceiling on this box, and it is the answer"
  fi
fi

################################################################################
if wants 10; then
  bold ""
  bold "--- Phase 10: ${NAMES} live secrets WITH rotation -- the cost of the tick ---"
  note "Production defaults: --rotate --ttl 5m, so a stream re-mints at 2/3 TTL,"
  note "about every 200s. This is deliberately NOT phase 5's compressed 15s TTL."
  note "Each stream has its own ticker, started when that stream opened, so the"
  note "ticks are smeared across however long the fill took -- and no wider. A"
  note "fill that is short relative to the 200s period gives a burst; a fill that"
  note "is comparable to it gives a continuous rate. Both the average and the"
  note "peak are reported, because at ${NAMES} names they can differ by an order"
  note "of magnitude and only one of them sizes the CPU request."
  note "Restarted clean, so this is not measured on top of phase 9's heap."

  stop_envoy
  stop_sds
  start_sds_real --cache-cap $((NAMES + 50000)) --rotate --ttl 5m
  start_envoy envoy-scale.yaml 2

  read -r e_rss0 _ e_cpu0 s_rss0 _ s_cpu0 _ _ _ _ _ < <(big_sample)

  rot_ok=true
  step=$((NAMES / 4))
  for i in 1 2 3 4; do
    load "rot-${i}" "${step}" "${FILL_RATE}"
    expect_all_served "phase10 fill step ${i}" || { rot_ok=false; break; }
  done

  if [[ "${rot_ok}" == true ]]; then
    sleep 3
    read -r _ _ _ _ _ _ _ active _ streams _ < <(big_sample)
    record "phase10 filled to ${active} live secrets over ${streams} streams; watching ${ROTATE_WATCH}s"

    # Sample throughout rather than only at the ends: a rotation rate that is
    # steady and one that arrives in bursts have the same average, and only one
    # of them is a capacity problem.
    w_e0="$(cpu_ms "${ENVOY_PID}")"; w_s0="$(cpu_ms "${SDS_PID}")"
    rot0="$(sds_metric rotations)"; sign0="$(sds_metric sign_nanos_total)"
    mint0="$(sds_metric mints_issued)"
    upd0="$(stat_value listener.mitm.on_demand_secret.cert_updated)"
    prev_rot="${rot0}"; peak_rate=0; peak_at=0
    for t in $(seq 15 15 "${ROTATE_WATCH}"); do
      sleep 15
      now_rot="$(sds_metric rotations)"
      win_rate=$(( (now_rot - prev_rot) / 15 ))
      if [[ "${win_rate}" -gt "${peak_rate}" ]]; then peak_rate="${win_rate}"; peak_at="${t}"; fi
      note "  +${t}s  rotations=${now_rot} (+${win_rate}/s)  sds rss=$(( $(rss_kb "${SDS_PID}") / 1024 ))MB  envoy rss=$(( $(rss_kb "${ENVOY_PID}") / 1024 ))MB"
      prev_rot="${now_rot}"
    done
    w_e1="$(cpu_ms "${ENVOY_PID}")"; w_s1="$(cpu_ms "${SDS_PID}")"
    rot1="$(sds_metric rotations)"; sign1="$(sds_metric sign_nanos_total)"
    mint1="$(sds_metric mints_issued)"
    upd1="$(stat_value listener.mitm.on_demand_secret.cert_updated)"
    nacks="$(sds_metric nacks)"

    d_rot=$((rot1 - rot0))
    d_mint=$((mint1 - mint0))
    sds_ms=$((w_s1 - w_s0))
    envoy_ms=$((w_e1 - w_e0))
    record "phase10 over ${ROTATE_WATCH}s at ${active} live: rotations=${d_rot} mints=${d_mint} cert_updated=+$((upd1 - upd0)) nacks=${nacks}"
    record "phase10 over ${ROTATE_WATCH}s: sds ${sds_ms}ms cpu, envoy ${envoy_ms}ms cpu, signing $(( (sign1 - sign0) / 1000000 ))ms of it"
    finding "ANSWER (rotation): ${active} live secrets at --ttl 5m sustain $((d_mint / ROTATE_WATCH)) mints/s and $((d_rot / ROTATE_WATCH)) pushes/s, forever, with no traffic at all"
    finding "  => peak was ${peak_rate}/s in the 15s window at +${peak_at}s; a fill shorter than the 200s period bunches every ticker together, so size for the peak"
    finding "  => sdsmintd $((sds_ms * 100 / (ROTATE_WATCH * 1000)))% of a core, envoy $((envoy_ms * 100 / (ROTATE_WATCH * 1000)))% of a core, purely to keep the set alive"
    if [[ -n "${QUIET_SDS_MS:-}" ]]; then
      # Only meaningful when phase 9 ran in the same invocation; the quiet
      # window is the zero point that turns these into the cost OF rotation
      # rather than the cost of rotation plus everything else.
      base_ms=$((QUIET_SDS_MS * ROTATE_WATCH / 30))
      finding "  => against phase 9's quiet window ($((base_ms))ms for the same duration), rotation itself is $((sds_ms - base_ms))ms"
    else
      note "run --phases 9,10 together to get the rotation cost net of the idle baseline"
    fi

    if [[ "${nacks}" -eq 0 ]]; then
      ok "Envoy accepted every rotated secret (no NACKs) at ${active} live names"
    else
      bad "${nacks} NACKs during the rotation watch -- Envoy is rejecting rotated secrets at ${active} live names"
    fi

    # Serving during the sustained rotation, not merely surviving it.
    load "rot-serve" 200 50
    record "phase10 cold fetch during sustained rotation: ok=${LOAD[ok]} failed=${LOAD[failed]} p50=${LOAD[handshake_us_p50]}us p99=${LOAD[handshake_us_p99]}us"
    if [[ "${LOAD[failed]}" -gt 0 ]]; then
      bad "${LOAD[failed]} of ${LOAD[attempted]} cold handshakes failed during sustained rotation"
    elif [[ "${LOAD[handshake_us_p50]}" -gt 1000000 ]]; then
      # Completing is not the same as working. Rotation at this size saturates
      # the signer, and a new name then queues behind it -- so this arrives as a
      # latency result rather than as an error, and asserting only on failures
      # would report a multi-second handshake as a pass.
      bad "no handshake failed, but a new name took $((LOAD[handshake_us_p50] / 1000))ms at the median while ${active} secrets rotate -- rotation is starving new work, not coexisting with it"
    else
      ok "new names are still minted normally while ${active} secrets rotate underneath ($((LOAD[handshake_us_p50] / 1000))ms median)"
    fi
  fi
fi

################################################################################
bold ""
bold "=== ${PASS} passed, ${FAIL} failed ==="
note "results: ${RESULTS}"
note "logs:    ${RUN_DIR}/sds.log ${RUN_DIR}/envoy.log"
note "raw:     ${RUN_DIR}/load-*.json"
[[ "${FAIL}" -eq 0 ]]
