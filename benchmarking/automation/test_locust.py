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

"""Unit tests for testtypes/locust.py:
python3 benchmarking/automation/test_locust.py"""

import json
import os
import unittest
from unittest import mock

import yaml

import orchestrator
from testtypes import locust


def entry(**fields):
    return {
        "name": "t",
        "file": "/app/tests/resumecold.py,/app/shapes/ladder_shape.py",
        "duration": "21m",
        "users": 200,
        **fields,
    }


def node(name, unschedulable=False, taints=(), cpu="88"):
    return {
        "metadata": {"name": name},
        "spec": {"unschedulable": unschedulable, "taints": list(taints)},
        "status": {"allocatable": {"cpu": cpu}},
    }


class ValidateTest(unittest.TestCase):
    def test_without_ateapi(self):
        locust.validate(entry())
        self.assertIsNone(locust.ateapi_config(entry()))

    def test_defaults(self):
        test = entry(ateapi={"cpu": 2})
        locust.validate(test)
        cfg = locust.ateapi_config(test)
        self.assertEqual(cfg["cpu"], 2)
        self.assertEqual(cfg["replicas"], 1)
        self.assertEqual(cfg["memory"], "4Gi")
        self.assertIsNone(cfg["poolMaxConns"])
        self.assertTrue(cfg["pgStats"])
        self.assertTrue(cfg["pinNodes"])

    def test_valid_knobs(self):
        locust.validate(entry(
            runnerCpu="8",
            runnerMemory="2Gi",
            ateapi={"cpu": "1500m", "memory": "8Gi", "replicas": 2,
                    "poolMaxConns": 16, "pgStats": False, "pinNodes": False},
        ))

    def test_invalid(self):
        bad = [
            {"ateapi": {}},
            {"ateapi": "2"},
            {"ateapi": {"cpu": 0}},
            {"ateapi": {"cpu": "two"}},
            {"ateapi": {"cpu": True}},
            {"ateapi": {"cpu": 2, "cpus": 2}},
            {"ateapi": {"cpu": 2, "replicas": 0}},
            {"ateapi": {"cpu": 2, "replicas": True}},
            {"ateapi": {"cpu": 2, "poolMaxConns": "64"}},
            {"ateapi": {"cpu": 2, "pgStats": "yes"}},
            {"ateapi": {"cpu": 2, "pinNodes": "yes"}},
            {"ateapi": {"cpu": 2, "memory": "4 GiB"}},
        ]
        for fields in bad:
            with self.assertRaises(ValueError, msg=repr(fields)):
                locust.validate(entry(**fields))

    def test_pin_nodes_needs_an_uncordoned_install(self):
        cordoned = {"ateArgs": ["--cordon-control-plane"]}
        with self.assertRaisesRegex(ValueError, "cordon"):
            locust.validate(entry(ateapi={"cpu": 2}, **cordoned))
        locust.validate(entry(ateapi={"cpu": 2, "pinNodes": False}, **cordoned))
        locust.validate(entry(ateapi={"cpu": 2}, ateArgs=["--cordon-control-plane=false"]))

    def test_missing_required(self):
        test = entry()
        del test["users"]
        with self.assertRaises(ValueError):
            locust.validate(test)


class AteapiPatchTest(unittest.TestCase):
    def test_guaranteed_and_pinned(self):
        patch = locust.ateapi_patch(locust.ateapi_config(entry(ateapi={"cpu": 2})))
        self.assertEqual(patch["spec"]["replicas"], 1)
        pod = patch["spec"]["template"]["spec"]
        resources = pod["containers"][0]["resources"]
        self.assertEqual(resources["requests"], {"cpu": "2", "memory": "4Gi"})
        self.assertEqual(resources["limits"], resources["requests"])
        self.assertEqual(pod["affinity"], locust.role_affinity("ateapi"))

    def test_unpinned(self):
        cfg = locust.ateapi_config(entry(ateapi={"cpu": 2, "pinNodes": False}))
        self.assertNotIn("affinity", locust.ateapi_patch(cfg)["spec"]["template"]["spec"])


class PickRoleNodesTest(unittest.TestCase):
    def test_skips_postgres_and_unschedulable_nodes(self):
        nodes = [
            node("n-d"),
            node("n-a"),
            node("n-b", unschedulable=True),
            node("n-c", taints=[{"key": "k", "effect": "NoSchedule"}]),
            node("n-e", taints=[{"key": "k", "effect": "PreferNoSchedule"}]),
        ]
        self.assertEqual(locust.pick_role_nodes(nodes, "n-a"), ("n-d", "n-e"))

    def test_too_few_nodes(self):
        with self.assertRaises(RuntimeError):
            locust.pick_role_nodes([node("pg"), node("n-a")], "pg")

    def test_runner_takes_the_load_generator_pool(self):
        loadgen = [dict(locust.LOADGEN_TAINT)]
        nodes = [node("n-a"), node("n-b"), node("lg-b", taints=loadgen), node("lg-a", taints=loadgen)]
        self.assertEqual(locust.pick_role_nodes(nodes, "n-b"), ("n-a", "lg-a"))

    def test_ateapi_never_takes_the_load_generator_pool(self):
        loadgen = [dict(locust.LOADGEN_TAINT)]
        with self.assertRaises(RuntimeError):
            locust.pick_role_nodes([node("pg"), node("lg-a", taints=loadgen), node("lg-b", taints=loadgen)], "pg")

    def test_each_role_takes_a_node_that_fits(self):
        loadgen = [dict(locust.LOADGEN_TAINT)]
        nodes = [node("n-a", cpu="3900m"), node("n-b", cpu="7950m"), node("n-c", cpu="15890m"),
                 node("lg-a", taints=loadgen, cpu="3920m"), node("lg-b", taints=loadgen, cpu="7920m")]
        # Two 4-CPU replicas share the ateapi node; the runner asks for 8.
        self.assertEqual(
            locust.pick_role_nodes(nodes, "pg", ateapi_millis=8000, runner_millis=7900),
            ("n-c", "lg-b"),
        )
        # No load-generator node fits the runner, so it takes an untainted one.
        self.assertEqual(
            locust.pick_role_nodes(nodes, "pg", ateapi_millis=8000, runner_millis=7930),
            ("n-c", "n-b"),
        )

    def test_no_node_fits(self):
        with self.assertRaisesRegex(RuntimeError, "8000m CPU for ateapi"):
            locust.pick_role_nodes([node("n-a", cpu="4"), node("n-b", cpu="4")], "pg", ateapi_millis=8000)
        with self.assertRaisesRegex(RuntimeError, "8000m CPU for the runner"):
            locust.pick_role_nodes([node("n-a", cpu="16"), node("n-b", cpu="4")], "pg",
                                   ateapi_millis=8000, runner_millis=8000)

    def test_other_taints_are_no_load_generator_pool(self):
        nodes = [
            node("n-a"),
            node("n-b"),
            node("x", taints=[{"key": "ate.dev/benchmarking", "value": "true", "effect": "NoExecute"}]),
            node("y", taints=[dict(locust.LOADGEN_TAINT), {"key": "k", "effect": "NoSchedule"}]),
        ]
        self.assertEqual(locust.pick_role_nodes(nodes, "pg"), ("n-a", "n-b"))


class CpuMillisTest(unittest.TestCase):
    def test_spellings(self):
        cases = {2: 2000, 0.5: 500, "8": 8000, "1.5": 1500, "1500m": 1500,
                 "87810m": 87810, "1e1": 10000, "2k": 2000000}
        for q, want in cases.items():
            with self.subTest(q=q):
                self.assertEqual(locust._cpu_millis(q), want)


class LabelNodesTest(unittest.TestCase):
    def test_ateapi_node_holds_every_replica(self):
        asked = {}

        def pick(nodes, postgres_node, **need):
            asked.update(need)
            return "n-a", "n-b"

        for name, fake in {
            "_kubectl_out": lambda args: '{"items": []}' if "nodes" in args else "pg",
            "pick_role_nodes": pick,
            "run": lambda *_: None,
            "run_no_check": lambda *_: None,
        }.items():
            patcher = mock.patch.object(locust, name, fake)
            patcher.start()
            self.addCleanup(patcher.stop)
        locust.label_nodes(locust.ateapi_config(entry(ateapi={"cpu": "1500m", "replicas": 2})), "8")
        self.assertEqual(asked, {"ateapi_millis": 3000, "runner_millis": 8000})


class PreTestTest(unittest.TestCase):
    # Enabling statistics restarts Postgres onto any node, so the roles that
    # must avoid its node are assigned only afterwards.
    def test_labels_nodes_after_postgres_restarts(self):
        calls = []
        stubs = {
            name: mock.patch.object(locust, name, lambda *_, name=name: calls.append(name))
            for name in ("enable_postgres_stats", "label_nodes", "set_pool_max_conns", "pin_ateapi", "run_no_check")
        }
        for stub in stubs.values():
            stub.start()
            self.addCleanup(stub.stop)
        locust.pre_test(entry(ateapi={"cpu": 2, "poolMaxConns": 40}))
        self.assertEqual(
            [c for c in calls if c != "run_no_check"],
            ["enable_postgres_stats", "label_nodes", "set_pool_max_conns", "pin_ateapi"],
        )


class PoolMaxConnsTest(unittest.TestCase):
    def test_patch(self):
        self.assertEqual(
            locust.pool_max_conns_patch(40),
            {"data": {"ATE_API_POSTGRES_POOL_MAX_CONNS": "40"}},
        )


class JobTest(unittest.TestCase):
    def render(self, test):
        subs = {
            "JOB_NAME": "j", "NAME": "t", "TAG": "c", "IMAGE": "i", "DEST": "d",
            **locust.job_subs(test),
        }
        tmpl = locust.job_tmpl(os.path.join(os.path.dirname(__file__), "manifests"))
        text = orchestrator.render_template(tmpl, subs, test.get("flags", []))
        self.assertNotIn("${", text)
        jobs = [d for d in yaml.safe_load_all(text) if d and d["kind"] == "Job"]
        self.assertEqual(len(jobs), 1)
        return jobs[0]["spec"]["template"]["spec"]

    def test_defaults_render(self):
        test = entry()
        pod = self.render(test)
        self.assertEqual(pod["affinity"], {})
        self.assertEqual(
            pod["containers"][0]["resources"],
            {"requests": {"cpu": "500m", "memory": "512Mi"}},
        )
        # monitoring.yaml's boomer-worker job scrapes this port.
        self.assertIn(
            {"name": "boomer-metrics", "containerPort": 8001},
            pod["containers"][0]["ports"],
        )

    def test_pinned_runner_renders(self):
        test = entry(runnerCpu="8", runnerMemory="2Gi", ateapi={"cpu": 2},
                     flags=["--actors", "200"])
        pod = self.render(test)
        self.assertEqual(pod["affinity"], locust.role_affinity("runner"))
        self.assertEqual(
            pod["containers"][0]["resources"]["requests"],
            {"cpu": "8", "memory": "2Gi"},
        )
        self.assertEqual(pod["containers"][0]["args"][-2:], ["--actors", "200"])

    def test_unpinned_runner_has_no_affinity(self):
        subs = locust.job_subs(entry(ateapi={"cpu": 2, "pinNodes": False}))
        self.assertEqual(json.loads(subs["RUNNER_AFFINITY"]), {})


class TestsYamlTest(unittest.TestCase):
    def test_entries_validate(self):
        path = os.path.join(os.path.dirname(__file__), "tests.yaml")
        with open(path) as f:
            tests = yaml.safe_load(f)["tests"]
        orchestrator.validate_and_normalize_tests(tests)


if __name__ == "__main__":
    unittest.main()
