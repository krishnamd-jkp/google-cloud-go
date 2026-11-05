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
"""Fetches Cloud Monitoring CPU and memory for a rapidbench run (as the Java PoC's cloud_metrics.py).

Reads the measurement window and GCE instance id from a run's summary.json and queries:
  compute.googleapis.com/instance/cpu/utilization   (VM CPU, 0..1, 60 s samples)
  agent.googleapis.com/memory/percent_used          (state="used"; needs the Ops Agent)
Reports mean and peak of each over the window and writes them back into summary.json under
"cloud_monitoring".

Usage:
  python3 cloud_metrics.py <out>/<run>/summary.json [more summary.json ...]

Auth: an access token from the GCE metadata server, or `gcloud auth print-access-token`.
Metrics are ingested with a delay of up to ~3-4 minutes, so run this a few minutes after the
benchmark ends. Only the Python standard library is used.
"""

import datetime as dt
import json
import subprocess
import sys
import urllib.parse
import urllib.request

METRICS = {
    "cpu_utilization_percent": (
        'metric.type="compute.googleapis.com/instance/cpu/utilization"',
        100.0,  # fraction -> percent
    ),
    "memory_percent_used": (
        'metric.type="agent.googleapis.com/memory/percent_used" AND metric.labels.state="used"',
        1.0,
    ),
}


def access_token():
    try:
        req = urllib.request.Request(
            "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
            headers={"Metadata-Flavor": "Google"},
        )
        with urllib.request.urlopen(req, timeout=2) as r:
            return json.load(r)["access_token"]
    except Exception:
        return subprocess.check_output(["gcloud", "auth", "print-access-token"], text=True).strip()


def query(project, instance_id, metric_filter, start, end, token):
    flt = f'{metric_filter} AND resource.labels.instance_id="{instance_id}"'
    params = {
        "filter": flt,
        "interval.startTime": start,
        "interval.endTime": end,
        "view": "FULL",
    }
    url = (
        f"https://monitoring.googleapis.com/v3/projects/{project}/timeSeries?"
        + urllib.parse.urlencode(params)
    )
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=30) as r:
        body = json.load(r)
    values = []
    for ts in body.get("timeSeries", []):
        for p in ts.get("points", []):
            v = p["value"]
            values.append(float(v.get("doubleValue", v.get("int64Value", 0))))
    return values


def pad(ts, seconds):
    t = dt.datetime.fromisoformat(ts.replace("Z", "+00:00")) + dt.timedelta(seconds=seconds)
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def main(paths):
    token = access_token()
    for path in paths:
        with open(path) as f:
            s = json.load(f)
        project, instance = s.get("gce_project"), s.get("gce_instance_id")
        if not project or not instance:
            print(f"{path}: no GCE instance info (not run on GCE?)", file=sys.stderr)
            continue
        # Only samples inside the measured window, so warm-up and neighbouring runs don't leak in.
        start, end = pad(s["window_start"], 0), pad(s["window_end"], 0)
        out = {"window_start": start, "window_end": end}
        for name, (flt, scale) in METRICS.items():
            vals = [v * scale for v in query(project, instance, flt, start, end, token)]
            out[name] = {
                "samples": len(vals),
                "mean": round(sum(vals) / len(vals), 2) if vals else None,
                "peak": round(max(vals), 2) if vals else None,
            }
        s["cloud_monitoring"] = out
        with open(path, "w") as f:
            json.dump(s, f, indent=2)
        print(f"{path}: {json.dumps(out)}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1:])
