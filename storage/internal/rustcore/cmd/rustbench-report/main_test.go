// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic results (no benchmark run): the report aggregates rounds and compares backends.
const synthetic = `{"schema":1,"run_id":"r1","round":1,"time":"2026-11-03T10:00:00Z","backend":"go-json","engine":"go","transport":"json","case":{"name":"read-16MiB-c8","workload":"read","size":16777216,"concurrency":8,"ops":8},"ops":64,"bytes":1073741824,"mib_per_s":400,"ops_per_s":25,"latency_ms":{"p50":300,"p99":500},"ttfb_ms":{"p50":40},"cpu_s_per_gib":1.0,"cpu_ms_per_op":16,"rss_baseline_mib":100,"rss_peak_mib":150,"threads_max":20,"env":{"machine_type":"n2-standard-16","zone":"us-central1-a","num_cpu":16,"gomaxprocs":16,"go_version":"go1.26","directpath_disabled":true,"bucket":"b","profile":"default"}}
{"schema":1,"run_id":"r1","round":2,"time":"2026-11-03T10:10:00Z","backend":"go-json","engine":"go","transport":"json","case":{"name":"read-16MiB-c8","workload":"read","size":16777216,"concurrency":8,"ops":8},"ops":64,"bytes":1073741824,"mib_per_s":420,"ops_per_s":26,"latency_ms":{"p50":310,"p99":520},"ttfb_ms":{"p50":42},"cpu_s_per_gib":1.2,"cpu_ms_per_op":17,"rss_baseline_mib":100,"rss_peak_mib":152,"threads_max":21}
{"schema":1,"run_id":"r1","round":1,"time":"2026-11-03T10:01:00Z","backend":"rust-json","engine":"rust","transport":"json","case":{"name":"read-16MiB-c8","workload":"read","size":16777216,"concurrency":8,"ops":8},"ops":64,"bytes":1073741824,"mib_per_s":800,"ops_per_s":50,"latency_ms":{"p50":150,"p99":250},"ttfb_ms":{"p50":30},"cpu_s_per_gib":0.5,"cpu_ms_per_op":8,"rss_baseline_mib":100,"rss_peak_mib":180,"threads_max":40,"ffi_calls":1280}
{"schema":1,"run_id":"r1","round":2,"time":"2026-11-03T10:11:00Z","backend":"rust-json","engine":"rust","transport":"json","case":{"name":"read-16MiB-c8","workload":"read","size":16777216,"concurrency":8,"ops":8},"failed":true,"error":"child process: exit status 1"}
{"schema":1,"run_id":"r0","round":1,"time":"2026-11-02T10:00:00Z","backend":"go-json","engine":"go","transport":"json","case":{"name":"old","workload":"read"},"ops":1,"mib_per_s":1}
`

func TestReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(path, []byte(synthetic), 0o644); err != nil {
		t.Fatal(err)
	}
	all, err := readResults(path)
	if err != nil {
		t.Fatal(err)
	}
	var rs []result
	for _, r := range all {
		if r.RunID == "r1" {
			rs = append(rs, r)
		}
	}
	var out bytes.Buffer
	writeReport(&out, "r1", rs, nil, nil)
	got := out.String()
	for _, want := range []string{
		"`n2-standard-16` in `us-central1-a`",
		"CloudPath (DirectPath disabled for every client)",
		"| read-16MiB-c8 | rust-json vs go-json (Go default HTTP transport (HTTP/2)) | 1.95x |", // 800 / median(400, 420)
		"| go-json | 2/2 | 410.0 (400.0–420.0) |",
		"| rust-json | 1/2 | 800.0 (800.0–800.0) |",
		"1 failed rounds",
		"child process: exit status 1",
		"| 20.0 |", // rust-json FFI calls per op: 1280 / 64
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "### old") {
		t.Error("report includes another run's results")
	}
}

// Timed phases joined with the VM's 1-minute CPU utilization samples.
const syntheticTimed = `{"schema":1,"run_id":"t1","round":1,"time":"2026-11-03T10:05:00Z","measure_start":"2026-11-03T10:05:30Z","measure_end":"2026-11-03T10:09:30Z","backend":"go-grpc-bidi","engine":"go","transport":"grpc","case":{"name":"open","workload":"open","size":102400,"concurrency":1,"duration_s":240},"ops":2400,"wall_s":240,"ops_per_s":10,"latency_ms":{"p50":90,"p90":100,"p99":150,"mean":95},"cpu_user_s":2.0,"cpu_sys_s":0.4,"cpu_ms_per_op":1,"rss_baseline_mib":10,"rss_peak_mib":40,"env":{"num_cpu":192,"directpath_disabled":true}}
{"schema":1,"run_id":"t1","round":1,"time":"2026-11-03T10:10:30Z","measure_start":"2026-11-03T10:11:00Z","measure_end":"2026-11-03T10:15:00Z","backend":"rust-grpc","engine":"rust","transport":"grpc","case":{"name":"open","workload":"open","size":102400,"concurrency":1,"duration_s":240},"ops":1200,"wall_s":240,"ops_per_s":5,"latency_ms":{"p50":180,"p90":200,"p99":300,"mean":190},"cpu_user_s":4.0,"cpu_sys_s":0.8,"cpu_ms_per_op":4,"rss_baseline_mib":10,"rss_peak_mib":60,"ffi_calls":3600}
`

func TestReportVMCPU(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.jsonl")
	if err := os.WriteFile(path, []byte(syntheticTimed), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := readResults(path)
	if err != nil {
		t.Fatal(err)
	}
	// One sample per minute from 10:00 to 10:20: idle 0.05 %, 0.2 % during the Go phase, 0.3 %
	// during the Rust phase. The samples ending at 10:06 and 10:10 straddle the Go phase's edges.
	var pts []string
	for m := 0; m <= 20; m++ {
		v := 0.0005
		switch {
		case m >= 6 && m <= 10:
			v = 0.002
		case m >= 11 && m <= 15:
			v = 0.003
		}
		pts = append(pts, fmt.Sprintf(`{"interval":{"startTime":"2026-11-03T10:%02d:00Z","endTime":"2026-11-03T10:%02d:00Z"},"value":{"doubleValue":%g}}`, m, m, v))
	}
	cpuPath := filepath.Join(dir, "cpu.json")
	js := `{"timeSeries":[{"metric":{"type":"compute.googleapis.com/instance/cpu/utilization"},"points":[` + strings.Join(pts, ",") + `]}]}`
	if err := os.WriteFile(cpuPath, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	samples, err := readVMCPU(cpuPath)
	if err != nil {
		t.Fatal(err)
	}
	if u, n := meanVMCPU(samples, rs[0].MeasureStart, rs[0].MeasureEnd); n != 3 || math.Abs(u-0.002) > 1e-9 {
		t.Errorf("Go phase: mean %v over %d samples, want 0.002 over 3 (10:07, 10:08, 10:09)", u, n)
	}
	var out bytes.Buffer
	writeReport(&out, "t1", rs, samples, nil)
	got := out.String()
	for _, want := range []string{
		"| open | rust-grpc vs go-grpc-bidi (same RPC (BidiReadObject)) | 0.50x |", // ops/s: 5 / 10
		"| 2.00x | 4.00x | +20 MiB | +0.100 pp |",                                  // p99, CPU per op, memory, VM CPU
		"Idle VM (samples before and after the run): 0.050 % (9 samples)",
		"| open | go-grpc-bidi | 0.200 | 0.288 | 0.010 | 3 |", // (0.2 - 0.05) % of 192 vCPUs
		"| open | rust-grpc | 0.300 | 0.480 | 0.020 | 4 |",
		"| go-grpc-bidi | 1/1 | 10.0 (10.0–10.0) | 10.0 | 90.0 | 100.0 | 150.0 | 95.0 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}
