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

# analyze-run.sh DIR: download DIR's results from GCS, run analyze_ladder.py
# (needs a Prometheus port-forward on 19090), then print per-step tables.
D="$1"; S=/home/haiyanmeng/gihub/code-review/substrate
st=$(grep -o 'gs://[^ ]*/status.json' "$D/orchestrator.log" | head -1); [ -n "$st" ] || { echo "no results in $D"; exit 1; }
mkdir -p "$D/results"; gcloud storage cp "${st%/status.json}/*" "$D/results/" >/dev/null 2>&1
python3 $S/benchmarking/automation/analyze_ladder.py "$D/results" > "$D/steps.jsonl" 2> "$D/steps.err"
python3 $(dirname "$0")/tables.py "$D" | tee "$D/tables.txt"
