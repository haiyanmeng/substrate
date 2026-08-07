#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# This is sourced as part of install-ate.sh. Do not run directly.
#
# Not a demo. It deploys the egress probe as an Actor for
# internal/e2e/suites/actoregress, and borrows the demo flag plumbing because
# that is where --deploy-x / --delete-x and the ${BUCKET_NAME} substitution
# already live. There is nothing here worth showing anyone.

ATE_DEMOS+=(demo-egress) # register demo-egress

demo-egress_usage() {
  echo "  (test fixture for internal/e2e/suites/actoregress, not a demo)"
}

demo-egress_cmdline() {
  case "${1}" in
    --deploy-demo-egress) demo-egress_deploy ;;
    --delete-demo-egress) demo-egress_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-egress_deploy() {
  log_step "demo-egress_deploy"
  ensure_crds
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
    internal/e2e/fixtures/egressprobe/egressprobe-actor.yaml.tmpl \
    | run_ko apply -f -

  # Wait for the fixture to be usable before returning: the WorkerPool must be
  # rolled out and the ActorTemplate's golden snapshot built, or the suite's
  # first ResumeActor fails on an unready template.
  log_step "Waiting for egress probe fixture to be ready..."
  run_kubectl rollout status deployment/egressprobe -n ate-demo-egress --timeout=300s
  run_kubectl wait --for=condition=Ready actortemplate/egressprobe -n ate-demo-egress --timeout=300s
}

demo-egress_delete() {
  log_step "demo-egress_delete"
  # The suite's actors live in atespace "acme-prod", not in the template's
  # namespace -- the egress policy table keys on the atespace/name pair
  # (internal/extproc/hardcoded.go), so the names are not ours to choose.
  # delete_demo_actors matches on the template instead, which finds them
  # wherever they landed.
  delete_demo_actors ate-demo-egress egressprobe
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
    internal/e2e/fixtures/egressprobe/egressprobe-actor.yaml.tmpl \
    | run_kubectl delete --ignore-not-found -f -
}
