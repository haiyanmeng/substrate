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


"""Fetch Postgres active connections by wait event, averaged over each
setup's saturated steps, from the benchmarking Prometheus into
data/postgres-wait-events.csv, which make_charts.py reads.

    kubectl -n benchmarking port-forward svc/prometheus 19090:9090 &
    python3 ateapi-cold-resume-charts/fetch_wait_events.py

Run from the repository root while Prometheus still holds the runs.
"""

import csv
import json
import os
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import make_charts as m  # noqa: E402

PROM = "http://localhost:19090"
# A wait event absent from a sample counts as zero there: sum each over the
# window and divide by the samples of the whole, not avg_over_time, which
# averages a series only over the samples it appears in.
EXPR = ('sum_over_time((sum by (wait_event_type, wait_event) '
        '(pg_stat_activity_count{{datname="atepg",state="active"}}))[{d}s:15s])')
SAMPLES = 'count_over_time((sum(pg_stat_activity_count{{datname="atepg"}}))[{d}s:15s])'


def query(expr, t):
    url = f"{PROM}/api/v1/query?" + urllib.parse.urlencode({"query": expr, "time": t})
    with urllib.request.urlopen(url, timeout=30) as r:
        data = json.load(r)
    if data.get("status") != "success":
        raise RuntimeError(data)
    return data["data"]["result"]


def category(wtype, wevent):
    if not wtype:
        return "running"
    if wtype == "LWLock" and wevent == "WALWrite":
        return "walwrite"
    if wtype == "Lock":
        return "lock"
    if wtype == "Client" and wevent == "ClientRead":
        return "clientread"
    return "other"


def main():
    setups = [("size0", lab, path, sat) for lab, path, sat in m.SIZE0]
    setups += [("size10", f"{n} × 8", path, sat) for n, path, sat, _ in m.SIZE10]
    out = []
    for pg, lab, path, sat_from in setups:
        sat = [d for d in m.steps(path) if d["target_rps"] >= sat_from]
        sums = {k: 0.0 for k in ("walwrite", "lock", "clientread", "running", "other")}
        for d in sat:
            a, b = d["window"]
            n = float(query(SAMPLES.format(d=b - a), b)[0]["value"][1])
            for r in query(EXPR.format(d=b - a), b):
                sums[category(r["metric"].get("wait_event_type", ""), r["metric"].get("wait_event", ""))] += float(r["value"][1]) / n
        out.append({"postgres": pg, "setup": lab, **{k: round(v / len(sat), 2) for k, v in sums.items()}})
        print(pg, lab, out[-1])
    with open(os.path.join(m.OUT, "data", "postgres-wait-events.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(out[0]))
        w.writeheader()
        w.writerows(out)


if __name__ == "__main__":
    main()
