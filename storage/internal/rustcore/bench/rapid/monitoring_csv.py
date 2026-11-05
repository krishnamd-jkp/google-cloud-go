#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Fetches the VM's CPU and memory time series over a whole rapidbench pass, for the timeline chart.

Usage:
  python3 monitoring_csv.py <results dir> > <results dir>/monitoring.csv

Covers the first measured window minus 5 minutes to the last one plus 5 minutes, using the project
and instance id recorded in the runs' summary.json. Writes "time,metric,value" rows for
cpu_utilization_percent (compute.googleapis.com/instance/cpu/utilization, in %) and
memory_percent_used (agent.googleapis.com/memory/percent_used, state "used"). Standard library only.
"""

import datetime as dt
import glob
import json
import os
import sys
import urllib.parse
import urllib.request

from cloud_metrics import METRICS, access_token


def series(project, instance, metric_filter, start, end, token):
    flt = f'{metric_filter} AND resource.labels.instance_id="{instance}"'
    params = {"filter": flt, "interval.startTime": start, "interval.endTime": end}
    url = (
        f"https://monitoring.googleapis.com/v3/projects/{project}/timeSeries?"
        + urllib.parse.urlencode(params)
    )
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=30) as r:
        body = json.load(r)
    out = []
    for ts in body.get("timeSeries", []):
        for p in ts.get("points", []):
            v = p["value"]
            out.append((p["interval"]["endTime"], float(v.get("doubleValue", v.get("int64Value", 0)))))
    return out


def parse(ts):
    return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))


def main(results):
    runs = [json.load(open(p)) for p in glob.glob(os.path.join(results, "*", "summary.json"))]
    if not runs:
        sys.exit(f"no summary.json under {results}")
    project, instance = runs[0]["gce_project"], runs[0]["gce_instance_id"]
    t0 = min(parse(r["window_start"]) for r in runs) - dt.timedelta(minutes=5)
    t1 = max(parse(r["window_end"]) for r in runs) + dt.timedelta(minutes=5)
    fmt = "%Y-%m-%dT%H:%M:%SZ"
    token = access_token()
    print("time,metric,value")
    for name, (flt, scale) in METRICS.items():
        for t, v in sorted(series(project, instance, flt, t0.strftime(fmt), t1.strftime(fmt), token)):
            print(f"{t},{name},{v * scale:.4f}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    main(sys.argv[1])
