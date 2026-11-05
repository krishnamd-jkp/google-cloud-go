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
"""Extracts the measured operations of every rapidbench run for the latency charts.

Usage:
  python3 latencies.py <results dir> <latency dir>

Writes <latency dir>/<run dir name>.csv with "end_ms,latency_ms" rows (no header) of the operations
that ended inside the measured window, from each run's ops.csv. Standard library only.
"""

import csv
import glob
import os
import sys


def main(results, latdir):
    os.makedirs(latdir, exist_ok=True)
    for path in sorted(glob.glob(os.path.join(results, "*", "ops.csv"))):
        run = os.path.basename(os.path.dirname(path))
        n = 0
        with open(path) as src, open(os.path.join(latdir, run + ".csv"), "w", newline="") as dst:
            w = csv.writer(dst)
            for row in csv.DictReader(src):
                if row["measured"] == "true":
                    w.writerow([row["end_ms"], row["latency_ms"]])
                    n += 1
        print(f"{run}: {n} measured operations")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(*sys.argv[1:])
