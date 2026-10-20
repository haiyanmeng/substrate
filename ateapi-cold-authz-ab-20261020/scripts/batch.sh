#!/usr/bin/env bash
# Runs each cold resume test on nh-prow-parity with OpenFGA authorization on,
# then each again with it off, through driver.py. One log per run under
# $OUT/<mode>/, one line per run in $OUT/summary.tsv. A failed run does not
# stop the batch.
set -u

REPO=/home/haiyanmeng/gihub/substrate-prow
OUT=$HOME/bench-runs/ateapi-cold-authz-ab-20261019
DEST=gs://snapshot-substrate-test-haiyanmeng-gke-dev/benchmark-results
TESTS=(
  ateapi_cold_resume_pause_fake_4cpu
  ateapi_cold_resume_pause_fake_8cpu
  ateapi_cold_resume_pause_fake_2x8cpu
  ateapi_cold_resume_pause_fake_2x8cpu_size10
  ateapi_cold_resume_pause_fake_3x8cpu_size10
  ateapi_cold_resume_pause_fake_4x8cpu_size10
  ateapi_cold_resume_pause_fake_5x8cpu_size10
  ateapi_cold_resume_pause_fake_6x8cpu_size10
)

export KUBECONFIG=/tmp/prow-run/kubeconfig
export RUNNER_NODE_SELECTOR=ate.dev/benchmarking=true

mkdir -p "$OUT/authz-on" "$OUT/authz-off"
[ -f "$OUT/summary.tsv" ] || printf 'mode\ttest\tstart_utc\tend_utc\texit\tresult\tstats\n' > "$OUT/summary.tsv"

for mode in authz-on authz-off; do
  if [ "$mode" = authz-on ]; then authz=true; else authz=false; fi
  for test in "${TESTS[@]}"; do
    log="$OUT/$mode/$test.log"
    start=$(date -u +%FT%TZ)
    ATE_API_EXPERIMENTAL_ENABLE_AUTHZ=$authz \
      python3 /tmp/prow-run/driver.py "$REPO" "$test" "$DEST" > "$log" 2>&1
    rc=$?
    end=$(date -u +%FT%TZ)
    result=$(grep -o '=== result: [a-z]*' "$log" | tail -1 | cut -d' ' -f3)
    stats=$(grep -o 'gs://[^ ]*/stats.jsonl' "$log" | tail -1)
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$mode" "$test" "$start" "$end" "$rc" "${result:-none}" "${stats:--}" >> "$OUT/summary.tsv"
  done
done
echo "batch done $(date -u +%FT%TZ)" >> "$OUT/summary.tsv"
