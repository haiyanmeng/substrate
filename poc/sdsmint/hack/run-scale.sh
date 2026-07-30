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
#   6  idle reclamation does memory ever come back?
#   7  reconnect        cost of Envoy replaying its live set after SDS restarts
#   8  realism          one run through dynamic_forward_proxy (needs internet)
#
# Phases 0-7 run the null minter, which serves pre-signed leaves so that what
# is measured is Envoy and not a P-256 signing loop -- a mint costs ~375us,
# which would swamp everything else. Phase 3 is the exception: the signer is
# its subject. Phase 8 is opt-in and not in the default set.
#
# Usage:
#   ./poc/sdsmint/hack/run-scale.sh                  # quick sweep, phases 0-7
#   ./poc/sdsmint/hack/run-scale.sh --phases 0,1,6   # a subset
#   ./poc/sdsmint/hack/run-scale.sh --full           # production-scale N, 30m idle
#   ./poc/sdsmint/hack/run-scale.sh --keep           # leave the processes up

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
    -h|--help) sed -n '17,45p' "${BASH_SOURCE[0]}"; exit 0 ;;
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
  STEADY_COUNT=20000
else
  N_STEPS=(200 1000 3000)
  RATES=(50 200 500)
  ROTATE_NS=(500)
  IDLE_SECONDS=120
  STEADY_COUNT=3000
fi

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
    --ca-cert "${RUN_DIR}/ca.pem" \
    --ca-key "${RUN_DIR}/ca-key.pem" \
    --allow '*.mitm.example' \
    --null-minter \
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
    --ca-cert "${RUN_DIR}/ca.pem" \
    --ca-key "${RUN_DIR}/ca-key.pem" \
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
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsmintd" ./poc/sdsmint/cmd/sdsmintd )
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsload" ./poc/sdsmint/cmd/sdsload )

info "Ensuring Envoy ${ENVOY_VERSION}"
ensure_envoy
cp "${POC_DIR}"/testdata/envoy-*.yaml "${RUN_DIR}/"
rm -f "${RUN_DIR}/ca.pem" "${RUN_DIR}/ca-key.pem"

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
  note "The stream's version map never shrinks and minter eviction does not"
  note "propagate, so the prediction is that nothing is ever released. If that"
  note "holds it is a production blocker, not a tuning note."

  stop_envoy
  stop_sds
  start_sds_null
  start_envoy envoy-scale.yaml 2

  n="${N_STEPS[-1]}"
  load "idle-fill" "${n}" 500
  expect_all_served "phase6 fill to ${n}"
  sleep 2
  peak_rss="$(rss_kb "${ENVOY_PID}")"
  peak_alloc="$(stat_value server.memory_allocated)"
  peak_active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  record "phase6 peak live=${peak_active} rss=${peak_rss}KB allocated=${peak_alloc}B"

  interval=30
  [[ "${IDLE_SECONDS}" -lt 120 ]] && interval=15
  elapsed=0
  while [[ "${elapsed}" -lt "${IDLE_SECONDS}" ]]; do
    sleep "${interval}"
    elapsed=$((elapsed + interval))
    read -r rss alloc <<<"$(envoy_mem)"
    record "phase6 t+${elapsed}s rss=${rss}KB allocated=${alloc}B cert_active=$(stat_value listener.mitm.on_demand_secret.cert_active)"
  done

  read -r end_rss end_alloc <<<"$(envoy_mem)"
  end_active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
  # 5% is well inside the noise of an allocator that does not promptly return
  # pages; anything smaller would report reclamation that did not happen.
  if [[ "${end_rss}" -lt $((peak_rss * 95 / 100)) ]]; then
    finding "ANSWER: memory IS reclaimed while idle (${peak_rss}KB -> ${end_rss}KB over ${IDLE_SECONDS}s)"
  else
    finding "ANSWER: nothing is reclaimed. ${peak_rss}KB -> ${end_rss}KB over ${IDLE_SECONDS}s with cert_active ${peak_active} -> ${end_active}. A host contacted once is held for the life of the stream."
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
bold ""
bold "=== ${PASS} passed, ${FAIL} failed ==="
note "results: ${RESULTS}"
note "logs:    ${RUN_DIR}/sds.log ${RUN_DIR}/envoy.log"
note "raw:     ${RUN_DIR}/load-*.json"
[[ "${FAIL}" -eq 0 ]]
