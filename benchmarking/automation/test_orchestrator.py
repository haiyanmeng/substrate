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

"""Unit tests for orchestrator.py: python3 benchmarking/automation/test_orchestrator.py"""

import json
import os
import subprocess
import unittest
from unittest import mock

import yaml

import orchestrator
from testtypes import locust


class DeployWorkloadsTest(unittest.TestCase):
    @mock.patch("orchestrator.run")
    def test_defaults_omit_optional_flags(self, run):
        orchestrator.deploy_workloads()
        run.assert_called_once_with(
            [
                "benchmarking/workloads/deploy.sh",
                "--deploy",
                "--worker-count",
                "1",
                "--sandbox-class",
                "gvisor",
            ]
        )

    @mock.patch("orchestrator.run")
    def test_worker_memory(self, run):
        orchestrator.deploy_workloads(worker_memory="12Gi")
        cmd = run.call_args.args[0]
        i = cmd.index("--worker-memory")
        self.assertEqual(cmd[i + 1], "12Gi")

    @mock.patch("orchestrator.run")
    def test_all_options(self, run):
        orchestrator.deploy_workloads(
            worker_count=3,
            sandbox_class="microvm",
            actor_memory="1536Mi",
            wait_timeout_secs=600,
            worker_memory="5Gi",
        )
        run.assert_called_once_with(
            [
                "benchmarking/workloads/deploy.sh",
                "--deploy",
                "--worker-count",
                "3",
                "--sandbox-class",
                "microvm",
                "--actor-memory",
                "1536Mi",
                "--worker-memory",
                "5Gi",
                "--wait-timeout",
                "600",
            ]
        )


class RunnerSizingTest(unittest.TestCase):
    TMPL = os.path.join(os.path.dirname(__file__), "manifests", "runner-job.yaml.tmpl")

    def render(self, test):
        subs = {"JOB_NAME": "j", "IMAGE": "i", "TAG": "t", "NAME": "n", "DEST": "d"}
        subs.update(locust.job_subs(test))
        text = orchestrator.render_template(self.TMPL, subs)
        job = next(d for d in yaml.safe_load_all(text) if d and d.get("kind") == "Job")
        return job["spec"]["template"]["spec"]["containers"][0]["resources"]

    def test_defaults(self):
        res = self.render({"file": "f", "duration": "1m", "users": 1})
        self.assertEqual(res, {"requests": {"cpu": "500m", "memory": "512Mi"}})

    def test_runner_cpu_and_memory(self):
        res = self.render(
            {"file": "f", "duration": "1m", "users": 1000, "runnerCpu": "4", "runnerMemory": "8Gi"}
        )
        self.assertEqual(res["requests"], {"cpu": "4", "memory": "8Gi"})
        self.assertNotIn("limits", res)

    def test_no_placeholder_survives(self):
        subs = {"JOB_NAME": "j", "IMAGE": "i", "TAG": "t", "NAME": "n", "DEST": "d"}
        subs.update(locust.job_subs({"file": "f", "duration": "1m", "users": 1}))
        self.assertNotIn("${", orchestrator.render_template(self.TMPL, subs))


class JobNameTest(unittest.TestCase):
    COMMIT = "ac41c06deadbeef"

    def test_short_name_keeps_full_test_name(self):
        name = orchestrator.job_name("Eng Review", self.COMMIT)
        self.assertRegex(name, r"^runner-eng-review-ac41c06-[0-9a-f]{6}$")

    def test_long_name_fits_a_label_and_keeps_suffix(self):
        test = "very-long-benchmark-name-that-overflows-the-kubernetes-label-limit"
        name = orchestrator.job_name(test, self.COMMIT)
        self.assertLessEqual(len(name), orchestrator.MAX_JOB_NAME_LEN)
        self.assertRegex(name, r"-ac41c06-[0-9a-f]{6}$")
        self.assertTrue(name.startswith("runner-very-long-benchmark-name"))

    def test_truncation_never_leaves_a_double_hyphen(self):
        # Cutting right after a hyphen must not yield "...-x--ac41c06-...".
        for i in range(1, 80):
            test = "-".join(["ab"] * i)
            name = orchestrator.job_name(test, self.COMMIT)
            self.assertLessEqual(len(name), orchestrator.MAX_JOB_NAME_LEN)
            self.assertNotIn("--", name, test)
            self.assertRegex(name, r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")

    def test_two_runs_of_the_same_long_test_differ(self):
        test = "x" * 100
        self.assertNotEqual(
            orchestrator.job_name(test, self.COMMIT),
            orchestrator.job_name(test, self.COMMIT),
        )



def completed(stdout="", returncode=0, stderr=""):
    return mock.Mock(stdout=stdout, returncode=returncode, stderr=stderr)


JOB_SUCCEEDED = completed(json.dumps({"status": {"succeeded": 1}}))
JOB_RUNNING = completed(json.dumps({"status": {"active": 1}}))
CREDENTIALS_DOWN = completed(returncode=1, stderr="getting credentials: exec failed")


class WaitForJobTest(unittest.TestCase):
    """wait_for_job against a fake clock: time.sleep advances it."""

    def setUp(self):
        self.now = 0.0
        for target, fake in {
            "orchestrator.time.time": lambda: self.now,
            "orchestrator.time.sleep": self.sleep,
        }.items():
            patcher = mock.patch(target, fake)
            patcher.start()
            self.addCleanup(patcher.stop)

    def sleep(self, seconds):
        self.now += seconds

    def polls(self, *results):
        """Patch kubectl to answer each poll with the next result; the last
        repeats. A result that is an exception is raised instead."""
        answers = list(results)

        def run(*_, **kwargs):
            self.assertIn("timeout", kwargs, "every poll must be bounded")
            answer = answers.pop(0) if len(answers) > 1 else answers[0]
            if isinstance(answer, BaseException):
                raise answer
            return answer

        patcher = mock.patch("orchestrator.subprocess.run", side_effect=run)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_succeeds(self):
        self.polls(JOB_RUNNING, JOB_RUNNING, JOB_SUCCEEDED)
        self.assertEqual(orchestrator.wait_for_job("j", 600), "complete")

    def test_a_hung_poll_is_cut_off_and_retried(self):
        self.polls(subprocess.TimeoutExpired("kubectl", 60), JOB_SUCCEEDED)
        self.assertEqual(orchestrator.wait_for_job("j", 600), "complete")

    def test_failing_polls_fall_back_to_the_uploaded_results(self):
        self.polls(CREDENTIALS_DOWN)
        asked = []

        def uploaded():
            asked.append(self.now)
            return "complete" if len(asked) >= 2 else None

        self.assertEqual(orchestrator.wait_for_job("j", 3600, uploaded), "complete")
        # First asked once polls had failed for UPLOAD_CHECK_AFTER_S, then
        # again no sooner than UPLOAD_CHECK_EVERY_S later.
        self.assertGreaterEqual(asked[0], orchestrator.UPLOAD_CHECK_AFTER_S)
        self.assertGreaterEqual(asked[1] - asked[0], orchestrator.UPLOAD_CHECK_EVERY_S)
        self.assertLess(self.now, 3600)

    def test_recovered_polls_restart_the_failure_clock(self):
        # Failures shorter than UPLOAD_CHECK_AFTER_S, apart, never ask.
        short = [CREDENTIALS_DOWN] * 10 + [JOB_RUNNING]
        self.polls(*short, *short, JOB_SUCCEEDED)
        uploaded = mock.Mock(return_value="complete")
        self.assertEqual(orchestrator.wait_for_job("j", 3600, uploaded), "complete")
        uploaded.assert_not_called()

    def test_the_deadline_asks_once_more(self):
        self.polls(JOB_RUNNING)
        self.assertEqual(orchestrator.wait_for_job("j", 60, lambda: "failed"), "failed")

    def test_timeout_without_uploaded_results(self):
        self.polls(CREDENTIALS_DOWN)
        self.assertEqual(orchestrator.wait_for_job("j", 900), "timeout")


if __name__ == "__main__":
    unittest.main()
