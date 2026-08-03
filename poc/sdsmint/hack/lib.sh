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
# Shared plumbing for run-poc.sh and run-scale.sh: process lifecycle, the Envoy
# download and version gate, and reading Envoy's admin stats. Sourced, never
# executed.
#
# Callers must set RUN_DIR, POC_DIR and REPO_ROOT before sourcing, and are
# responsible for their own trap.

readonly ENVOY_VERSION="1.37.5"
readonly ENVOY_URL="https://github.com/envoyproxy/envoy/releases/download/v${ENVOY_VERSION}/envoy-${ENVOY_VERSION}-linux-x86_64"

readonly LISTEN_PORT=18443
readonly ADMIN_PORT=19000
readonly METRICS_PORT=19100

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

# ensure_envoy downloads the pinned release if it is not already in RUN_DIR and
# fails loudly on anything older than 1.37, where on_demand_secret and
# cert_mappers.sni first shipped. On an older build the config is rejected
# outright, and the error Envoy gives is not obviously about the version.
ensure_envoy() {
  if [[ ! -x "${RUN_DIR}/envoy" ]]; then
    note "downloading ${ENVOY_URL}"
    curl -sSL --max-time 900 -o "${RUN_DIR}/envoy" "${ENVOY_URL}"
    chmod +x "${RUN_DIR}/envoy"
  fi
  local envoy_version
  envoy_version="$("${RUN_DIR}/envoy" --version 2>&1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
  note "envoy ${envoy_version}"
  if ! printf '%s\n%s\n' "1.37.0" "${envoy_version}" | sort -V -C; then
    echo "envoy ${envoy_version} is too old; on_demand_secret and cert_mappers.sni need 1.37+" >&2
    exit 1
  fi
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

# start_envoy CONFIG [CONCURRENCY] starts Envoy in RUN_DIR and waits for admin.
#
# The subshell execs rather than forking so that ENVOY_PID is Envoy itself.
# Bash usually applies that optimisation on its own, but the scale harness
# reads /proc/$ENVOY_PID/status for RSS, and "usually" is not good enough when
# the alternative is silently reporting the memory of a shell.
start_envoy() {
  local config="$1" concurrency="${2:-2}"
  ( cd "${RUN_DIR}" && exec ./envoy -c "${config}" --concurrency "${concurrency}" \
      --log-level warn >"${RUN_DIR}/envoy.log" 2>&1 ) &
  ENVOY_PID=$!
  wait_for_admin
}

# rss_kb PID prints a process's resident set size in kilobytes, or 0 if it is
# gone. tcmalloc does not always return freed pages to the OS, so this is
# reported alongside Envoy's own server.memory_allocated rather than instead
# of it -- the two disagreeing is itself a finding.
rss_kb() {
  local pid="$1"
  [[ -n "${pid}" && -r "/proc/${pid}/status" ]] || { printf '0\n'; return; }
  awk '/^VmRSS:/ {print $2; found=1} END {if (!found) print 0}' "/proc/${pid}/status"
}

# cpu_ms PID prints the total CPU a process has consumed, user plus system, in
# milliseconds -- or 0 if it is gone. Milliseconds rather than seconds because
# the callers subtract two readings and bash has no floats.
#
# The field offsets in /proc/PID/stat are counted from the END of the comm
# field, not from the start of the line: comm is the executable name in
# parentheses and may itself contain spaces and parentheses, which would shift
# every subsequent column. Splitting on the last ") " is the documented way to
# parse this file, and awk's greedy match gives it for free.
cpu_ms() {
  local pid="$1"
  [[ -n "${pid}" && -r "/proc/${pid}/stat" ]] || { printf '0\n'; return; }
  local tck
  tck="$(getconf CLK_TCK 2>/dev/null || echo 100)"
  awk -v tck="${tck}" '
    {
      sub(/^.*\) /, "")     # drop pid and comm, leaving state as field 1
      # utime and stime are fields 14 and 15 of the original line, i.e. 12 and
      # 13 once pid, comm and the trailing space are gone.
      printf "%d\n", (($12 + $13) * 1000) / tck
    }' "/proc/${pid}/stat" 2>/dev/null || printf '0\n'
}

# require_clean_ports refuses to start when something is already listening on
# the ports or socket this harness uses.
#
# A leftover sdsmintd -- from --keep, or from a run that was interrupted --
# answers /healthz and owns the UDS path, so the readiness check passes for a
# process that is not the one just started. The new server then dies on "address
# already in use" while the harness carries on believing it is up. That failure
# is silent and its symptoms appear several phases later, so it is checked here
# rather than diagnosed there.
require_clean_ports() {
  local stale=""
  local p
  for p in "${LISTEN_PORT}" "${ADMIN_PORT}" "${METRICS_PORT}"; do
    if curl -s --max-time 1 --connect-timeout 1 "127.0.0.1:${p}/" >/dev/null 2>&1; then
      stale="${stale} ${p}"
    fi
  done
  if [[ -n "${stale}" ]]; then
    echo "something is already listening on:${stale}" >&2
    echo "a previous run left processes behind. Kill them and retry:" >&2
    echo "    pkill -f '${RUN_DIR}/sdsmintd'; pkill -f '${RUN_DIR}/envoy'" >&2
    exit 1
  fi
  # A stale socket file with nothing behind it is harmless to remove, and
  # leaving it would let a readiness check that tests for the path succeed
  # before the server has bound.
  rm -f "${RUN_DIR}/sdsmint.sock"
}

# stat_value NAME prints the current value of an Envoy stat, or 0 if absent.
#
# The 30s timeout is not generosity, it is a bug fix. /stats renders every
# counter Envoy has, and the on-demand selector adds several per live secret;
# at 100k names the document is large enough that the former 5s ceiling cut the
# transfer off. curl then exited non-zero, the `|| true` swallowed it, and this
# returned 0 -- which is indistinguishable from "the counter did not move". A
# phase 10 run reported cert_updated=+0 across 213,000 pushed rotations that
# way, and it read as a finding about Envoy rather than about the harness.
stat_value() {
  local raw
  raw="$(curl -s --max-time 30 "127.0.0.1:${ADMIN_PORT}/stats" 2>/dev/null || true)"
  local v
  v="$(printf '%s\n' "${raw}" | awk -F': ' -v n="$1" '$1 == n {print $2}' | tail -1)"
  if [[ "${v}" =~ ^[0-9]+$ ]]; then printf '%s\n' "${v}"; else printf '0\n'; fi
}
