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

// Command rustbench-report summarizes rustbench results (JSON lines) as a Markdown report:
// per case, the median over rounds of every metric for every backend, the spread between
// rounds, and the Rust core compared with the Go SDK on the same transport.
//
//	rustbench-report [-run RUN_ID] [-vm-cpu cpu.json] results.jsonl [more.jsonl ...] > report.md
//
// Without -run, the most recent run in the input is reported. -vm-cpu adds the VM's CPU
// utilization during every measured phase: a Cloud Monitoring timeSeries.list response for
// compute.googleapis.com/instance/cpu/utilization of the VM that ran the suite, covering the run
// and a few idle minutes before it (see ../../bench/README.md).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// result mirrors the fields of rustbench's result lines that the report uses.
type result struct {
	Schema    int       `json:"schema"`
	RunID     string    `json:"run_id"`
	Round     int       `json:"round"`
	Time      time.Time `json:"time"`
	Backend   string    `json:"backend"`
	Engine    string    `json:"engine"`
	Transport string    `json:"transport"`
	Case      struct {
		Name        string `json:"name"`
		Workload    string `json:"workload"`
		Size        int64  `json:"size"`
		RangeSize   int64  `json:"range_size"`
		Concurrency int    `json:"concurrency"`
		Ops         int    `json:"ops"`
		IOBuf       int    `json:"io_buf"`
		Objects     int    `json:"objects"`
		PageSize    int    `json:"page_size"`
		Delimiter   string `json:"delimiter"`
		Desc        string `json:"desc"`
		Sources     int    `json:"sources"`
		Ranges      int    `json:"ranges"`
		Stride      int64  `json:"stride"`
		Parallel    bool   `json:"parallel"`
		CopyIO      bool   `json:"copy_io"`
		DurationS   int    `json:"duration_s"`
		ConnPool    int    `json:"conn_pool"`
		FirstPage   bool   `json:"first_page_only"`
	} `json:"case"`
	Failed       bool      `json:"failed"`
	Error        string    `json:"error"`
	ClientInitMs float64   `json:"client_init_ms"`
	Ops          int       `json:"ops"`
	Errors       int       `json:"errors"`
	FirstError   string    `json:"first_error"`
	Bytes        int64     `json:"bytes"`
	Entries      int64     `json:"entries"`
	WallS        float64   `json:"wall_s"`
	MiBPerS      float64   `json:"mib_per_s"`
	OpsPerS      float64   `json:"ops_per_s"`
	EntriesPerS  float64   `json:"entries_per_s"`
	LatencyMs    stats     `json:"latency_ms"`
	TTFBMs       *stats    `json:"ttfb_ms"`
	MeasureStart time.Time `json:"measure_start"`
	MeasureEnd   time.Time `json:"measure_end"`
	CPUUserS     float64   `json:"cpu_user_s"`
	CPUSysS      float64   `json:"cpu_sys_s"`
	CPUSPerGiB   float64   `json:"cpu_s_per_gib"`
	CPUMsPerOp   float64   `json:"cpu_ms_per_op"`
	RSSBaseMiB   float64   `json:"rss_baseline_mib"`
	RSSPeakMiB   float64   `json:"rss_peak_mib"`
	ThreadsMax   int64     `json:"threads_max"`
	GoGCs        uint32    `json:"go_gc_cycles"`
	FFICalls     int64     `json:"ffi_calls"`
	Env          *struct {
		Host               string `json:"host"`
		MachineType        string `json:"machine_type"`
		Zone               string `json:"zone"`
		InstanceID         string `json:"instance_id"`
		Kernel             string `json:"kernel"`
		NumCPU             int    `json:"num_cpu"`
		GOMAXPROCS         int    `json:"gomaxprocs"`
		GoVersion          string `json:"go_version"`
		Revision           string `json:"revision"`
		Modified           bool   `json:"modified"`
		Source             string `json:"source"`
		DirectPathDisabled bool   `json:"directpath_disabled"`
		Bucket             string `json:"bucket"`
		ZonalBucket        string `json:"zonal_bucket"`
		Profile            string `json:"profile"`
		Seed               int64  `json:"seed"`
		MemTotalMiB        int64  `json:"mem_total_mib"`
	} `json:"env"`
}

type stats struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

// backendOrder is the row order; unknown backends follow.
var backendOrder = []string{"go-json", "go-json-h1", "go-xml", "go-grpc", "go-grpc-bidi", "rust-json", "rust-grpc"}

// comparisons: each Rust backend against the Go configurations on the same transport.
var comparisons = []struct{ rust, goName, note string }{
	{"rust-json", "go-json", "Go default HTTP transport (HTTP/2)"},
	{"rust-json", "go-json-h1", "same protocol (HTTP/1.1)"},
	{"rust-json", "go-xml", "Go default read path (XML API)"},
	{"rust-grpc", "go-grpc", "Go gRPC client; reads with ReadObject"},
	{"rust-grpc", "go-grpc-bidi", "same RPC (BidiReadObject)"},
}

var workloadOrder = []string{"open", "strided", "range", "chunked", "read", "write", "append", "list", "list-buckets"}

func main() {
	log.SetFlags(0)
	runID := flag.String("run", "", "run id to report (default: the most recent run in the input)")
	vmCPU := flag.String("vm-cpu", "", "Cloud Monitoring timeSeries.list response (JSON) for compute.googleapis.com/instance/cpu/utilization of the VM")
	vmMem := flag.String("vm-mem", "", `Cloud Monitoring timeSeries.list response (JSON) for agent.googleapis.com/memory/percent_used (state "used") of the VM`)
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rustbench-report [-run RUN_ID] [-vm-cpu cpu.json] [-vm-mem mem.json] results.jsonl [more.jsonl ...] > report.md")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	var all []result
	for _, path := range flag.Args() {
		rs, err := readResults(path)
		if err != nil {
			log.Fatal(err)
		}
		all = append(all, rs...)
	}
	if len(all) == 0 {
		log.Fatal("no results in the input")
	}
	if *runID == "" {
		latest := all[0]
		for _, r := range all {
			if r.Time.After(latest.Time) {
				latest = r
			}
		}
		*runID = latest.RunID
	}
	var rs []result
	for _, r := range all {
		if r.RunID == *runID {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		log.Fatalf("no results for run %q", *runID)
	}
	var samples, memSamples []vmSample
	if *vmCPU != "" {
		var err error
		if samples, err = readVMCPU(*vmCPU); err != nil {
			log.Fatal(err)
		}
	}
	if *vmMem != "" {
		var err error
		if memSamples, err = readVMCPU(*vmMem); err != nil {
			log.Fatal(err)
		}
	}
	writeReport(os.Stdout, *runID, rs, samples, memSamples)
}

// vmSample is one point of a VM metric: for compute.googleapis.com/instance/cpu/utilization the
// mean utilization of all the VM's vCPUs (0..1) over the minute that ended at end; for
// agent.googleapis.com/memory/percent_used the memory in use (0..100) at end.
type vmSample struct {
	end  time.Time
	util float64
}

// readVMCPU reads a Cloud Monitoring timeSeries.list response with exactly one time series.
func readVMCPU(path string) ([]vmSample, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var resp struct {
		TimeSeries []struct {
			Points []struct {
				Interval struct {
					EndTime time.Time `json:"endTime"`
				} `json:"interval"`
				Value struct {
					DoubleValue float64 `json:"doubleValue"`
				} `json:"value"`
			} `json:"points"`
		} `json:"timeSeries"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(resp.TimeSeries) != 1 {
		return nil, fmt.Errorf("%s: %d time series, want 1 (filter on the VM's instance_id)", path, len(resp.TimeSeries))
	}
	var out []vmSample
	for _, p := range resp.TimeSeries[0].Points {
		out = append(out, vmSample{end: p.Interval.EndTime, util: p.Value.DoubleValue})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].end.Before(out[j].end) })
	return out, nil
}

// meanVMCPU returns the mean utilization of the samples whose minute lies entirely within
// [from, to], and how many there are.
func meanVMCPU(samples []vmSample, from, to time.Time) (float64, int) {
	var sum float64
	n := 0
	for _, s := range samples {
		if !s.end.Add(-time.Minute).Before(from) && !s.end.After(to) {
			sum += s.util
			n++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return sum / float64(n), n
}

// idleVMCPU returns the mean utilization of the samples entirely before the run's first child
// process (with a margin) or after its last measured phase.
func idleVMCPU(samples []vmSample, rs []result) (float64, int) {
	first, last := rs[0].Time, rs[0].MeasureEnd
	for _, r := range rs {
		if r.Time.Before(first) {
			first = r.Time
		}
		if r.MeasureEnd.After(last) {
			last = r.MeasureEnd
		}
	}
	first, last = first.Add(-30*time.Second), last.Add(30*time.Second)
	var sum float64
	n := 0
	for _, s := range samples {
		if !s.end.After(first) || !s.end.Add(-time.Minute).Before(last) {
			sum += s.util
			n++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return sum / float64(n), n
}

// peakVMMem returns the highest memory sample taken within [from, to], and how many there are.
func peakVMMem(samples []vmSample, from, to time.Time) (float64, int) {
	peak, n := math.NaN(), 0
	for _, s := range samples {
		if !s.end.Before(from) && !s.end.After(to) {
			if n == 0 || s.util > peak {
				peak = s.util
			}
			n++
		}
	}
	return peak, n
}

func readResults(path string) ([]result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []result
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// agg is one (case, backend) over all rounds.
type agg struct {
	backend, transport, engine string
	rounds, failed, errors     int
	firstError                 string
	thr, opsPerS               []float64 // MiB/s (entries/s for list, ops/s for open)
	p50, p90, p99, mean, ttfb  []float64
	cpuGiB, cpuMsPerOp         []float64 // process CPU per GiB moved, per op
	cores                      []float64 // process CPU time / wall time of the measured phase
	mem, threads, initMs       []float64
	ffiPerOp                   []float64
	vmCPU                      []float64 // VM utilization (%) during the measured phase (-vm-cpu)
	vmSamples                  int
	vmMem                      []float64 // VM memory in use (%), peak during the measured phase (-vm-mem)
	vmMemSamples               int
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := slices.Clone(v)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func minMax(v []float64) (float64, float64) {
	if len(v) == 0 {
		return math.NaN(), math.NaN()
	}
	return slices.Min(v), slices.Max(v)
}

type caseGroup struct {
	name, workload, desc string
	byteBased            bool // throughput in MiB/s (else entries/s or ops/s)
	isList               bool
	aggs                 map[string]*agg
}

func group(rs []result, samples, memSamples []vmSample) []*caseGroup {
	groups := map[string]*caseGroup{}
	for _, r := range rs {
		g := groups[r.Case.Name]
		if g == nil {
			isList := r.Case.Workload == "list" || r.Case.Workload == "list-buckets"
			g = &caseGroup{name: r.Case.Name, workload: r.Case.Workload, desc: describe(r), aggs: map[string]*agg{},
				byteBased: !isList && r.Case.Workload != "open", isList: isList}
			groups[r.Case.Name] = g
		}
		a := g.aggs[r.Backend]
		if a == nil {
			a = &agg{backend: r.Backend, transport: r.Transport, engine: r.Engine}
			g.aggs[r.Backend] = a
		}
		a.rounds++
		if r.Failed || r.Ops == 0 {
			a.failed++
			if a.firstError == "" {
				a.firstError = r.Error + r.FirstError
			}
			continue
		}
		a.errors += r.Errors
		if a.firstError == "" && r.FirstError != "" {
			a.firstError = r.FirstError
		}
		switch {
		case g.isList:
			a.thr = append(a.thr, r.EntriesPerS)
		case !g.byteBased:
			a.thr = append(a.thr, r.OpsPerS)
		default:
			a.thr = append(a.thr, r.MiBPerS)
			a.cpuGiB = append(a.cpuGiB, r.CPUSPerGiB)
		}
		a.cpuMsPerOp = append(a.cpuMsPerOp, r.CPUMsPerOp)
		if r.WallS > 0 {
			a.cores = append(a.cores, (r.CPUUserS+r.CPUSysS)/r.WallS)
		}
		a.opsPerS = append(a.opsPerS, r.OpsPerS)
		a.p50 = append(a.p50, r.LatencyMs.P50)
		a.p90 = append(a.p90, r.LatencyMs.P90)
		a.p99 = append(a.p99, r.LatencyMs.P99)
		a.mean = append(a.mean, r.LatencyMs.Mean)
		if r.TTFBMs != nil {
			a.ttfb = append(a.ttfb, r.TTFBMs.P50)
		}
		a.mem = append(a.mem, r.RSSPeakMiB-r.RSSBaseMiB)
		a.threads = append(a.threads, float64(r.ThreadsMax))
		a.initMs = append(a.initMs, r.ClientInitMs)
		if r.Engine == "rust" && r.Ops > 0 {
			a.ffiPerOp = append(a.ffiPerOp, float64(r.FFICalls)/float64(r.Ops))
		}
		if len(samples) > 0 && !r.MeasureStart.IsZero() {
			if u, n := meanVMCPU(samples, r.MeasureStart, r.MeasureEnd); n > 0 {
				a.vmCPU = append(a.vmCPU, 100*u)
				a.vmSamples += n
			}
		}
		if len(memSamples) > 0 && !r.MeasureStart.IsZero() {
			if m, n := peakVMMem(memSamples, r.MeasureStart, r.MeasureEnd); n > 0 {
				a.vmMem = append(a.vmMem, m)
				a.vmMemSamples += n
			}
		}
	}
	var out []*caseGroup
	for _, g := range groups {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		wi, wj := slices.Index(workloadOrder, out[i].workload), slices.Index(workloadOrder, out[j].workload)
		if wi != wj {
			return wi < wj
		}
		return out[i].name < out[j].name
	})
	return out
}

func describe(r result) string {
	c := r.Case
	load := fmt.Sprintf("%d workers x %d ops", c.Concurrency, c.Ops)
	if c.DurationS > 0 {
		load = fmt.Sprintf("%d workers, %d s timed phase", c.Concurrency, c.DurationS)
	}
	drain := ""
	if c.CopyIO {
		drain = "; io.Copy"
	}
	var s string
	switch c.Workload {
	case "read":
		s = fmt.Sprintf("read whole %s objects; %d workers x %d ops; %s Read calls", human(c.Size), c.Concurrency, c.Ops, human(int64(ioBuf(c.IOBuf))))
	case "range":
		s = fmt.Sprintf("read random %s ranges of %s objects; %s%s", human(c.RangeSize), human(c.Size), load, drain)
	case "chunked":
		s = fmt.Sprintf("read %s objects in %s ranges, one range reader per range; %s%s", human(c.Size), human(c.RangeSize), load, drain)
	case "write":
		s = fmt.Sprintf("upload %s objects; %s; %s Write calls", human(c.Size), load, human(int64(ioBuf(c.IOBuf))))
	case "append":
		s = fmt.Sprintf("appendable upload of %s objects to the zonal bucket (BidiWriteObject), finalized on Close; %s; %s Write calls",
			human(c.Size), load, human(int64(ioBuf(c.IOBuf))))
	case "list":
		s = fmt.Sprintf("list %d objects (pages of %d", c.Objects, c.PageSize)
		if c.Delimiter != "" {
			s += fmt.Sprintf(", delimiter %q", c.Delimiter)
		}
		if c.FirstPage {
			s += fmt.Sprintf("); one page per op; %s", load)
		} else {
			s += fmt.Sprintf("); %d workers x %d full listings", c.Concurrency, c.Ops)
		}
	case "list-buckets":
		s = fmt.Sprintf("list one page of %d buckets of the project per op; %s", c.PageSize, load)
	case "open":
		s = fmt.Sprintf("open a zero-length reader at a random offset of a %s object and close it; %s", human(c.Size), load)
	case "strided":
		mode := "one after the other"
		if c.Parallel {
			mode = "all at once"
		}
		s = fmt.Sprintf("read %d x %s at a %s stride of a %s object, one range reader per range, %s; %s%s",
			c.Ranges, human(c.RangeSize), human(c.Stride), human(c.Size), mode, load, drain)
	}
	if c.ConnPool > 0 {
		s += fmt.Sprintf("; gRPC clients use %d connections", c.ConnPool)
	}
	if c.Desc != "" {
		s += " — " + c.Desc
	}
	return s
}

func ioBuf(n int) int {
	if n > 0 {
		return n
	}
	return 1 << 20
}

func human(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func f1(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	return fmt.Sprintf("%.1f", v)
}

func f2(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	return fmt.Sprintf("%.2f", v)
}

func ratio(a, b float64) string {
	if math.IsNaN(a) || math.IsNaN(b) || b == 0 {
		return "–"
	}
	return fmt.Sprintf("%.2fx", a/b)
}

func orderedBackends(g *caseGroup) []*agg {
	var out []*agg
	for _, name := range backendOrder {
		if a := g.aggs[name]; a != nil {
			out = append(out, a)
		}
	}
	var rest []string
	for name := range g.aggs {
		if !slices.Contains(backendOrder, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, g.aggs[name])
	}
	return out
}

func writeReport(w io.Writer, runID string, rs []result, samples, memSamples []vmSample) {
	first, last := rs[0].Time, rs[0].Time
	rounds := 0
	for _, r := range rs {
		if r.Time.Before(first) {
			first = r.Time
		}
		if r.Time.After(last) {
			last = r.Time
		}
		rounds = max(rounds, r.Round)
	}
	fmt.Fprintf(w, "# Go SDK vs Rust core: data-path benchmark\n\n")
	fmt.Fprintf(w, "Run `%s`: %d results, %d rounds, %s to %s (%s).\n\n", runID, len(rs), rounds,
		first.Format(time.RFC3339), last.Format(time.RFC3339), last.Sub(first).Round(time.Minute))
	if e := rs[0].Env; e != nil {
		fmt.Fprintf(w, "| Environment | |\n|---|---|\n")
		fmt.Fprintf(w, "| Machine | `%s` in `%s` (%d vCPUs, GOMAXPROCS %d), kernel %s |\n", orDash(e.MachineType), orDash(e.Zone), e.NumCPU, e.GOMAXPROCS, orDash(e.Kernel))
		fmt.Fprintf(w, "| Bucket / profile | `%s` / `%s` (shuffle seed %d) |\n", e.Bucket, e.Profile, e.Seed)
		if e.ZonalBucket != "" {
			fmt.Fprintf(w, "| Zonal bucket (appendable uploads) | `%s` |\n", e.ZonalBucket)
		}
		rev := orDash(e.Revision)
		if e.Modified {
			rev += " + local changes"
		}
		fmt.Fprintf(w, "| Build | %s, revision %s |\n", e.GoVersion, rev)
		if e.Source != "" {
			fmt.Fprintf(w, "| Source | %s |\n", e.Source)
		}
		fmt.Fprintf(w, "| gRPC path | %s |\n\n", map[bool]string{true: "CloudPath (DirectPath disabled for every client)", false: "**DirectPath allowed for the Go gRPC client** (not comparable: the Rust SDK has no DirectPath)"}[e.DirectPathDisabled])
		if e.MachineType == "" {
			fmt.Fprintf(w, "> **Not run on a GCE VM**: absolute numbers depend on this machine's network.\n\n")
		}
	}

	groups := group(rs, samples, memSamples)
	fmt.Fprintf(w, "## Summary: Rust core relative to the Go SDK (medians over rounds)\n\n")
	fmt.Fprintf(w, "Throughput ratio > 1, and latency and CPU ratios < 1, favor the Rust core. `~` marks a throughput difference within the spread between rounds.")
	if len(samples) > 0 {
		fmt.Fprintf(w, " VM CPU: difference of the VM's utilization (all vCPUs) in percentage points.")
	}
	fmt.Fprintf(w, "\n\n| case | comparison | throughput | p50 latency | p99 latency | CPU per op | peak memory (Rust − Go) |")
	if len(samples) > 0 {
		fmt.Fprintf(w, " VM CPU (Rust − Go) |\n|---|---|---|---|---|---|---|---|\n")
	} else {
		fmt.Fprintf(w, "\n|---|---|---|---|---|---|---|\n")
	}
	for _, g := range groups {
		for _, cmp := range comparisons {
			ra, ga := g.aggs[cmp.rust], g.aggs[cmp.goName]
			if ra == nil || ga == nil || len(ra.thr) == 0 || len(ga.thr) == 0 {
				continue
			}
			fmt.Fprintf(w, "| %s | %s vs %s (%s) | %s%s | %s | %s | %s | %+.0f MiB |", g.name, cmp.rust, cmp.goName, cmp.note,
				ratio(median(ra.thr), median(ga.thr)), overlapMark(ra.thr, ga.thr),
				ratio(median(ra.p50), median(ga.p50)), ratio(median(ra.p99), median(ga.p99)),
				ratio(median(ra.cpuMsPerOp), median(ga.cpuMsPerOp)), median(ra.mem)-median(ga.mem))
			if len(samples) > 0 {
				fmt.Fprintf(w, " %s pp |", signed3(median(ra.vmCPU)-median(ga.vmCPU)))
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "## Per case\n\n")
	for _, g := range groups {
		fmt.Fprintf(w, "### %s\n\n%s.\n\n", g.name, g.desc)
		thrUnit := "MiB/s"
		switch {
		case g.isList:
			thrUnit = "entries/s"
		case !g.byteBased:
			thrUnit = "ops/s"
		}
		fmt.Fprintf(w, "| backend | rounds ok | %s median (min–max) | ops/s | p50 ms | p90 ms | p99 ms | mean ms | first byte/page p50 ms | CPU ms/op | CPU s/GiB | CPU cores | peak RSS − baseline MiB | threads | FFI calls/op | errors |\n", thrUnit)
		fmt.Fprintf(w, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, a := range orderedBackends(g) {
			lo, hi := minMax(a.thr)
			ffi := "–"
			if len(a.ffiPerOp) > 0 {
				ffi = f1(median(a.ffiPerOp))
			}
			errs := fmt.Sprintf("%d", a.errors)
			if a.failed > 0 {
				errs += fmt.Sprintf(", %d failed rounds", a.failed)
			}
			fmt.Fprintf(w, "| %s | %d/%d | %s (%s–%s) | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				a.backend, a.rounds-a.failed, a.rounds, f1(median(a.thr)), f1(lo), f1(hi), f1(median(a.opsPerS)),
				f1(median(a.p50)), f1(median(a.p90)), f1(median(a.p99)), f1(median(a.mean)), f1(median(a.ttfb)),
				f3(median(a.cpuMsPerOp)), f2(median(a.cpuGiB)), f3(median(a.cores)), f1(median(a.mem)),
				f1(median(a.threads)), ffi, errs)
		}
		fmt.Fprintln(w)
		for _, a := range orderedBackends(g) {
			if a.firstError != "" {
				fmt.Fprintf(w, "- `%s` first error: `%s`\n", a.backend, truncate(a.firstError, 300))
			}
		}
		fmt.Fprintln(w)
	}

	if len(samples) > 0 {
		writeVMCPU(w, rs, groups, samples)
	}
	if len(memSamples) > 0 {
		writeVMMem(w, rs, groups, memSamples)
	}

	fmt.Fprint(w, `## How to read this

- Each (case, backend) pair ran once per round in its own process, in a new random order every round; the
  tables show the median over rounds (min–max for throughput).
- Throughput: payload bytes (or list entries, or ops when nothing is transferred) of successful ops divided by
  the wall time of the measured phase (all workers). Latency: per op, from opening the reader/writer/iterator
  until the op completed (for strided cases: all ranges of the op).
- CPU: user + system time of the whole process (Go and Rust threads) during the measured phase, per op and per
  GiB moved; CPU cores is that time divided by the phase's wall time. Warmup ops are excluded.
- Memory: peak RSS (VmHWM) minus the RSS before the client was created (after the harness allocated its buffers).
- Backends use each client's defaults (chunk sizes, checksums; one gRPC connection unless the case sets a pool for
  both). Known transport differences: the Rust SDK's JSON transport is HTTP/1.1-only (compare with go-json-h1);
  Go's gRPC client reads with ReadObject unless WithGRPCBidiReads (compare rust-grpc with go-grpc-bidi);
  rust-grpc uploads to standard buckets use JSON (the Rust SDK's only gRPC upload is appendable objects).
`)
}

// writeVMMem reports the VM's memory in use during every measured phase.
func writeVMMem(w io.Writer, rs []result, groups []*caseGroup, samples []vmSample) {
	var memMiB int64
	if e := rs[0].Env; e != nil {
		memMiB = e.MemTotalMiB
	}
	idle, idleN := idleVMCPU(samples, rs)
	fmt.Fprintf(w, "## VM memory (agent.googleapis.com/memory/percent_used, state used)\n\n")
	fmt.Fprintf(w, "Highest 1-minute sample inside a measured phase (median over rounds). ")
	if memMiB > 0 {
		fmt.Fprintf(w, "The VM has %.0f GiB: 1 GiB is %.3f %%. ", float64(memMiB)/1024, 100*1024/float64(memMiB))
	}
	fmt.Fprintf(w, "Idle VM (samples before and after the run): %s %% (%d samples).\n\n", f3(idle), idleN)
	fmt.Fprintf(w, "| case | backend | VM memory used %% (peak) | above idle, GiB | process peak RSS − baseline MiB | samples |\n|---|---|---|---|---|---|\n")
	for _, g := range groups {
		for _, a := range orderedBackends(g) {
			peak := median(a.vmMem)
			above := math.NaN()
			if memMiB > 0 && !math.IsNaN(idle) {
				above = (peak - idle) / 100 * float64(memMiB) / 1024
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %d |\n", g.name, a.backend, f3(peak), f2(above), f1(median(a.mem)), a.vmMemSamples)
		}
	}
	fmt.Fprintln(w)
}

// writeVMCPU reports the VM's CPU utilization (all vCPUs) during every measured phase.
func writeVMCPU(w io.Writer, rs []result, groups []*caseGroup, samples []vmSample) {
	ncpu := 0
	if e := rs[0].Env; e != nil {
		ncpu = e.NumCPU
	}
	idle, idleN := idleVMCPU(samples, rs)
	fmt.Fprintf(w, "## VM CPU utilization (compute.googleapis.com/instance/cpu/utilization)\n\n")
	fmt.Fprintf(w, "Mean of the 1-minute samples that lie entirely inside a measured phase (median over rounds). ")
	if ncpu > 0 {
		fmt.Fprintf(w, "The VM has %d vCPUs: one fully busy vCPU is %.2f %%. ", ncpu, 100/float64(ncpu))
	}
	fmt.Fprintf(w, "Idle VM (samples before and after the run): %s %% (%d samples).\n\n", f3(100*idle), idleN)
	fmt.Fprintf(w, "| case | backend | VM CPU %% | VM CPU − idle, in vCPUs | process CPU cores | samples |\n|---|---|---|---|---|---|\n")
	for _, g := range groups {
		for _, a := range orderedBackends(g) {
			vm := median(a.vmCPU)
			busy := math.NaN()
			if ncpu > 0 && !math.IsNaN(idle) {
				busy = (vm - 100*idle) / 100 * float64(ncpu)
			}
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %d |\n", g.name, a.backend, f3(vm), f3(busy), f3(median(a.cores)), a.vmSamples)
		}
	}
	fmt.Fprintln(w)
}

func f3(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	return fmt.Sprintf("%.3f", v)
}

func signed3(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	return fmt.Sprintf("%+.3f", v)
}

func overlapMark(a, b []float64) string {
	alo, ahi := minMax(a)
	blo, bhi := minMax(b)
	if alo <= bhi && blo <= ahi {
		return " ~"
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
