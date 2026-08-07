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
# Deploys demos/egress/github-poller.yaml.tmpl: an Actor that curls the GitHub
# API in a loop while holding no credential. See demos/egress/github-poller.md.

ATE_DEMOS+=(demo-github-poller) # register demo-github-poller

GITHUB_POLLER_TMPL="demos/egress/github-poller.yaml.tmpl"

demo-github-poller_usage() {
  echo "  egress demo: an Actor that curls api.github.com every 10s with no credential of its own"
}

demo-github-poller_cmdline() {
  case "${1}" in
    --deploy-demo-github-poller) demo-github-poller_deploy ;;
    --delete-demo-github-poller) demo-github-poller_delete ;;
    *)
      return 1
      ;;
  esac
  return 0
}

demo-github-poller_deploy() {
  log_step "demo-github-poller_deploy"
  ensure_crds
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" "${GITHUB_POLLER_TMPL}" | run_ko apply -f -

  log_step "Waiting for the github-poller template to be ready..."
  run_kubectl rollout status deployment/github-poller -n ate-demo-github-poller --timeout=300s
  run_kubectl wait --for=condition=Ready actortemplate/github-poller -n ate-demo-github-poller --timeout=300s
}

demo-github-poller_delete() {
  log_step "demo-github-poller_delete"
  # The Actors live in atespace "acme-prod", not in the template's namespace --
  # the egress policy table keys on the atespace/name pair
  # (internal/extproc/hardcoded.go), so the names are not ours to choose.
  # delete_demo_actors matches on the template instead, which finds them
  # wherever they landed.
  delete_demo_actors ate-demo-github-poller github-poller
  sed "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" "${GITHUB_POLLER_TMPL}" \
    | run_kubectl delete --ignore-not-found -f -
}
