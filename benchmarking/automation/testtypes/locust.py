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

An optional `ateapi:` block shapes the control plane for a capacity run of
ate-api-server and Postgres (see ATEAPI_DEFAULTS); without it pre_test
leaves the cluster as installed.
"""

import json
import os
import re
import subprocess
from typing import Any

from util import build_and_push, run, run_no_check

TEST_TYPE = "locust"

NAMESPACE = "ate-system"

# Knobs of the `ateapi:` block. `cpu` has no default: it is the independent
# variable of the run.
ATEAPI_DEFAULTS = {
    # ate-api-server replicas, each with requests = limits of cpu and
    # memory. Go sizes GOMAXPROCS from the CPU limit.
    "replicas": 1,
    "memory": "4Gi",
    # pgxpool max connections per replica, spliced into the DSN. None keeps
    # the installed value (64 on size10).
    "poolMaxConns": None,
    # Load pg_stat_statements and run postgres_exporter next to Postgres,
    # for Prometheus (benchmarking/monitoring.yaml) to scrape.
    "pgStats": True,
    # Label one node for ateapi and another for the runner, away from the
    # Postgres node, and pin each there.
    "pinNodes": True,
}

def ateapi_config(test: dict[str, Any]) -> dict[str, Any] | None:
    """The test's `ateapi:` block merged over ATEAPI_DEFAULTS, or None."""
    if "ateapi" not in test:
        return None
    return {**ATEAPI_DEFAULTS, **test["ateapi"]}


def validate(test: dict[str, Any]) -> None:
    name = test.get("name")
    for field in ("file", "duration", "users"):
        if field not in test:
            raise ValueError(f"locust test {name!r} missing {field!r}")
    for field in ("runnerCpu", "runnerMemory"):
        if field in test and not _is_quantity(test[field]):
            raise ValueError(
                f"locust test {name!r} has invalid {field} {test[field]!r}"
            )
    if "ateapi" not in test:
        return
    block = test["ateapi"]
    if not isinstance(block, dict):
        raise ValueError(f"locust test {name!r} ateapi must be a mapping")
    allowed = set(ATEAPI_DEFAULTS) | {"cpu"}
    unknown = set(block) - allowed
    if unknown:
        raise ValueError(
            f"locust test {name!r} has unknown ateapi knob(s) "
            f"{sorted(unknown)}; allowed: {sorted(allowed)}"
        )
    cfg = ateapi_config(test)
    if "cpu" not in block or not _is_quantity(cfg["cpu"]):
        raise ValueError(f"locust test {name!r} needs ateapi.cpu, as in 2 or 1500m")
    if not _is_quantity(cfg["memory"]):
        raise ValueError(f"locust test {name!r} has invalid ateapi.memory")
    if not _is_positive_int(cfg["replicas"]):
        raise ValueError(f"locust test {name!r} ateapi.replicas must be an int >= 1")
    if cfg["poolMaxConns"] is not None and not _is_positive_int(cfg["poolMaxConns"]):
        raise ValueError(
            f"locust test {name!r} ateapi.poolMaxConns must be an int >= 1"
        )
    for knob in ("pgStats", "pinNodes"):
        if not isinstance(cfg[knob], bool):
            raise ValueError(f"locust test {name!r} ateapi.{knob} must be a bool")


def _is_positive_int(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool) and v >= 1


def _is_quantity(v: Any) -> bool:
    """A Kubernetes quantity as tests.yaml spells it: 2, 0.5, "1500m", "4Gi"."""
    if isinstance(v, bool):
        return False
    if isinstance(v, (int, float)):
        return v > 0
    return isinstance(v, str) and bool(re.fullmatch(r"[0-9]+(\.[0-9]+)?([mkMGT]i?)?", v))


def build_image(commit: str) -> str:
    """Build & push the locust runner image. It must live in the same
    project as the test cluster so the runner Job can pull it."""
    return build_and_push(
        f"{os.environ['KO_DOCKER_REPO']}/locust-test:{commit}",
        "benchmarking/locust/Dockerfile",
    )


def pre_test(test: dict[str, Any]) -> None:
    """With an `ateapi:` block, shape the control plane for the run: node
    roles, Postgres statistics, the pool size, then the ateapi pin. No
    unpatch: every test redeploys substrate."""
    cfg = ateapi_config(test)
    if cfg is None:
        return
    if cfg["pinNodes"]:
        label_nodes()
    if cfg["pgStats"]:
        enable_postgres_stats()
    if cfg["poolMaxConns"] is not None:
        set_pool_max_conns(cfg["poolMaxConns"])
    pin_ateapi(cfg)
    # The placement of the run, for comparing runs: a step compares across
    # runs only when ateapi, Postgres, and the runner sat on the same nodes.
    run_no_check(
        ["kubectl", "-n", NAMESPACE, "get", "pods", "-o", "wide",
         "-l", "app in (ate-api-server,postgres)"]
    )
    run_no_check(["kubectl", "get", "nodes", "-L", ROLE_LABEL])


# Node label that gives ate-api-server and the runner a node each, so the
# load generator is never ateapi's neighbor. Both pods require it through a
# node affinity.
ROLE_LABEL = "ate.dev/benchmark-role"


def pick_role_nodes(nodes: list[dict[str, Any]], postgres_node: str) -> tuple[str, str]:
    """Return (ateapi node, runner node): the first two schedulable nodes by
    name, leaving out the Postgres node. Postgres on size10 requests nearly
    a whole node, so its node is not a candidate for either role."""
    candidates = sorted(
        n["metadata"]["name"]
        for n in nodes
        if n["metadata"]["name"] != postgres_node
        and not n.get("spec", {}).get("unschedulable")
        and not any(
            t.get("effect") in ("NoSchedule", "NoExecute")
            for t in n.get("spec", {}).get("taints", [])
        )
    )
    if len(candidates) < 2:
        raise RuntimeError(
            f"pinNodes needs two schedulable nodes besides the Postgres node "
            f"{postgres_node!r}; found {candidates}"
        )
    return candidates[0], candidates[1]


def label_nodes() -> None:
    postgres_node = _kubectl_out(
        ["-n", NAMESPACE, "get", "pod", "postgres-0", "-o", "jsonpath={.spec.nodeName}"]
    )
    nodes = json.loads(_kubectl_out(["get", "nodes", "-o", "json"]))["items"]
    ateapi_node, runner_node = pick_role_nodes(nodes, postgres_node)
    # A label left by an earlier run on another node would give a role two
    # nodes.
    run_no_check(["kubectl", "label", "nodes", "--all", f"{ROLE_LABEL}-"])
    run(["kubectl", "label", "node", ateapi_node, f"{ROLE_LABEL}=ateapi", "--overwrite"])
    run(["kubectl", "label", "node", runner_node, f"{ROLE_LABEL}=runner", "--overwrite"])


def role_affinity(role: str) -> dict[str, Any]:
    return {
        "nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {
                "nodeSelectorTerms": [
                    {
                        "matchExpressions": [
                            {"key": ROLE_LABEL, "operator": "In", "values": [role]}
                        ]
                    }
                ]
            }
        }
    }


POSTGRES_EXPORTER_IMAGE = "quay.io/prometheuscommunity/postgres-exporter:v0.17.1"


def enable_postgres_stats() -> None:
    """Load pg_stat_statements and add a postgres_exporter sidecar, which
    reaches the server over the pod's unix socket (trusted by pg_hba). One
    patch, thus one Postgres restart, before ateapi rolls."""
    patch = {
        "spec": {
            "template": {
                "spec": {
                    "containers": [
                        {
                            "name": "postgres",
                            # Strategic merge replaces args whole; repeat the
                            # base manifest's config_file.
                            "args": [
                                "-c", "config_file=/etc/postgresql/postgresql.conf",
                                "-c", "shared_preload_libraries=pg_stat_statements",
                            ],
                        },
                        {
                            "name": "postgres-exporter",
                            "image": POSTGRES_EXPORTER_IMAGE,
                            "args": ["--collector.stat_statements"],
                            "env": [
                                {
                                    "name": "DATA_SOURCE_NAME",
                                    "value": "host=/var/run/postgresql user=postgres dbname=atepg sslmode=disable",
                                }
                            ],
                            "ports": [{"name": "pg-metrics", "containerPort": 9187}],
                            "volumeMounts": [
                                {"name": "socket", "mountPath": "/var/run/postgresql"}
                            ],
                            "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}},
                        },
                    ]
                }
            }
        }
    }
    _patch_and_wait("statefulset", "postgres", patch)
    run(
        ["kubectl", "-n", NAMESPACE, "exec", "postgres-0", "-c", "postgres", "--",
         "psql", "-U", "postgres", "-d", "atepg", "-v", "ON_ERROR_STOP=1",
         "-c", "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"]
    )


# ate-setup's ConfigMap of ateapi environment variables. ateapi reads
# ATE_API_POSTGRES_POOL_MAX_CONNS from it at start, over any pool_max_conns in
# the DSN.
ENV_CONFIGMAP = "ate-api-server-envvars"
POOL_MAX_CONNS_KEY = "ATE_API_POSTGRES_POOL_MAX_CONNS"


def pool_max_conns_patch(n: int) -> dict[str, Any]:
    return {"data": {POOL_MAX_CONNS_KEY: str(n)}}


def set_pool_max_conns(n: int) -> None:
    """Set the read/write pool size ateapi reads at start; the pin_ateapi
    rollout that follows picks it up. The owner and watch pools stay at 2
    and 3 connections per replica."""
    run(
        ["kubectl", "-n", NAMESPACE, "patch", "configmap", ENV_CONFIGMAP,
         "--type", "merge", "-p", json.dumps(pool_max_conns_patch(n))]
    )


def ateapi_patch(cfg: dict[str, Any]) -> dict[str, Any]:
    pod_spec: dict[str, Any] = {
        "containers": [
            {
                "name": "ate-api-server",
                "resources": {
                    "requests": {"cpu": str(cfg["cpu"]), "memory": cfg["memory"]},
                    "limits": {"cpu": str(cfg["cpu"]), "memory": cfg["memory"]},
                },
            }
        ]
    }
    if cfg["pinNodes"]:
        pod_spec["affinity"] = role_affinity("ateapi")
    return {"spec": {"replicas": cfg["replicas"], "template": {"spec": pod_spec}}}


def pin_ateapi(cfg: dict[str, Any]) -> None:
    _patch_and_wait("deployment", "ate-api-server", ateapi_patch(cfg))


def _patch_and_wait(kind: str, name: str, patch: dict[str, Any]) -> None:
    run(
        ["kubectl", "-n", NAMESPACE, "patch", kind, name,
         "--type=strategic", "-p", json.dumps(patch)]
    )
    run(
        ["kubectl", "-n", NAMESPACE, "rollout", "status", f"{kind}/{name}",
         "--timeout=600s"]
    )


def _kubectl_out(args: list[str]) -> str:
    return subprocess.run(
        ["kubectl", *args], capture_output=True, text=True, check=True
    ).stdout


def job_tmpl(manifests_dir: str) -> str:
    return os.path.join(manifests_dir, "runner-job.yaml.tmpl")


# Runner pod requests when a test sets none. Enough for the glutton and
# sweperf suites at their usual user counts; a boomer-driven class at
# thousands of users (agentsession marshals a 32Mi ingest payload per
# in-flight op) needs runnerCpu / runnerMemory raised in tests.yaml.
DEFAULT_RUNNER_CPU = "500m"
DEFAULT_RUNNER_MEMORY = "512Mi"


def job_subs(test: dict[str, Any]) -> dict[str, Any]:
    cfg = ateapi_config(test)
    pinned = cfg is not None and cfg["pinNodes"]
    return {
        "TEST_FILE": test["file"],
        "DURATION": test["duration"],
        "USERS": test["users"],
        "RUNNER_CPU": test.get("runnerCpu", DEFAULT_RUNNER_CPU),
        "RUNNER_MEMORY": test.get("runnerMemory", DEFAULT_RUNNER_MEMORY),
        # JSON is YAML flow syntax, thus it drops into `affinity:` as is.
        "RUNNER_AFFINITY": json.dumps(role_affinity("runner") if pinned else {}),
    }
