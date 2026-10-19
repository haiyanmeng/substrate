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

# Waits for the benchmark runner pod to be Running, then starts pgio.bt on the
# node running postgres-0, filtered to the Postgres data volume, for $2 seconds.
# Output: trace.txt (pod log, saved after the pod completes) and trace-target.txt.
W="$1"; secs="${2:-2700}"
ctx=gke_haiyanmeng-gke-dev_us-central1-a_nh-prow-parity; k="kubectl --context $ctx"
log() { echo "$(date -u +%FT%TZ) $*"; }
until $k -n benchmarking get pods --no-headers 2>/dev/null | grep '^runner-ateapi-cold-resume-pause-fake-6x8cpu' | grep -q Running; do
  [ -e "$W/stop-sampler" ] && { log "run ended before runner started"; exit 1; }
  sleep 15
done
log "runner running"
node=$($k -n ate-system get pod postgres-0 -o jsonpath='{.spec.nodeName}')
dev=$($k -n ate-system exec postgres-0 -c postgres -- awk '$5=="/var/lib/postgresql/data"{print $3}' /proc/self/mountinfo)
devt=$(( ${dev%:*} << 20 | ${dev#*:} ))
log "node=$node dev=$dev devt=$devt secs=$secs" | tee "$W/trace-target.txt"
$k -n kernel-trace create configmap pgio-bt --from-file="$W/pgio.bt" -o yaml --dry-run=client | $k apply -f -
$k -n kernel-trace delete pod pgio-trace --ignore-not-found
sed -e "s/NODE/$node/; s/DEVT/$devt/; s/SECONDS/$secs/" "$W/trace-pod.yaml.tmpl" | $k apply -f -
# Stream as a backup; the authoritative copy is fetched after completion.
sleep 20; $k -n kernel-trace logs -f pgio-trace > "$W/trace-stream.log" 2>&1 &
until phase=$($k -n kernel-trace get pod pgio-trace -o jsonpath='{.status.phase}' 2>/dev/null) && [[ $phase == Succeeded || $phase == Failed ]]; do sleep 30; done
log "trace pod $phase"
$k -n kernel-trace logs pgio-trace > "$W/trace.txt"
log "saved trace.txt ($(wc -l < "$W/trace.txt") lines)"
