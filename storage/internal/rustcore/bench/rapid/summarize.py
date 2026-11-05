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
"""Markdown comparison of rapidbench runs (as the Java PoC's summarize.py).

Usage:
  python3 summarize.py <out dir> > <out dir>/summary.md

For each op, one row per configuration (label) with the median over rounds and the min..max
spread, then every individual run, so nothing is hidden. Standard library only.
"""

import glob
import json
import os
import statistics
import sys
from collections import defaultdict


def fmt(v, digits=1):
    if v is None:
        return "-"
    if isinstance(v, (int, float)):
        return f"{v:,.{digits}f}"
    return str(v)


def get(s, *path):
    for p in path:
        if not isinstance(s, dict):
            return None
        s = s.get(p)
    return s


COLUMNS = [
    # (header, path, digits)
    ("MB/s", ("throughput_MBps",), 1),
    ("Gbps", ("throughput_Gbps",), 2),
    ("items/s", ("items_per_sec",), 0),
    ("ops/s", ("ops_per_sec",), 2),
    ("p50 ms", ("latency_ms_p50",), 1),
    ("p90 ms", ("latency_ms_p90",), 1),
    ("p99 ms", ("latency_ms_p99",), 1),
    ("proc CPU %", ("process_cpu_percent_of_all_cores",), 1),
    ("peak RSS MiB", ("process_peak_rss_mib",), 0),
    ("VM CPU % mean", ("cloud_monitoring", "cpu_utilization_percent", "mean"), 1),
    ("VM mem % peak", ("cloud_monitoring", "memory_percent_used", "peak"), 1),
    ("TCP :443", ("params", "network", "tcp_established_443_mid"), 0),
    ("NIC rx Gbps", ("params", "network", "nic_rx_Gbps"), 2),
    ("NIC tx Gbps", ("params", "network", "nic_tx_Gbps"), 2),
]


def main(root):
    runs = defaultdict(lambda: defaultdict(list))
    for path in sorted(glob.glob(os.path.join(root, "*", "summary.json"))):
        with open(path) as f:
            s = json.load(f)
        runs[s["op"]][s.get("label", s["engine"])].append(
            (os.path.basename(os.path.dirname(path)), s)
        )

    for op, by_label in runs.items():
        is_list = op.startswith("LIST")
        is_list = is_list or op.startswith("OPEN")
        drop = {"MB/s", "Gbps"} if is_list else {"items/s"}
        cols = [c for c in COLUMNS if c[0] not in drop]
        first = next(iter(by_label.values()))[0][1]
        print(f"## {op}\n")
        print(
            f"threads={first['threads']}, measured={fmt(first['measured_seconds'], 0)} s per run, "
            f"machine={first.get('gce_machine_type', '?')} ({first.get('available_processors')} vCPU), "
            f"zone={first.get('gce_zone', '?')}, bucket={first.get('bucket') or '-'}\n"
        )
        print("Median over rounds (min..max in brackets for the first column).\n")
        print("| config | rounds | errors | " + " | ".join(c[0] for c in cols) + " |")
        print("|---|--:|--:|" + "--:|" * len(cols))
        for label, items in by_label.items():
            cells = []
            for i, (_, path, d) in enumerate(cols):
                vals = [v for v in (get(s, *path) for _, s in items) if isinstance(v, (int, float))]
                if not vals:
                    cells.append("-")
                    continue
                cell = fmt(statistics.median(vals), d)
                if i == 0 and len(vals) > 1:
                    cell += f" ({fmt(min(vals), d)}..{fmt(max(vals), d)})"
                cells.append(cell)
            errors = sum(s["errors"] for _, s in items)
            print(f"| {label} | {len(items)} | {errors} | " + " | ".join(cells) + " |")
        print("\n<details><summary>Every run</summary>\n")
        print("| config | run | ops | errors | " + " | ".join(c[0] for c in cols) + " | first error |")
        print("|---|---|--:|--:|" + "--:|" * len(cols) + "---|")
        for label, items in by_label.items():
            for run, s in items:
                cells = [fmt(get(s, *path), d) for _, path, d in cols]
                err = (s.get("first_error") or "")[:80].replace("|", "/")
                print(
                    f"| {label} | {run} | {s['ops']:,} | {s['errors']} | "
                    + " | ".join(cells)
                    + f" | {err} |"
                )
        print("\n</details>\n")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
