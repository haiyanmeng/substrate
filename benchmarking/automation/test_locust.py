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
import unittest
from unittest import mock

from testtypes import locust


def entry(**fields):
    return {
        "name": "t",
        "file": "/app/tests/resumepaused.py,/app/shapes/ladder_shape.py",
        "duration": "21m",
        "users": 200,
        **fields,
    }


class ResultsUploadedTest(unittest.TestCase):
    PREFIX = "gs://b/bench/runs/t/run_date=2026-10-11"

    def gcloud(self, listing, statuses):
        """Patch gcloud storage: ls prints listing, cat prints statuses[uri]."""
        def run(args, **kwargs):
            self.assertIn("timeout", kwargs)
            if args[2] == "ls":
                return mock.Mock(returncode=0 if listing else 1, stdout="\n".join(listing))
            return mock.Mock(returncode=0, stdout=json.dumps(statuses[args[3]]))

        patcher = mock.patch.object(locust.subprocess, "run", side_effect=run)
        patcher.start()
        self.addCleanup(patcher.stop)

    def uri(self, run_ts):
        return f"{self.PREFIX}/run_ts={run_ts}/run_tag=c/status.json"

    def test_finds_this_runs_status(self):
        self.gcloud([self.uri(1000)], {self.uri(1000): {"stats_generated": True}})
        self.assertEqual(locust.results_uploaded(entry(name="t"), "gs://b/bench/", "c", 900), "complete")

    def test_no_stats_is_failed(self):
        self.gcloud([self.uri(1000)], {self.uri(1000): {"stats_generated": False}})
        self.assertEqual(locust.results_uploaded(entry(name="t"), "gs://b/bench", "c", 900), "failed")

    def test_skips_an_earlier_run_at_the_same_commit(self):
        self.gcloud([self.uri(500)], {self.uri(500): {"stats_generated": True}})
        self.assertIsNone(locust.results_uploaded(entry(name="t"), "gs://b/bench", "c", 900))

    def test_nothing_uploaded_yet(self):
        self.gcloud([], {})
        self.assertIsNone(locust.results_uploaded(entry(name="t"), "gs://b/bench", "c", 900))


if __name__ == "__main__":
    unittest.main()
