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
"""Copies a rapidbench pass into a directory to publish, without environment-specific names.

Usage:
  python3 publish.py <results dir> <publish dir> NAME=REPLACEMENT [NAME=REPLACEMENT ...]

Copies every run's summary.json and timeline.csv, run_all.log and summary.md. In summary.json it
drops the VM's project, instance id and instance name; in every file it replaces each NAME (e.g. a
bucket or project name) with REPLACEMENT. ops.csv, the latency files and monitoring.csv are not
copied: the charts made from them are. Standard library only.
"""

import glob
import json
import os
import shutil
import sys

DROP = ("gce_project", "gce_instance_id", "gce_instance_name")


def scrub(text, repl):
    for name, new in repl:
        text = text.replace(name, new)
    return text


def main(src, dst, pairs):
    repl = [tuple(p.split("=", 1)) for p in pairs]
    os.makedirs(dst, exist_ok=True)
    for path in sorted(glob.glob(os.path.join(src, "*", "summary.json"))):
        run = os.path.basename(os.path.dirname(path))
        os.makedirs(os.path.join(dst, run), exist_ok=True)
        s = json.load(open(path))
        for k in DROP:
            s.pop(k, None)
        with open(os.path.join(dst, run, "summary.json"), "w") as f:
            f.write(scrub(json.dumps(s, indent=2), repl) + "\n")
        shutil.copy(os.path.join(os.path.dirname(path), "timeline.csv"), os.path.join(dst, run, "timeline.csv"))
    for name in ("run_all.log", "summary.md"):
        p = os.path.join(src, name)
        if os.path.exists(p):
            with open(p) as f, open(os.path.join(dst, name), "w") as g:
                g.write(scrub(f.read(), repl))
    charts = os.path.join(src, "charts")
    if os.path.isdir(charts):
        shutil.copytree(charts, os.path.join(dst, "charts"), dirs_exist_ok=True)
    leftovers = []
    for root, _, files in os.walk(dst):
        for fn in files:
            text = open(os.path.join(root, fn), errors="ignore").read()
            leftovers += [f"{fn}: {name}" for name, _ in repl if name in text]
    if leftovers:
        sys.exit("names left in the published files:\n" + "\n".join(leftovers))
    print(f"published {src} -> {dst}")


if __name__ == "__main__":
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2], sys.argv[3:])
