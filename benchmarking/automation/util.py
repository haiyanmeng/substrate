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

"""Helpers shared by orchestrator.py and the per-test-type modules."""

import re
import shlex
import subprocess
import time


def run(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    print(f"$ {' '.join(shlex.quote(c) for c in cmd)}", flush=True)
    start = time.monotonic()
    try:
        return subprocess.run(cmd, check=True, **kwargs)
    finally:
        print(f"  (took {time.monotonic() - start:.1f}s)", flush=True)


def run_no_check(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    print(f"$ {' '.join(shlex.quote(c) for c in cmd)}", flush=True)
    start = time.monotonic()
    try:
        return subprocess.run(cmd, check=False, **kwargs)
    finally:
        print(f"  (took {time.monotonic() - start:.1f}s)", flush=True)


def build_and_push(image: str, dockerfile: str) -> str:
    """docker build (linux/amd64, the cluster architecture) + push, from
    the repo root."""
    run(
        [
            "docker",
            "build",
            "--platform",
            "linux/amd64",
            "-t",
            image,
            "-f",
            dockerfile,
            ".",
        ]
    )
    run(["docker", "push", image])
    return image


def parse_duration_seconds(s: str) -> int:
    m = re.fullmatch(r"(\d+)\s*([smh]?)", s.strip())
    if not m:
        raise ValueError(f"unrecognized duration: {s}")
    n = int(m.group(1))
    unit = m.group(2) or "s"
    return n * {"s": 1, "m": 60, "h": 3600}[unit]


# The keys of a tests.yaml fakeDataPlane block and the
# benchmarking/workloads/deploy.sh flag each one sets.
FAKE_DATA_PLANE_FLAGS = {
    "nodes": "--fake-nodes",
    "workersPerNode": "--fake-workers-per-node",
    "run": "--fake-run",
    "delay": "--fake-delay",
    "capacityActors": "--fake-capacity-actors",
    "capacityResources": "--fake-capacity-resources",
}


def fake_data_plane_args(block: dict | None) -> list[str]:
    """Return the deploy.sh flags for a tests.yaml fakeDataPlane block, or
    none when the entry has no block. Raises ValueError on an unknown key or
    a count that is not a positive integer, so a bad entry fails before any
    cluster work rather than in deploy.sh."""
    if block is None:
        return []
    if not isinstance(block, dict):
        raise ValueError(f"fakeDataPlane must be a mapping, got {block!r}")
    unknown = sorted(set(block) - set(FAKE_DATA_PLANE_FLAGS))
    if unknown:
        raise ValueError(
            f"fakeDataPlane has unknown keys {unknown} "
            f"(want some of {sorted(FAKE_DATA_PLANE_FLAGS)})"
        )
    args = ["--fake-data-plane"]
    for key, flag in FAKE_DATA_PLANE_FLAGS.items():
        if key not in block:
            continue
        value = block[key]
        if key in ("nodes", "workersPerNode", "capacityActors"):
            if isinstance(value, bool) or not isinstance(value, int) or value < 1:
                raise ValueError(f"fakeDataPlane.{key} must be a positive integer, got {value!r}")
        args += [flag, str(value)]
    return args
