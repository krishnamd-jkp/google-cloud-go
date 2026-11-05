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
"""Draws the rapidbench charts as SVG files (Python standard library only). Adapted from the Java
PoC's charts.py (java-storage/rust-poc-benchmark/scripts), so the two reports look the same.

Usage:
  python3 charts.py <results dir> <latency dir> <monitoring.csv> <output dir>

<results dir>     one sub-directory per run with summary.json and timeline.csv
<latency dir>     <run dir name>.csv files with "end_ms,latency_ms" of measured operations
<monitoring.csv>  "time,metric,value" rows for cpu_utilization_percent and memory_percent_used
"""

import csv
import datetime as dt
import glob
import json
import math
import os
import sys

GO = "#1f6fb5"
RUST = "#e0631c"
COLORS = {"go-default": GO, "rust-default": RUST}
NAMES = {"go-default": "Go", "rust-default": "Rust"}

VARIANTS = [
    ("OPEN", "1. Open (bidi), 100 MiB object"),
    ("OPEN_100KIB", "1. Open (bidi), 100 KiB object"),
    ("READ50X2X4_BIDI_CONCURRENT", "2. Read50x2x4 bidi, concurrent"),
    ("READ50X2X4_BIDI_SERIAL", "2. Read50x2x4 bidi, serialized"),
    ("READ50X2X4_JSON_CONCURRENT", "2. Read50x2x4 JSON, concurrent"),
    ("READ50X2X4_JSON_SERIAL", "2. Read50x2x4 JSON, serialized"),
    ("RANDOM8K_BIDI", "3. Random8K bidi"),
    ("RANDOM8K_JSON", "3. Random8K JSON"),
    ("BIDI_READ", "4. 16 MiB reads x48, bidi"),
    ("READ_JSON", "4. 16 MiB reads x48, JSON"),
    ("BIDI_APPENDABLE_WRITE", "5. 200 MB writes x48, bidi appendable"),
    ("WRITE_JSON", "5. 200 MB writes x48, JSON"),
    ("LIST_OBJECTS", "6. List Objects (gRPC)"),
    ("LIST_BUCKETS", "6. List Buckets (gRPC)"),
]
SHORT = {
    "OPEN": "1 Open 100MiB",
    "OPEN_100KIB": "1 Open 100KiB",
    "READ50X2X4_BIDI_CONCURRENT": "2 R50 bidi conc",
    "READ50X2X4_BIDI_SERIAL": "2 R50 bidi serial",
    "READ50X2X4_JSON_CONCURRENT": "2 R50 JSON conc",
    "READ50X2X4_JSON_SERIAL": "2 R50 JSON serial",
    "RANDOM8K_BIDI": "3 Random8K bidi",
    "RANDOM8K_JSON": "3 Random8K JSON",
    "BIDI_READ": "4 Read x48 bidi",
    "READ_JSON": "4 Read x48 JSON",
    "BIDI_APPENDABLE_WRITE": "5 Write x48 bidi",
    "WRITE_JSON": "5 Write x48 JSON",
    "LIST_OBJECTS": "6 List Objects",
    "LIST_BUCKETS": "6 List Buckets",
}
FONT = 'font-family="Helvetica,Arial,sans-serif"'


class Svg:
    def __init__(self, w, h):
        self.w, self.h, self.parts = w, h, []

    def add(self, s):
        self.parts.append(s)

    def text(self, x, y, s, size=12, anchor="start", color="#222", weight="normal", rotate=None):
        s = str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        tr = f' transform="rotate({rotate} {x:.1f} {y:.1f})"' if rotate else ""
        self.add(
            f'<text x="{x:.1f}" y="{y:.1f}" font-size="{size}" text-anchor="{anchor}" '
            f'fill="{color}" font-weight="{weight}" {FONT}{tr}>{s}</text>'
        )

    def line(self, x1, y1, x2, y2, color="#ccc", width=1, dash=None):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        self.add(
            f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" '
            f'stroke="{color}" stroke-width="{width}"{d}/>'
        )

    def rect(self, x, y, w, h, color, opacity=1.0, stroke=None):
        st = f' stroke="{stroke}"' if stroke else ""
        self.add(
            f'<rect x="{x:.1f}" y="{y:.1f}" width="{max(w, 0):.1f}" height="{max(h, 0):.1f}" '
            f'fill="{color}" fill-opacity="{opacity}"{st}/>'
        )

    def poly(self, pts, color, width=2):
        p = " ".join(f"{x:.1f},{y:.1f}" for x, y in pts)
        self.add(f'<polyline points="{p}" fill="none" stroke="{color}" stroke-width="{width}"/>')

    def save(self, path):
        with open(path, "w") as f:
            f.write(
                f'<svg xmlns="http://www.w3.org/2000/svg" width="{self.w}" height="{self.h}" '
                f'viewBox="0 0 {self.w} {self.h}">\n<rect width="100%" height="100%" fill="white"/>\n'
            )
            f.write("\n".join(self.parts))
            f.write("\n</svg>\n")


def legend(svg, x, y):
    for i, (label, color) in enumerate([("Go (current SDK)", GO), ("Rust (prototype)", RUST)]):
        svg.rect(x + i * 170, y - 10, 14, 14, color)
        svg.text(x + i * 170 + 20, y + 2, label, 12)


def nice_log_ticks(lo, hi):
    ticks = []
    e = math.floor(math.log10(lo))
    while 10**e <= hi * 1.0001:
        for m in (1, 2, 5):
            v = m * 10**e
            if lo * 0.999 <= v <= hi * 1.001:
                ticks.append(v)
        e += 1
    return ticks


def nice_step(hi, n=5):
    raw = hi / n
    mag = 10 ** math.floor(math.log10(raw))
    for m in (1, 2, 5, 10):
        if m * mag >= raw:
            return m * mag
    return 10 * mag


def fmt(v):
    if v >= 1000:
        return f"{v / 1000:g}k" if v < 1e6 else f"{v / 1e6:g}M"
    if v >= 1:
        return f"{v:g}"
    return f"{v:.2g}"


def load(results, latdir):
    runs = {}
    for d in sorted(glob.glob(os.path.join(results, "*", "summary.json"))):
        run = os.path.basename(os.path.dirname(d))
        s = json.load(open(d))
        lat = []
        p = os.path.join(latdir, run + ".csv")
        if os.path.exists(p):
            for row in csv.reader(open(p)):
                lat.append(float(row[1]))
        tl = []
        t = os.path.join(os.path.dirname(d), "timeline.csv")
        if os.path.exists(t):
            for row in csv.DictReader(open(t)):
                tl.append((int(row["second"]), int(row["ops"]), int(row["amount"])))
        runs[(s["op"], s["label"])] = {"s": s, "lat": sorted(lat), "tl": tl}
    return runs


def pct(sorted_vals, p):
    if not sorted_vals:
        return 0
    i = min(len(sorted_vals) - 1, max(0, int(math.ceil(p / 100 * len(sorted_vals))) - 1))
    return sorted_vals[i]


# ------------------------------------------------------------------------------------------
# Latency: distribution (CDF) per workload, and p50/p90/p99 bars
# ------------------------------------------------------------------------------------------


def latency_cdf(runs, out):
    cols, pw, ph = 3, 380, 230
    rows = math.ceil(len(VARIANTS) / cols)
    svg = Svg(cols * pw + 20, rows * ph + 70)
    svg.text(15, 25, "Latency distribution per workload (CDF, log scale; further left = faster)", 16, weight="bold")
    legend(svg, 15, 50)
    for i, (op, title) in enumerate(VARIANTS):
        ox, oy = 10 + (i % cols) * pw, 65 + (i // cols) * ph
        x0, x1, y0, y1 = ox + 50, ox + pw - 15, oy + 30, oy + ph - 40
        svg.text(ox + 10, oy + 18, title, 12, weight="bold")
        series = [(l, runs.get((op, l), {}).get("lat", [])) for l in ("go-default", "rust-default")]
        allv = [v for _, vs in series for v in vs if v > 0]
        if not allv:
            continue
        lo, hi = min(allv) * 0.9, max(allv) * 1.1
        lx = lambda v: x0 + (math.log10(v) - math.log10(lo)) / (math.log10(hi) - math.log10(lo)) * (x1 - x0)
        ly = lambda q: y1 - q * (y1 - y0)
        for q, name in ((0.5, "p50"), (0.9, "p90"), (0.99, "p99")):
            svg.line(x0, ly(q), x1, ly(q), "#e4e4e4", dash="3,3")
            svg.text(x0 - 5, ly(q) + (6 if name == "p90" else 3), name, 9, "end", "#666")
        for t in nice_log_ticks(lo, hi):
            svg.line(lx(t), y0, lx(t), y1, "#f2f2f2")
            svg.text(lx(t), y1 + 13, fmt(t), 9, "middle", "#666")
        svg.text((x0 + x1) / 2, y1 + 28, "latency (ms)", 10, "middle", "#444")
        svg.line(x0, y1, x1, y1, "#999")
        svg.line(x0, y0, x0, y1, "#999")
        for label, vs in series:
            if not vs:
                continue
            n = len(vs)
            step = max(1, n // 400)
            pts = [(lx(max(vs[k], lo)), ly((k + 1) / n)) for k in range(0, n, step)]
            pts.append((lx(vs[-1]), ly(1.0)))
            svg.poly(pts, COLORS[label], 2)
        nj = len(series[0][1])
        nr = len(series[1][1])
        svg.text(x1, y0 - 4, f"n = {nj:,} / {nr:,}", 9, "end", "#666")
    svg.save(out)


def latency_bars(runs, out):
    rowh, left, width = 58, 190, 640
    svg = Svg(left + width + 220, 110 + rowh * len(VARIANTS))
    svg.text(15, 25, "Latency p50 / p90 / p99 per workload (log scale; shorter = faster)", 16, weight="bold")
    legend(svg, 15, 50)
    svg.text(left + width - 260, 50, "bar = p50,  thin = p90,  tick = p99", 11, color="#444")
    vals = [runs[k]["s"][f"latency_ms_{p}"] for k in runs for p in ("p50", "p90", "p99") if runs[k]["s"][f"latency_ms_{p}"] > 0]
    lo, hi = min(vals) * 0.8, max(vals) * 1.25
    lx = lambda v: left + (math.log10(max(v, lo)) - math.log10(lo)) / (math.log10(hi) - math.log10(lo)) * width
    top = 70
    for t in nice_log_ticks(lo, hi):
        if str(t)[0] in "15":
            svg.line(lx(t), top, lx(t), top + rowh * len(VARIANTS), "#eee")
            svg.text(lx(t), top + rowh * len(VARIANTS) + 14, fmt(t), 10, "middle", "#666")
    svg.text(left + width / 2, top + rowh * len(VARIANTS) + 30, "latency (ms)", 11, "middle", "#444")
    for i, (op, _) in enumerate(VARIANTS):
        y = top + i * rowh
        svg.text(left - 10, y + rowh / 2 + 4, SHORT[op], 11, "end")
        for j, label in enumerate(("go-default", "rust-default")):
            r = runs.get((op, label))
            if not r:
                continue
            s = r["s"]
            by = y + 10 + j * 20
            c = COLORS[label]
            svg.rect(left, by, lx(s["latency_ms_p50"]) - left, 14, c, 0.9)
            svg.line(lx(s["latency_ms_p50"]), by + 7, lx(s["latency_ms_p90"]), by + 7, c, 2)
            svg.line(lx(s["latency_ms_p99"]), by + 1, lx(s["latency_ms_p99"]), by + 13, c, 2)
            svg.text(
                lx(s["latency_ms_p99"]) + 5,
                by + 11,
                f'{s["latency_ms_p50"]:,.1f} / {s["latency_ms_p90"]:,.1f} / {s["latency_ms_p99"]:,.1f}',
                9,
                color="#444",
            )
    svg.save(out)


# ------------------------------------------------------------------------------------------
# Throughput: overview bars and over time
# ------------------------------------------------------------------------------------------


def hbar_chart(runs, out, title, ops, metric, unit, log=True):
    rowh, left, width = 46, 190, 600
    svg = Svg(left + width + 120, 115 + rowh * len(ops))
    svg.text(15, 25, title, 16, weight="bold")
    legend(svg, 15, 50)
    vals = [metric(runs[(op, l)]["s"]) for op in ops for l in ("go-default", "rust-default") if (op, l) in runs]
    vals = [v for v in vals if v is not None and v > 0]
    top = 70
    if log:
        lo, hi = min(vals) * 0.5, max(vals) * 1.3
        sx = lambda v: left + (math.log10(max(v, lo)) - math.log10(lo)) / (math.log10(hi) - math.log10(lo)) * width
        ticks = [t for t in nice_log_ticks(lo, hi) if str(t)[0] in "15"]
    else:
        lo, hi = 0, max(vals) * 1.15
        sx = lambda v: left + v / hi * width
        step = nice_step(hi)
        ticks = [k * step for k in range(0, int(hi / step) + 1)]
    for t in ticks:
        svg.line(sx(t), top, sx(t), top + rowh * len(ops), "#eee")
        svg.text(sx(t), top + rowh * len(ops) + 14, fmt(t), 10, "middle", "#666")
    svg.text(left + width / 2, top + rowh * len(ops) + 30, unit, 11, "middle", "#444")
    for i, op in enumerate(ops):
        y = top + i * rowh
        svg.text(left - 10, y + rowh / 2 + 4, SHORT[op], 11, "end")
        for j, label in enumerate(("go-default", "rust-default")):
            r = runs.get((op, label))
            if not r:
                continue
            v = metric(r["s"])
            if v is None:
                continue
            by = y + 6 + j * 17
            svg.rect(left, by, sx(v) - left if v > 0 else 0, 14, COLORS[label], 0.9)
            svg.text(sx(v) + 5 if v > 0 else left + 5, by + 11, f"{v:,.2f}" if v < 10 else f"{v:,.1f}", 10, color="#333")
    svg.save(out)


def throughput_time(runs, out):
    panels = [("BIDI_READ", "4. 16 MiB reads x48, bidi"), ("READ_JSON", "4. 16 MiB reads x48, JSON"),
              ("BIDI_APPENDABLE_WRITE", "5. 200 MB writes x48, bidi appendable"), ("WRITE_JSON", "5. 200 MB writes x48, JSON")]
    pw, ph = 560, 250
    svg = Svg(2 * pw + 20, 2 * ph + 70)
    svg.text(15, 25, "Throughput over the 5-minute measured window (MB/s, 10 s moving average)", 16, weight="bold")
    legend(svg, 15, 50)
    for i, (op, title) in enumerate(panels):
        ox, oy = 10 + (i % 2) * pw, 65 + (i // 2) * ph
        x0, x1, y0, y1 = ox + 60, ox + pw - 15, oy + 30, oy + ph - 40
        svg.text(ox + 10, oy + 18, title, 12, weight="bold")
        series = []
        for label in ("go-default", "rust-default"):
            r = runs.get((op, label))
            if not r:
                continue
            warm = 30
            dur = int(round(r["s"]["measured_seconds"]))
            per = {sec: amt for sec, _, amt in r["tl"]}
            xs = list(range(warm, warm + dur))
            raw = [per.get(sec, 0) / 1e6 for sec in xs]
            sm = []
            for k in range(len(raw)):
                w = raw[max(0, k - 9): k + 1]
                sm.append(sum(w) / len(w))
            series.append((label, sm))
        hi = max(max(v) for _, v in series) * 1.15
        sx = lambda k: x0 + k / 300 * (x1 - x0)
        sy = lambda v: y1 - v / hi * (y1 - y0)
        step = nice_step(hi, 4)
        for t in [k * step for k in range(0, int(hi / step) + 1)]:
            svg.line(x0, sy(t), x1, sy(t), "#eee")
            svg.text(x0 - 5, sy(t) + 4, fmt(t), 9, "end", "#666")
        for t in range(0, 301, 60):
            svg.text(sx(t), y1 + 13, f"{t}s", 9, "middle", "#666")
        svg.line(x0, y1, x1, y1, "#999")
        svg.line(x0, y0, x0, y1, "#999")
        for label, sm in series:
            svg.poly([(sx(k), sy(v)) for k, v in enumerate(sm)], COLORS[label], 2)
        if op in ("BIDI_APPENDABLE_WRITE", "WRITE_JSON"):
            svg.text(x1, y0 - 4, "per-second bytes of completed 200 MB objects", 9, "end", "#666")
    svg.save(out)


# ------------------------------------------------------------------------------------------
# CPU and memory: per-run bars and the whole-run timeline from Cloud Monitoring
# ------------------------------------------------------------------------------------------


def vm_bars(runs, out, key, title, unit):
    rowh, left, width = 46, 190, 560
    ops = [op for op, _ in VARIANTS]
    svg = Svg(left + width + 170, 125 + rowh * len(ops))
    svg.text(15, 25, title, 16, weight="bold")
    legend(svg, 15, 50)
    svg.text(380, 50, "bar = mean in the window, tick = peak" if key.startswith("cpu") else "bar = peak in the window", 11, color="#444")
    vals = []
    for k in runs:
        cm = runs[k]["s"].get("cloud_monitoring", {}).get(key, {})
        if cm.get("peak") is not None:
            vals.append(cm["peak"])
    hi = max(vals) * 1.15
    top = 75
    sx = lambda v: left + v / hi * width
    step = nice_step(hi, 6)
    for t in [k * step for k in range(0, int(hi / step) + 1)]:
        svg.line(sx(t), top, sx(t), top + rowh * len(ops), "#eee")
        svg.text(sx(t), top + rowh * len(ops) + 14, f"{t:g}", 10, "middle", "#666")
    svg.text(left + width / 2, top + rowh * len(ops) + 30, unit, 11, "middle", "#444")
    for i, op in enumerate(ops):
        y = top + i * rowh
        svg.text(left - 10, y + rowh / 2 + 4, SHORT[op], 11, "end")
        for j, label in enumerate(("go-default", "rust-default")):
            r = runs.get((op, label))
            if not r:
                continue
            cm = r["s"].get("cloud_monitoring", {}).get(key, {})
            by = y + 6 + j * 17
            c = COLORS[label]
            if key.startswith("cpu"):
                svg.rect(left, by, sx(cm["mean"]) - left, 14, c, 0.9)
                svg.line(sx(cm["peak"]), by, sx(cm["peak"]), by + 14, c, 2)
                txt = f'{cm["mean"]:.2f} (peak {cm["peak"]:.2f})'
                end = sx(cm["peak"])
            else:
                svg.rect(left, by, sx(cm["peak"]) - left, 14, c, 0.9)
                txt = f'{cm["peak"]:.2f}%  (~{cm["peak"] * 7.2:.1f} GB)'
                end = sx(cm["peak"])
            svg.text(end + 5, by + 11, txt, 10, color="#333")
    svg.save(out)


def vm_timeline(runs, monitoring, out):
    pts = {"cpu_utilization_percent": [], "memory_percent_used": []}
    for row in csv.DictReader(open(monitoring)):
        t = dt.datetime.fromisoformat(row["time"].replace("Z", "+00:00"))
        pts[row["metric"]].append((t, float(row["value"])))
    for k in pts:
        pts[k].sort()
    windows = []
    for (op, label), r in runs.items():
        s = r["s"]
        a = dt.datetime.fromisoformat(s["window_start"].replace("Z", "+00:00"))
        b = dt.datetime.fromisoformat(s["window_end"].replace("Z", "+00:00"))
        windows.append((a, b, op, label))
    t0 = min(w[0] for w in windows) - dt.timedelta(minutes=2)
    t1 = max(w[1] for w in windows) + dt.timedelta(minutes=2)
    W, ph = 1300, 230
    svg = Svg(W, 2 * ph + 120)
    svg.text(15, 25, "Whole run from Cloud Monitoring (1 sample per minute); shaded = measured window of each run", 16, weight="bold")
    legend(svg, 15, 50)
    for i, (key, title, unit) in enumerate([
        ("cpu_utilization_percent", "VM CPU utilization (compute.googleapis.com/instance/cpu/utilization)", "%"),
        ("memory_percent_used", "VM memory used (agent.googleapis.com/memory/percent_used)", "%"),
    ]):
        oy = 70 + i * (ph + 20)
        x0, x1, y0, y1 = 60, W - 20, oy + 25, oy + ph - 30
        svg.text(15, oy + 15, title, 12, weight="bold")
        vals = [v for t, v in pts[key] if t0 <= t <= t1]
        hi = max(vals) * 1.15
        span = (t1 - t0).total_seconds()
        sx = lambda t: x0 + (t - t0).total_seconds() / span * (x1 - x0)
        sy = lambda v: y1 - v / hi * (y1 - y0)
        for a, b, op, label in windows:
            svg.rect(sx(a), y0, sx(b) - sx(a), y1 - y0, COLORS[label], 0.12)
            if i == 0:
                svg.text((sx(a) + sx(b)) / 2, y0 + 10, SHORT[op].split(" ")[0], 8, "middle", "#555")
        step = nice_step(hi, 4)
        for t in [k * step for k in range(0, int(hi / step) + 1)]:
            svg.line(x0, sy(t), x1, sy(t), "#eee")
            svg.text(x0 - 5, sy(t) + 4, f"{t:g}{unit}", 9, "end", "#666")
        tick = t0.replace(minute=(t0.minute // 15) * 15, second=0, microsecond=0)
        while tick <= t1:
            if tick >= t0:
                svg.line(sx(tick), y1, sx(tick), y1 + 4, "#999")
                svg.text(sx(tick), y1 + 15, tick.strftime("%H:%M"), 9, "middle", "#666")
            tick += dt.timedelta(minutes=15)
        svg.line(x0, y1, x1, y1, "#999")
        svg.line(x0, y0, x0, y1, "#999")
        svg.poly([(sx(t), sy(v)) for t, v in pts[key] if t0 <= t <= t1], "#333", 1.5)
    svg.text(x1, 2 * ph + 110, "time (UTC)", 10, "end", "#444")
    svg.save(out)


def main(results, latdir, monitoring, outdir):
    os.makedirs(outdir, exist_ok=True)
    runs = load(results, latdir)
    latency_cdf(runs, os.path.join(outdir, "latency_distribution.svg"))
    latency_bars(runs, os.path.join(outdir, "latency_percentiles.svg"))
    data_ops = ["READ50X2X4_BIDI_CONCURRENT", "READ50X2X4_BIDI_SERIAL", "READ50X2X4_JSON_CONCURRENT",
                "READ50X2X4_JSON_SERIAL", "RANDOM8K_BIDI", "RANDOM8K_JSON", "BIDI_READ", "READ_JSON",
                "BIDI_APPENDABLE_WRITE", "WRITE_JSON"]
    hbar_chart(runs, os.path.join(outdir, "throughput_mbps.svg"),
               "Throughput, MB/s (log scale; longer = more data moved)", data_ops,
               lambda s: s.get("throughput_MBps"), "MB/s (log scale)")
    hbar_chart(runs, os.path.join(outdir, "throughput_ops.svg"),
               "Operations per second (longer = more operations)", [op for op, _ in VARIANTS],
               lambda s: s.get("ops_per_sec"), "operations per second (log scale)")
    throughput_time(runs, os.path.join(outdir, "throughput_over_time.svg"))
    vm_bars(runs, os.path.join(outdir, "cpu_utilization.svg"), "cpu_utilization_percent",
            "VM CPU utilization per run (compute.googleapis.com/instance/cpu/utilization)", "% of the VM's 192 vCPUs")
    vm_bars(runs, os.path.join(outdir, "memory_peak.svg"), "memory_percent_used",
            "VM peak memory per run (agent.googleapis.com/memory/percent_used)", "% of the VM's 720 GB")
    vm_timeline(runs, monitoring, os.path.join(outdir, "cpu_memory_timeline.svg"))
    print("charts in", outdir)


if __name__ == "__main__":
    if len(sys.argv) != 5:
        sys.exit(__doc__)
    main(*sys.argv[1:])
