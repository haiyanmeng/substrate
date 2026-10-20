"""Runs one tests.yaml entry against the cluster in $KUBECONFIG, through
orchestrator.py's own steps, from a local checkout instead of the CronJob.

    python3 driver.py <repo> <test-name> <dest>

RUNNER_CPU overrides the entry's runnerCpu, and RUNNER_NODE_SELECTOR
(key=value) pins the runner Job to the nodes carrying that label.
"""

import os
import subprocess
import sys
from pathlib import Path

repo, test_name, dest = sys.argv[1:4]
os.chdir(repo)
sys.path.insert(0, str(Path(repo, "benchmarking/automation")))

import yaml  # noqa: E402

import orchestrator as o  # noqa: E402
from testtypes import TYPES  # noqa: E402

o.source_env(".ate-dev-env.sh")
tests = yaml.safe_load(Path("benchmarking/automation/tests.yaml").read_text())["tests"]
test = next(t for t in tests if t["name"] == test_name)
o.validate_and_normalize_tests([test])
if os.environ.get("RUNNER_CPU"):
    test["runnerCpu"] = os.environ["RUNNER_CPU"]
if os.environ.get("RUNNER_NODE_SELECTOR"):
    key, _, value = os.environ["RUNNER_NODE_SELECTOR"].partition("=")
    render = o.render_template

    def render_pinned(*args, **kwargs):
        text = render(*args, **kwargs)
        anchor = "      restartPolicy: Never\n"
        assert text.count(anchor) == 1, "runner Job template changed"
        return text.replace(anchor, anchor + f'      nodeSelector:\n        {key}: "{value}"\n')

    o.render_template = render_pinned
commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
print(f"=== {test_name} at {commit} -> {dest}", flush=True)

o.run(["gcloud", "auth", "configure-docker", o.registry_host(os.environ["KO_DOCKER_REPO"]), "--quiet"])
ttype = o.test_type(test)
image = TYPES[ttype].build_image(commit)

o.teardown_workloads()
o.teardown_substrate()
status = "error"
try:
    o.deploy_substrate(o.substrate_args(test))
    TYPES[ttype].pre_test(test)
    o.deploy_workloads(
        test.get("workerCount", 1),
        test.get("sandboxClass", "gvisor"),
        test.get("actorMemory", ""),
        test.get("workerWaitTimeout", ""),
        test.get("workerMemory", ""),
        o.isolate_workload_args(test),
        test.get("actorCPU", ""),
    )
    status = o.run_test(test, image, dest, commit, "benchmarking/automation/manifests")
finally:
    print(f"=== result: {status}; tearing down", flush=True)
    o.teardown_workloads()
    o.teardown_substrate()
sys.exit(0 if status == "complete" else 1)
