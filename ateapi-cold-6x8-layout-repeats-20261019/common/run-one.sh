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

# run-one.sh DIR TESTNAME BRANCH: one 6 x 8 run with the samplers, into DIR.
D="$1"; name="$2"; branch="$3"; C=$(dirname "$0"); S=/home/haiyanmeng/gihub/code-review/substrate
mkdir -p "$D"; cd "$D"
cat > run.py <<PY
import sys
sys.path.insert(0, "$S/benchmarking/automation")
import orchestrator
orchestrator.SUBSTRATE_DIR = "$D/substrate"
orchestrator.main()
PY
sed "s/^- name: ateapi_cold_resume_pause_fake_6x8cpu_size10\$/- name: $name/" $S/ateapi-cold-6x8-traced-20261018/tests.yaml > tests.yaml
echo "branch $branch $(git -C $S rev-parse --short $branch)" > run-times.txt
setsid nohup $C/sample-io.sh "$D" > io-sampler.log 2>&1 < /dev/null &
setsid nohup $C/sample-wal-io.sh "$D" > sampler.log 2>&1 < /dev/null &
setsid nohup $C/record-disks.sh "$D" > record-disks.log 2>&1 < /dev/null &
echo "start $(date -u +%FT%TZ)" >> run-times.txt
$S/benchmarking/automation/venv/bin/python -u run.py --repo file://$S --branch "$branch" \
  --dest gs://snapshot-substrate-test-haiyanmeng-gke-dev/bench-dev --tests "$D/tests.yaml" \
  --target-cluster-dir $C/target-clusters --manifests-dir $S/benchmarking/automation/manifests \
  --junit-output "$D/junit.xml" > orchestrator.log 2>&1
echo "exit=$?" >> run-times.txt; echo "end $(date -u +%FT%TZ)" >> run-times.txt
touch "$D/stop-sampler"; sleep 20
