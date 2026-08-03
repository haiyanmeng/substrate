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
# End-to-end run of the ext_proc egress-authorization PoC against a real Envoy.
#
# Starts extprocd and Envoy, drives the five hardcoded policies through a real
# CONNECT tunnel with egressprobe, and asserts on what the destination actually
# received -- not on "the request succeeded". Everything runs on loopback; no
# network egress beyond the one-time Envoy download.
#
#   ./poc/extproc/hack/run-poc.sh          run, then tear down
#   ./poc/extproc/hack/run-poc.sh --keep   leave extprocd and Envoy running

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly REPO_ROOT
readonly POC_DIR="${REPO_ROOT}/poc/extproc"
readonly RUN_DIR="${POC_DIR}/__run"

readonly ENVOY_VERSION="1.37.5"
readonly ENVOY_URL="https://github.com/envoyproxy/envoy/releases/download/v${ENVOY_VERSION}/envoy-${ENVOY_VERSION}-linux-x86_64"

# Must agree with testdata/envoy-extproc.yaml and extprocd's flag defaults.
readonly GATEWAY_PORT=18500
readonly ENVOY_ADMIN_PORT=19001
readonly CONNECT_PORT=19600
readonly INNER_PORT=19601
readonly ADMIN_PORT=19602

KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

PASS=0
FAIL=0
EXTPROCD_PID=""
ENVOY_PID=""

bold()    { printf '\033[1m%s\033[0m\n' "$*"; }
info()    { printf '\033[1;36m[step]\033[0m %s\n' "$*"; }
note()    { printf '       %s\n' "$*"; }
ok()      { PASS=$((PASS + 1)); printf '\033[1;32m  PASS\033[0m %s\n' "$*"; }
bad()     { FAIL=$((FAIL + 1)); printf '\033[1;31m  FAIL\033[0m %s\n' "$*"; }
finding() { printf '\033[1;35m  ????\033[0m %s\n' "$*"; }

# stop_pid terminates a child and refuses to block on it. A bare `wait` has no
# timeout, so a child that mishandles SIGTERM wedges the whole harness.
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

cleanup() {
  if [[ "${KEEP}" == "1" ]]; then
    note "--keep: leaving extprocd (${EXTPROCD_PID}) and envoy (${ENVOY_PID}) running"
    return
  fi
  stop_pid "${ENVOY_PID}" envoy
  stop_pid "${EXTPROCD_PID}" extprocd
}
trap cleanup EXIT

# ---------------------------------------------------------------- preflight ---

ensure_envoy() {
  if [[ ! -x "${RUN_DIR}/envoy" ]]; then
    note "downloading ${ENVOY_URL}"
    curl -sSL --max-time 900 -o "${RUN_DIR}/envoy" "${ENVOY_URL}"
    chmod +x "${RUN_DIR}/envoy"
  fi
  local v
  v="$("${RUN_DIR}/envoy" --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  note "envoy ${v}"
  # 1.37 is the floor for the internal-listener and ext_proc shapes used here.
  # On an older build the config is rejected and the error does not mention the
  # version, so the gate is cheaper than the diagnosis.
  if ! printf '%s\n%s\n' "1.37.0" "${v}" | sort -V -C; then
    echo "envoy ${v} is too old; this PoC needs 1.37+" >&2
    exit 1
  fi
}

# A leftover extprocd from a --keep run answers /healthz, so the readiness check
# would pass for a process that is not the one just started -- while the new one
# dies on "address already in use". Fail here rather than several phases later.
require_clean_ports() {
  local stale="" p
  for p in "${GATEWAY_PORT}" "${ENVOY_ADMIN_PORT}" "${CONNECT_PORT}" "${INNER_PORT}" "${ADMIN_PORT}"; do
    if (exec 3<>"/dev/tcp/127.0.0.1/${p}") 2>/dev/null; then
      stale="${stale} ${p}"
    fi
  done
  if [[ -n "${stale}" ]]; then
    echo "something is already listening on:${stale}" >&2
    echo "a previous run left processes behind. Kill them and retry:" >&2
    echo "    pkill -f '${RUN_DIR}/extprocd'; pkill -f '${RUN_DIR}/envoy'" >&2
    exit 1
  fi
}

wait_for() {
  local url="$1" label="$2" i
  for i in $(seq 1 80); do
    if curl -s --max-time 1 "${url}" >/dev/null 2>&1; then return 0; fi
    sleep 0.25
  done
  echo "${label} never came up" >&2
  tail -20 "${RUN_DIR}/envoy.log" "${RUN_DIR}/extprocd.log" 2>/dev/null >&2 || true
  return 1
}

start_extprocd() {
  ( cd "${RUN_DIR}" && exec ./extprocd >"${RUN_DIR}/extprocd.log" 2>&1 ) &
  EXTPROCD_PID=$!
  wait_for "127.0.0.1:${ADMIN_PORT}/healthz" extprocd
}

start_envoy() {
  # The subshell execs so ENVOY_PID is Envoy itself, not a shell wrapping it.
  # --file-flush-interval-msec defaults to 10s, which is longer than this whole
  # run: the assertions below read the access log while it is still buffered and
  # conclude that requests never happened.
  ( cd "${RUN_DIR}" && exec ./envoy -c ../testdata/envoy-extproc.yaml \
      --log-path "${RUN_DIR}/envoy.log" --log-level warn \
      --file-flush-interval-msec 100 \
      >"${RUN_DIR}/envoy.stdout" 2>&1 ) &
  ENVOY_PID=$!
  wait_for "127.0.0.1:${ENVOY_ADMIN_PORT}/ready" "envoy admin"
}

# ------------------------------------------------------------------ probing ---

# probe runs egressprobe and publishes its JSON result as P_CONNECT, P_INNER,
# P_BODY and P_ERR. Splitting the result into shell variables keeps the
# assertions below readable as a policy table rather than as JSON plumbing.
probe() {
  local out
  out="$("${RUN_DIR}/egressprobe" --timeout 8s "$@" 2>/dev/null || true)"
  eval "$(printf '%s' "${out}" | python3 -c '
import json, shlex, sys
try:
    r = json.load(sys.stdin)
except Exception:
    r = {}
print("P_CONNECT=%d" % r.get("connectStatus", 0))
print("P_INNER=%d" % r.get("innerStatus", 0))
print("P_BODY=%s" % shlex.quote(r.get("connectBody", "") + r.get("innerBody", "")))
print("P_ERR=%s" % shlex.quote(r.get("error", "")))
')"
}

# expect LABEL WANT_CONNECT WANT_INNER checks the two status codes. Pass -1 for
# an inner status that is not reached.
expect() {
  local label="$1" want_c="$2" want_i="$3"
  if [[ -n "${P_ERR}" ]]; then
    bad "${label}: transport error: ${P_ERR}"
    return
  fi
  if [[ "${P_CONNECT}" != "${want_c}" ]]; then
    bad "${label}: CONNECT ${P_CONNECT}, want ${want_c}"
    return
  fi
  if [[ "${want_i}" != "-1" && "${P_INNER}" != "${want_i}" ]]; then
    bad "${label}: inner ${P_INNER}, want ${want_i} (body: ${P_BODY})"
    return
  fi
  ok "${label}"
}

body_has()     { [[ "${P_BODY}" == *"$1"* ]] || return 1; }
expect_body()  { if body_has "$1"; then ok "$2"; else bad "$2: body does not contain $1 (got: ${P_BODY})"; fi; }
refute_body()  { if body_has "$1"; then bad "$2: the body still contains $1"; else ok "$2"; fi; }

counter() {
  curl -s --max-time 5 "127.0.0.1:${ADMIN_PORT}/stats" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["counters"].get(sys.argv[1], 0))' "$1"
}

# ---------------------------------------------------------------------- run ---

mkdir -p "${RUN_DIR}"

info "building"
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/extprocd" ./poc/extproc/cmd/extprocd )
( cd "${REPO_ROOT}" && go build -o "${RUN_DIR}/egressprobe" ./poc/extproc/cmd/egressprobe )
ensure_envoy
require_clean_ports

info "starting extprocd and envoy"
start_extprocd
start_envoy
note "gateway 127.0.0.1:${GATEWAY_PORT}, envoy admin 127.0.0.1:${ENVOY_ADMIN_PORT}, extprocd admin 127.0.0.1:${ADMIN_PORT}"

bold ""
bold "1. CONNECT checkpoint: policies answerable from the destination address"

probe --actor quarantined --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example
expect "DENY_ALL is refused at CONNECT" 403 -1
expect_body "policy DENY_ALL" "the refusal names the policy"

probe --actor wide-open --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example
expect "ALLOW_ALL tunnels to the destination" 200 200

probe --actor metrics-shipper --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example
expect "ALLOW_BY_IP_BLOCK admits an address inside 127.0.0.0/8" 200 200

probe --actor metrics-shipper --connect-to "8.8.8.8:443" --inner-host anything.example
expect "ALLOW_BY_IP_BLOCK refuses an address outside every block" 403 -1

# The CONNECT authority is the only destination the gateway may trust. Resolving
# a name here would let the actor pass the check with one answer and be dialled
# with another.
probe --actor metrics-shipper --connect-to "localhost:${ADMIN_PORT}" --inner-host anything.example
expect "a non-literal CONNECT authority fails closed" 403 -1

bold ""
bold "2. Identity: the gateway decides, the actor does not"

probe --actor "" --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example
expect "a missing actor header is refused" 403 -1

probe --actor ghost --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example
expect "an actor with no policy is refused" 403 -1

probe --actor quarantined --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example \
  --connect-header "x-ate-egress-mode: passthrough"
expect "a forged egress-mode header is overwritten" 403 -1

probe --actor quarantined --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example \
  --connect-header "x-ate-actor-key: acme-prod/wide-open"
expect "a forged actor-key header is overwritten" 403 -1

bold ""
bold "3. MITM checkpoint: policies that need the inner hostname"

probe --actor repo-reader --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host github.com
expect "ALLOW_BY_HOSTNAME admits an allowlisted host" 200 200

probe --actor repo-reader --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host GitHub.COM
expect "the allowlist is case-insensitive" 200 200

probe --actor repo-reader --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host evil.example
expect "ALLOW_BY_HOSTNAME refuses an unlisted host" 200 403

# The CONNECT for this one is indistinguishable from the allowed case: same
# actor, same destination address. Only the inner Host differs, which is the
# whole reason this checkpoint exists.
probe --actor repo-reader --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host sub.github.com
expect "a subdomain of an allowlisted host is not allowlisted" 200 403

probe --actor invoice-agent --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host api.stripe.com \
  --inner-header "Authorization: Bearer actor-supplied-value"
expect "BASIC_CREDENTIAL_INJECT admits its allowlisted host" 200 200
expect_body '"Token": "X"' "the destination received the injected credential"
refute_body "actor-supplied-value" "the actor's own credential was removed"

probe --actor invoice-agent --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host evil.example
expect "BASIC_CREDENTIAL_INJECT refuses a host outside its allowlist" 200 403

probe --actor repo-reader --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host github.com \
  --inner-header "X-Ate-Actor-Name: wide-open"
expect "an allowed request with forged tunnel headers still succeeds" 200 200
refute_body "X-Ate-Actor-Name" "no X-Ate-* header reached the destination"

bold ""
bold "4. How the actor reaches the MITM checkpoint"

fs="$(counter inner.actor_source.filter_state)"
none="$(counter inner.actor_source.none)"
hdr="$(counter inner.actor_source.ate_headers)"
key="$(counter inner.actor_source.actor_key)"
if [[ "${fs}" -gt 0 && "${none}" == "0" ]]; then
  ok "every inner request resolved its actor from filter state (${fs})"
else
  bad "actor sources: filter_state=${fs} actor_key=${key} ate_headers=${hdr} none=${none}"
fi
finding "filter state crosses the internal listener only with the internal_upstream"
finding "transport socket on the cluster; shared_with_upstream: TRANSITIVE alone is"
finding "not enough, and the failure is silent (fs=[-] at the inner listener)"
finding "request_attributes must subscript the key: bare \"filter_state\" arrives as"
finding "the literal string \"CelMap value\""

# Both mode routes must have carried a real tunnel. Without clear_route_cache
# the mode header is set but Envoy has already chosen the catch-all, so the
# identical request comes back as the catch-all's 500 instead -- a failure that
# is invisible unless something asserts the tunnel actually opened.
#
for m in passthrough mitm; do
  if grep " -> 200 route=${m} " "${RUN_DIR}/envoy.stdout" 2>/dev/null | grep -q "mode=${m}"; then
    ok "the ${m} route carried a tunnel"
  else
    bad "no CONNECT ever tunnelled in ${m} mode"
  fi
done
# A denial legitimately logs route=no_mode: Envoy resolves the route before
# ext_proc runs and an ImmediateResponse short-circuits the route action without
# re-resolving. Only a 2xx or 5xx there would mean the catch-all really ran.
if grep "route=no_mode" "${RUN_DIR}/envoy.stdout" 2>/dev/null | grep -qE " -> (2|4[^0])"; then
  bad "a request was served by the no-mode catch-all"
else
  ok "the no-mode catch-all never served a request"
fi

bold ""
bold "5. Fail closed when the authorization server is gone"

stop_pid "${EXTPROCD_PID}" extprocd
EXTPROCD_PID=""
probe --actor wide-open --connect-to "127.0.0.1:${ADMIN_PORT}" --inner-host anything.example
if [[ -n "${P_ERR}" ]]; then
  ok "with ext_proc down the CONNECT fails (${P_ERR})"
elif [[ "${P_CONNECT}" -ge 200 && "${P_CONNECT}" -lt 300 ]]; then
  bad "with ext_proc down the gateway still opened a tunnel (${P_CONNECT})"
else
  ok "with ext_proc down the CONNECT is refused (${P_CONNECT})"
  # The refusal must come from the filter, not from the catch-all route happening
  # to be safe. Those are the same status code from the client's point of view
  # and very different guarantees.
  refute_body "no egress mode set" "the refusal came from the filter, not the catch-all route"
fi
finding "failure_mode_allow: false turns an ext_proc outage into an egress outage."
finding "That is the correct trade for this gate, and it makes extprocd's"
finding "availability a hard dependency of every actor's outbound traffic."
start_extprocd

bold ""
bold "$([[ "${FAIL}" == "0" ]] && echo "PASS" || echo "FAIL"): ${PASS} passed, ${FAIL} failed"
[[ "${FAIL}" == "0" ]]
