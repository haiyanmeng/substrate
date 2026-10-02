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


def node(name, unschedulable=False, taints=()):
    return {
        "metadata": {"name": name},
        "spec": {"unschedulable": unschedulable, "taints": list(taints)},
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
            {"ateapi": {"cpu": 2, "memory": "4 GiB"}},
            {"runnerCpu": "8 cores"},
            {"runnerMemory": 0},
        ]
        for fields in bad:
            with self.assertRaises(ValueError, msg=repr(fields)):
                locust.validate(entry(**fields))

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


class PoolMaxConnsTest(unittest.TestCase):
    def test_uri(self):
        cases = {
            "postgres://u:p@h/db": "postgres://u:p@h/db?pool_max_conns=16",
            "postgres://h/db?sslmode=disable":
                "postgres://h/db?sslmode=disable&pool_max_conns=16",
            "postgres://h/db?pool_max_conns=64&pool_min_conns=4":
                "postgres://h/db?pool_max_conns=16&pool_min_conns=4",
        }
        for dsn, want in cases.items():
            self.assertEqual(locust.with_pool_max_conns(dsn, 16), want)

    def test_keyword(self):
        self.assertEqual(
            locust.with_pool_max_conns("host=h dbname=db", 16),
            "host=h dbname=db pool_max_conns=16",
        )
        self.assertEqual(
            locust.with_pool_max_conns("host=h pool_max_conns=64 dbname=db", 16),
            "host=h pool_max_conns=16 dbname=db",
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
