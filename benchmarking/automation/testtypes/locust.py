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

"""The `type: locust` hooks (see the package docstring for the lifecycle).

The runner Job wraps benchmarking/locust/runner.py, which drives locust
(and boomer for glutton tests) and uploads its own results.
"""

import json
import os
import re
import subprocess
from typing import Any

from util import build_and_push

TEST_TYPE = "locust"


def validate(test: dict[str, Any]) -> None:
    name = test.get("name")
    for field in ("file", "duration", "users"):
        if field not in test:
            raise ValueError(f"locust test {name!r} missing {field!r}")


def results_uploaded(test: dict[str, Any], dest: str, commit: str, since: float) -> str | None:
    """Whether the runner Job for test at commit, submitted at since, already
    uploaded its results: "complete" or "failed" by its status.json, or None
    if there is none yet or it cannot be read. runner.py uploads status.json
    last, so finding it means every file is in place."""
    pattern = (f"{dest.rstrip('/')}/runs/{test['name']}"
               f"/run_date=*/run_ts=*/run_tag={commit}/status.json")
    try:
        ls = subprocess.run(["gcloud", "storage", "ls", pattern],
                            capture_output=True, text=True, timeout=60)
    except subprocess.TimeoutExpired:
        return None
    if ls.returncode != 0:
        return None
    for uri in sorted(ls.stdout.split(), reverse=True):
        m = re.search(r"/run_ts=(\d+)/", uri)
        # An earlier run of the same test at the same commit is not this one.
        if not m or int(m[1]) < since:
            continue
        try:
            cat = subprocess.run(["gcloud", "storage", "cat", uri],
                                 capture_output=True, text=True, timeout=60)
            status = json.loads(cat.stdout) if cat.returncode == 0 else None
        except (subprocess.TimeoutExpired, json.JSONDecodeError):
            return None
        if not isinstance(status, dict):
            return None
        # runner.py exits non-zero only when no stats were generated.
        return "complete" if status.get("stats_generated") else "failed"
    return None


def build_image(commit: str) -> str:
    """Build & push the locust runner image. It must live in the same
    project as the test cluster so the runner Job can pull it."""
    return build_and_push(
        f"{os.environ['KO_DOCKER_REPO']}/locust-test:{commit}",
        "benchmarking/locust/Dockerfile",
    )


def pre_test(test: dict[str, Any]) -> None:
    """Nothing to shape on the cluster before a locust run."""


def job_tmpl(manifests_dir: str) -> str:
    return os.path.join(manifests_dir, "runner-job.yaml.tmpl")


# Runner pod requests when a test sets none. Enough for the glutton and
# sweperf suites at their usual user counts; a boomer-driven class at
# thousands of users (agentsession marshals a 32Mi ingest payload per
# in-flight op) needs runnerCpu / runnerMemory raised in tests.yaml.
DEFAULT_RUNNER_CPU = "500m"
DEFAULT_RUNNER_MEMORY = "512Mi"


def job_subs(test: dict[str, Any]) -> dict[str, Any]:
    return {
        "TEST_FILE": test["file"],
        "DURATION": test["duration"],
        "USERS": test["users"],
        "RUNNER_CPU": test.get("runnerCpu", DEFAULT_RUNNER_CPU),
        "RUNNER_MEMORY": test.get("runnerMemory", DEFAULT_RUNNER_MEMORY),
    }
