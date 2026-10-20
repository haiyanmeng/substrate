"""Install, shape and deploy workloads for one tests.yaml entry, without a run."""
import os, sys
from pathlib import Path
repo, test_name = sys.argv[1:3]
os.chdir(repo)
sys.path.insert(0, str(Path(repo, "benchmarking/automation")))
import yaml
import orchestrator as o
from testtypes import TYPES
o.source_env(".ate-dev-env.sh")
test = next(t for t in yaml.safe_load(Path("benchmarking/automation/tests.yaml").read_text())["tests"] if t["name"] == test_name)
o.validate_and_normalize_tests([test])
o.teardown_workloads(); o.teardown_substrate()
o.deploy_substrate(o.substrate_args(test))
TYPES[o.test_type(test)].pre_test(test)
o.deploy_workloads(test.get("workerCount", 1), "gvisor", "", "", "", o.isolate_workload_args(test), test.get("actorCPU", ""))
