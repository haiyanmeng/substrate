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


class ValidateTest(unittest.TestCase):
    def test_missing_required(self):
        test = entry()
        del test["users"]
        with self.assertRaises(ValueError):
            locust.validate(test)


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
        pod = self.render(entry())
        runner = pod["containers"][0]
        self.assertEqual(
            runner["resources"], {"requests": {"cpu": "500m", "memory": "512Mi"}}
        )
        # monitoring.yaml's boomer-worker job scrapes this port.
        self.assertIn({"name": "boomer-metrics", "containerPort": 8001}, runner["ports"])

    def test_sized_runner_renders(self):
        pod = self.render(entry(runnerCpu="8", runnerMemory="2Gi",
                                flags=["--actors", "200"]))
        runner = pod["containers"][0]
        self.assertEqual(runner["resources"]["requests"], {"cpu": "8", "memory": "2Gi"})
        self.assertEqual(runner["args"][-2:], ["--actors", "200"])


class TestsYamlTest(unittest.TestCase):
    def test_entries_validate(self):
        path = os.path.join(os.path.dirname(__file__), "tests.yaml")
        with open(path) as f:
            tests = yaml.safe_load(f)["tests"]
        orchestrator.validate_and_normalize_tests(tests)


if __name__ == "__main__":
    unittest.main()
