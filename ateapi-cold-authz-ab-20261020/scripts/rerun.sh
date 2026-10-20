#!/usr/bin/env bash
set -u
OUT=/home/haiyanmeng/bench-runs/ateapi-cold-authz-ab-20261019
T=ateapi_cold_resume_pause_fake_6x8cpu_size10
export KUBECONFIG=/tmp/prow-run/kubeconfig RUNNER_NODE_SELECTOR=ate.dev/benchmarking=true
start=$(date -u +%FT%TZ)
ATE_API_EXPERIMENTAL_ENABLE_AUTHZ=true python3 /tmp/prow-run/driver.py /home/haiyanmeng/gihub/substrate-prow $T gs://snapshot-substrate-test-haiyanmeng-gke-dev/benchmark-results > $OUT/authz-on/$T.log 2>&1
rc=$?
result=$(grep -o '=== result: [a-z]*' $OUT/authz-on/$T.log | tail -1 | cut -d' ' -f3)
stats=$(grep -o 'gs://[^ ]*/stats.jsonl' $OUT/authz-on/$T.log | tail -1)
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' authz-on "$T (rerun)" "$start" "$(date -u +%FT%TZ)" "$rc" "${result:-none}" "${stats:--}" >> $OUT/summary.tsv
