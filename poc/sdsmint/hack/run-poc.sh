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
# End-to-end harness for the mint.md PoC.
#
# Brings up a standalone Envoy configured with the on_demand_secret certificate
# selector plus the sni certificate mapper, pointed at the sdsmintd minting SDS
# server over a unix socket, then runs the checks and experiments described in
# poc/sdsmint/README.md.
#
# Usage:
#   ./poc/sdsmint/hack/run-poc.sh                 # hermetic checks + experiments
#   ./poc/sdsmint/hack/run-poc.sh --forward-proxy # also MITM a real host
#   ./poc/sdsmint/hack/run-poc.sh --keep          # leave Envoy and sdsmintd running

set -o errexit
set -o nounset
set -o pipefail

readonly ENVOY_VERSION="1.37.5"
readonly ENVOY_URL="https://github.com/envoyproxy/envoy/releases/download/v${ENVOY_VERSION}/envoy-${ENVOY_VERSION}-linux-x86_64"

readonly LISTEN_PORT=18443
readonly ADMIN_PORT=19000

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly POC_DIR="$(dirname "${SCRIPT_DIR}")"
readonly REPO_ROOT="$(cd "${POC_DIR}/../.." && pwd)"
# __run is matched by the "__*" rule in .gitignore, so everything the harness
# generates stays out of version control.
readonly RUN_DIR="${POC_DIR}/__run"

FORWARD_PROXY=false
KEEP=false
for arg in "$@"; do
  case "${arg}" in
    --forward-proxy) FORWARD_PROXY=true ;;
    --keep) KEEP=true ;;
    -h|--help) sed -n '17,27p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: ${arg}" >&2; exit 2 ;;
  esac
done

PASS=0
FAIL=0
SDS_PID=""
ENVOY_PID=""

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
info()  { printf '\033[1;36m[step]\033[0m %s\n' "$*"; }
note()  { printf '       %s\n' "$*"; }
ok()    { PASS=$((PASS + 1)); printf '\033[1;32m  PASS\033[0m %s\n' "$*"; }
bad()   { FAIL=$((FAIL + 1)); printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; }
finding() { printf '\033[1;35m  ????\033[0m %s\n' "$*"; }

cleanup() {
  if [[ "${KEEP}" == true ]]; then
    bold "--keep set; leaving sdsmintd (pid ${SDS_PID:-none}) and envoy (pid ${ENVOY_PID:-none}) running."
    note "admin: http://127.0.0.1:${ADMIN_PORT}   listener: 127.0.0.1:${LISTEN_PORT}"
    return
  fi
  stop_envoy
  stop_sds
}
trap cleanup EXIT

# stop_pid terminates a child and refuses to block on it. A bare `wait` has no
# timeout, so a child that mishandles SIGTERM wedges the whole harness; poll
# instead and escalate to SIGKILL.
stop_pid() {
  local pid="$1" label="$2" i
  [[ -n "${pid}" ]] || return 0
  kill -0 "${pid}" 2>/dev/null || return 0
  kill "${pid}" 2>/dev/null || true
  for i in $(seq 1 50); do
    kill -0 "${pid}" 2>/dev/null || { wait "${pid}" 2>/dev/null || true; return 0; }
    sleep 0.1
  done
  echo "  WARN ${label} (pid ${pid}) ignored SIGTERM for 5s; sending SIGKILL" >&2
  kill -9 "${pid}" 2>/dev/null || true
  wait "${pid}" 2>/dev/null || true
}

stop_envoy() {
  stop_pid "${ENVOY_PID}" envoy
  ENVOY_PID=""
}

stop_sds() {
  stop_pid "${SDS_PID}" sdsmintd
  SDS_PID=""
}

# stat_value NAME prints the current value of an Envoy stat, or 0 if absent.
stat_value() {
  local raw
  raw="$(curl -s --max-time 5 "127.0.0.1:${ADMIN_PORT}/stats" 2>/dev/null || true)"
  local v
  v="$(printf '%s\n' "${raw}" | awk -F': ' -v n="$1" '$1 == n {print $2}' | tail -1)"
  if [[ "${v}" =~ ^[0-9]+$ ]]; then printf '%s\n' "${v}"; else printf '0\n'; fi
}

# leaf_field SNI OPENSSL_ARGS... prints one field of the certificate Envoy
# serves for the given SNI, or nothing if the handshake fails. s_client exits
# non-zero on a failed handshake, which is an expected outcome in some checks,
# so this never propagates a failure to errexit.
leaf_field() {
  local sni="$1"; shift
  local pem
  pem="$(echo | openssl s_client -connect "127.0.0.1:${LISTEN_PORT}" \
    -servername "${sni}" -CAfile "${RUN_DIR}/ca.pem" 2>/dev/null || true)"
  [[ -z "${pem}" ]] && return 0
  printf '%s\n' "${pem}" | openssl x509 -noout "$@" 2>/dev/null || true
}

# fetch SNI performs a full HTTPS request through the MITM listener. The client
# is told it is talking to :443 so the Host header carries no port, which is
# what the forward-proxy variant needs to find the real upstream.
fetch() {
  local sni="$1"
  curl -sS --max-time 20 --cacert "${RUN_DIR}/ca.pem" \
    --connect-to "${sni}:443:127.0.0.1:${LISTEN_PORT}" \
    "https://${sni}/" -o /dev/null -w '%{http_code}' 2>/dev/null || echo "000"
}

wait_for_admin() {
  for _ in $(seq 1 60); do
    if curl -s --max-time 1 "127.0.0.1:${ADMIN_PORT}/ready" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  echo "envoy admin never came up; see ${RUN_DIR}/envoy.log" >&2
  tail -20 "${RUN_DIR}/envoy.log" >&2 || true
  return 1
}

start_sds() {
  local ttl="$1"; shift
  "${RUN_DIR}/sdsmintd" \
    --uds "${RUN_DIR}/sdsmint.sock" \
    --ca-cert "${RUN_DIR}/ca.pem" \
    --ca-key "${RUN_DIR}/ca-key.pem" \
    --ttl "${ttl}" \
    --allow 'default.mitm.example' \
    --allow '*.mitm.example' \
    --allow 'example.com' \
    "$@" \
    >"${RUN_DIR}/sds.log" 2>&1 &
  SDS_PID=$!
  for _ in $(seq 1 40); do
    [[ -S "${RUN_DIR}/sdsmint.sock" ]] && return 0
    sleep 0.25
  done
  echo "sdsmintd never created its socket; see ${RUN_DIR}/sds.log" >&2
  tail -20 "${RUN_DIR}/sds.log" >&2
  return 1
}

start_envoy() {
  local config="$1" concurrency="${2:-2}"
  ( cd "${RUN_DIR}" && ./envoy -c "${config}" --concurrency "${concurrency}" \
      --log-level warn >"${RUN_DIR}/envoy.log" 2>&1 ) &
  ENVOY_PID=$!
  wait_for_admin
}

################################################################################
bold "=== mint.md PoC: on-demand per-SNI certificate minting ==="
mkdir -p "${RUN_DIR}"

info "Building sdsmintd"
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/sdsmintd" ./poc/sdsmint/cmd/sdsmintd )

info "Ensuring Envoy ${ENVOY_VERSION}"
if [[ ! -x "${RUN_DIR}/envoy" ]]; then
  note "downloading ${ENVOY_URL}"
  curl -sSL --max-time 900 -o "${RUN_DIR}/envoy" "${ENVOY_URL}"
  chmod +x "${RUN_DIR}/envoy"
fi
envoy_version="$("${RUN_DIR}/envoy" --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
note "envoy ${envoy_version}"
# The two extensions this design depends on first shipped in 1.37. On an older
# build the config is rejected outright, so fail with a useful message instead.
if ! printf '%s\n%s\n' "1.37.0" "${envoy_version}" | sort -V -C; then
  echo "envoy ${envoy_version} is too old; on_demand_secret and cert_mappers.sni need 1.37+" >&2
  exit 1
fi

cp "${POC_DIR}"/testdata/envoy-bootstrap*.yaml "${RUN_DIR}/"
rm -f "${RUN_DIR}/ca.pem" "${RUN_DIR}/ca-key.pem"

################################################################################
info "Bringing up sdsmintd + Envoy (hermetic bootstrap)"
start_sds 5m
start_envoy envoy-bootstrap.yaml 2
note "CA: ${RUN_DIR}/ca.pem   admin: 127.0.0.1:${ADMIN_PORT}"

################################################################################
bold ""
bold "--- Check 1: a leaf is minted per SNI, on demand ---"

# Prefetch should already have fired before any client connected.
if grep -q 'host=default.mitm.example' "${RUN_DIR}/sds.log"; then
  ok "prefetch_secret_names minted default.mitm.example at config load, before any request"
else
  bad "prefetch did not mint the default secret"
fi

code_a="$(fetch a.mitm.example)"
code_b="$(fetch b.mitm.example)"
[[ "${code_a}" == "200" ]] && ok "a.mitm.example -> HTTP 200" || bad "a.mitm.example -> HTTP ${code_a}"
[[ "${code_b}" == "200" ]] && ok "b.mitm.example -> HTTP 200" || bad "b.mitm.example -> HTTP ${code_b}"

subj_a="$(leaf_field a.mitm.example -subject)"
subj_b="$(leaf_field b.mitm.example -subject)"
serial_a="$(leaf_field a.mitm.example -serial)"
serial_b="$(leaf_field b.mitm.example -serial)"
san_a="$(leaf_field a.mitm.example -ext subjectAltName | tr -d ' \n')"

if [[ "${subj_a}" == *"CN=a.mitm.example"* && "${subj_b}" == *"CN=b.mitm.example"* ]]; then
  ok "each SNI got a leaf carrying its own CN (${subj_a} / ${subj_b})"
else
  bad "unexpected subjects: ${subj_a} / ${subj_b}"
fi
if [[ "${san_a}" == *"DNS:a.mitm.example"* ]]; then
  ok "leaf SAN matches the SNI"
else
  bad "leaf SAN does not match the SNI: ${san_a}"
fi
if [[ -n "${serial_a}" && "${serial_a}" != "${serial_b}" ]]; then
  ok "distinct serials per SNI, so these are genuinely separate leaves"
else
  bad "the two SNIs were served the same certificate (serial ${serial_a})"
fi

requested="$(stat_value listener.mitm.on_demand_secret.cert_requested)"
active="$(stat_value listener.mitm.on_demand_secret.cert_active)"
note "listener.mitm.on_demand_secret: cert_requested=${requested} cert_active=${active}"
if [[ "${requested}" -ge 3 ]]; then
  ok "Envoy created one on-demand SDS subscription per name (cert_requested=${requested})"
else
  bad "expected at least 3 on-demand subscriptions, saw ${requested}"
fi

################################################################################
bold ""
bold "--- Check 2: the allowlist actually blocks minting ---"

before_denied="$(grep -c 'certificate request denied' "${RUN_DIR}/sds.log" || true)"
blocked_out="$(curl -sS --max-time 20 --cacert "${RUN_DIR}/ca.pem" \
  --connect-to "evil.test:443:127.0.0.1:${LISTEN_PORT}" \
  https://evil.test/ 2>&1 >/dev/null || true)"
after_denied="$(grep -c 'certificate request denied' "${RUN_DIR}/sds.log" || true)"

if [[ "${blocked_out}" == *"handshake failure"* || "${blocked_out}" == *"alert"* || "${blocked_out}" == *"SSL"* ]]; then
  ok "a host outside --allow fails the handshake"
  note "client sees: ${blocked_out}"
else
  bad "disallowed host did not fail as expected: ${blocked_out}"
fi
if [[ "${after_denied}" -gt "${before_denied}" ]]; then
  ok "the refusal was audited server-side"
else
  bad "no audit line for the refused host"
fi

# mint.md's sketch says to "NACK" a disallowed name, but a server cannot NACK in
# xDS. We withdraw it instead, and the docs say a removal cancels the data-plane
# subscription -- so cert_active must not have grown.
active_after_block="$(stat_value listener.mitm.on_demand_secret.cert_active)"
if [[ "${active_after_block}" -le "${active}" ]]; then
  ok "the withdrawn name left no live subscription (cert_active ${active} -> ${active_after_block})"
else
  finding "cert_active grew after a withdrawal: ${active} -> ${active_after_block}"
fi

################################################################################
bold ""
bold "--- Experiment A: is Envoy's secret cache shared or per-worker? ---"
note "mint.md open question 2: memory footprint under a large live host set."

stop_envoy
start_envoy envoy-bootstrap.yaml 4
note "restarted Envoy with --concurrency 4"

base_requested="$(stat_value listener.mitm.on_demand_secret.cert_requested)"
# Wait on these PIDs specifically: a bare `wait` would also block on the
# backgrounded Envoy, which never exits.
fetch_pids=()
for _ in $(seq 1 12); do
  ( fetch shared.mitm.example >/dev/null ) &
  fetch_pids+=("$!")
done
for pid in "${fetch_pids[@]}"; do
  wait "${pid}" || true
done
sleep 1
after_requested="$(stat_value listener.mitm.on_demand_secret.cert_requested)"
delta=$((after_requested - base_requested))

note "12 concurrent connections to one SNI across 4 workers => cert_requested +${delta}"
if [[ "${delta}" -lt 0 ]]; then
  bad "stats went backwards (${base_requested} -> ${after_requested}); Envoy probably died, see ${RUN_DIR}/envoy.log"
elif [[ "${delta}" -eq 1 ]]; then
  finding "ANSWER: the secret cache is SHARED across workers (one subscription for the host)"
elif [[ "${delta}" -gt 1 && "${delta}" -le 4 ]]; then
  finding "ANSWER: the cache is PER-WORKER (+${delta} with 4 workers) -- footprint scales with workers x live hosts"
else
  finding "inconclusive: +${delta} subscriptions for one SNI"
fi

################################################################################
bold ""
bold "--- Experiment B: how does a secret get rotated or expire? ---"
note "mint.md open question 1: TTL or push-based invalidation?"

stop_envoy
stop_sds
rm -f "${RUN_DIR}/sds.log"
# Short TTL with --rotate so the server pushes a replacement quickly.
start_sds 6s --rotate
start_envoy envoy-bootstrap.yaml 2

fetch rotate.mitm.example >/dev/null
updated_before="$(stat_value listener.mitm.on_demand_secret.cert_updated)"
serial_before="$(leaf_field rotate.mitm.example -serial)"

note "holding the subscription for 15s while the server pushes re-mints..."
sleep 15

updated_after="$(stat_value listener.mitm.on_demand_secret.cert_updated)"
serial_after="$(leaf_field rotate.mitm.example -serial)"

note "cert_updated ${updated_before} -> ${updated_after}"
note "serial ${serial_before} -> ${serial_after}"
if [[ "${updated_after}" -gt "${updated_before}" && "${serial_before}" != "${serial_after}" ]]; then
  finding "ANSWER: rotation is PUSH-driven. Envoy applies a new version for a live name; it has no TTL of its own."
  ok "server-initiated rotation reached the data plane"
else
  bad "no rotation observed (cert_updated ${updated_before} -> ${updated_after})"
fi

################################################################################
bold ""
bold "--- Experiment C: what happens when the SDS server is down? ---"
note "mint.md open question 3: handshake behaviour and client-visible impact."

# A name Envoy has already cached should keep working with SDS gone.
stop_sds
cached_code="$(fetch rotate.mitm.example)"
if [[ "${cached_code}" == "200" ]]; then
  finding "ANSWER (cached names): already-minted secrets keep serving with SDS down -- no hard dependency per connection"
  ok "cached SNI still served HTTP 200 after sdsmintd was killed"
else
  finding "ANSWER (cached names): a cached SNI FAILED with SDS down (HTTP ${cached_code})"
fi

# A brand-new name has to reach SDS, so this is the real failure mode. Give the
# client a 20s budget: the point is that Envoy blows straight through it.
# Sets DOWN_OUT and ELAPSED_MS rather than printing, because a command
# substitution would run it in a subshell and lose both.
DOWN_OUT=""
ELAPSED_MS=0
cold_fetch() {
  local budget="$1" host="$2" start_ns
  start_ns="$(date +%s%N)"
  DOWN_OUT="$(curl -sS --max-time "${budget}" --cacert "${RUN_DIR}/ca.pem" \
    --connect-to "${host}:443:127.0.0.1:${LISTEN_PORT}" \
    "https://${host}/" 2>&1 >/dev/null || true)"
  ELAPSED_MS=$(( ($(date +%s%N) - start_ns) / 1000000 ))
}

note "leg 1: the shipped bootstrap sets no transport_socket_connect_timeout"
cold_fetch 20 cold1.mitm.example
elapsed_ms="${ELAPSED_MS}"
if [[ "${DOWN_OUT}" == *"timed out"* ]]; then
  finding "ANSWER (new names): Envoy NEVER gives up -- it paused the handshake for the client's full ${elapsed_ms}ms budget and was still waiting"
  ok "confirmed the default is an unbounded stall, not a failure"
else
  bad "expected the client to time out first, got: ${DOWN_OUT}"
fi

# The fix. transport_socket_connect_timeout is a filter-chain-level sibling of
# transport_socket, so it cannot be merged in with --config-yaml (repeated
# fields append rather than merge); generate the variant instead.
awk '/^      transport_socket:$/ && !d {print "      transport_socket_connect_timeout: 5s"; d=1} {print}' \
  "${POC_DIR}/testdata/envoy-bootstrap.yaml" >"${RUN_DIR}/envoy-bootstrap-timeout.yaml"

# Bring SDS back for the restart: the listener prefetches at config load, so
# Envoy needs a reachable server to initialise. Kill it again once it is up, so
# leg 2 measures the same cold-name-with-SDS-down case as leg 1.
stop_envoy
start_sds 5m
start_envoy envoy-bootstrap-timeout.yaml 2
stop_sds
note "leg 2: same run with transport_socket_connect_timeout: 5s"
# A name leg 1 never reached, so this is still a genuine cold fetch.
cold_fetch 60 cold2.mitm.example
elapsed_ms="${ELAPSED_MS}"
if [[ "${elapsed_ms}" -lt 10000 && "${DOWN_OUT}" != *"timed out"* ]]; then
  finding "ANSWER (mitigation): transport_socket_connect_timeout bounds it -- failed in ${elapsed_ms}ms with '${DOWN_OUT}'"
  ok "the stall is bounded by config, so this knob is REQUIRED in production"
else
  bad "timeout did not bound the stall (${elapsed_ms}ms, ${DOWN_OUT})"
fi
note "either way SDS availability gates every first-contact host; cached hosts are unaffected"

################################################################################
if [[ "${FORWARD_PROXY}" == true ]]; then
  bold ""
  bold "--- Check 3: real MITM re-origination (dynamic_forward_proxy) ---"
  stop_envoy
  start_sds 5m
  start_envoy envoy-bootstrap-fwdproxy.yaml 2

  body="${RUN_DIR}/example.com.html"
  fp_code="$(curl -sS --max-time 30 --cacert "${RUN_DIR}/ca.pem" \
    --connect-to "example.com:443:127.0.0.1:${LISTEN_PORT}" \
    https://example.com/ -o "${body}" -w '%{http_code}' 2>/dev/null || echo 000)"
  fp_issuer="$(leaf_field example.com -issuer)"

  if [[ "${fp_code}" == "200" ]] && grep -qi "example domain" "${body}"; then
    ok "fetched the real example.com through the MITM (HTTP ${fp_code}, body from origin)"
  else
    bad "forward-proxy fetch failed (HTTP ${fp_code})"
  fi
  if [[ "${fp_issuer}" == *"sdsmint PoC MITM CA"* ]]; then
    ok "the client was served our minted leaf, not the origin's (${fp_issuer})"
  else
    bad "unexpected issuer: ${fp_issuer}"
  fi
  note "upstream was still verified against the system trust store (auto_sni + auto_san_validation)"
fi

################################################################################
bold ""
bold "=== ${PASS} passed, ${FAIL} failed ==="
note "logs: ${RUN_DIR}/sds.log ${RUN_DIR}/envoy.log"
[[ "${FAIL}" -eq 0 ]]
