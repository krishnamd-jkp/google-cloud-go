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
	"context"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"
)

// result is one JSON line of the output: one (case, backend) pair in one round, measured in
// its own process. rustbench-report reads these.
type result struct {
	Schema    int       `json:"schema"` // bumped on incompatible changes
	RunID     string    `json:"run_id"`
	Round     int       `json:"round"`
	Seq       int       `json:"seq"` // position in the round's shuffled order
	Time      time.Time `json:"time"`
	Backend   string    `json:"backend"`
	Engine    string    `json:"engine"`
	Transport string    `json:"transport"`
	Case      benchCase `json:"case"`
	Failed    bool      `json:"failed,omitempty"` // the process failed: no measurements
	Error     string    `json:"error,omitempty"`

	ClientInitMs float64 `json:"client_init_ms"`
	Ops          int     `json:"ops"`    // successful measured ops
	Errors       int     `json:"errors"` // failed measured ops (excluded from the statistics)
	FirstError   string  `json:"first_error,omitempty"`
	WarmupErrors int     `json:"warmup_errors,omitempty"`
	Bytes        int64   `json:"bytes"`             // payload bytes moved by successful ops
	Entries      int64   `json:"entries,omitempty"` // list: objects and prefixes returned
	WallS        float64 `json:"wall_s"`            // measured phase, all workers
	MiBPerS      float64 `json:"mib_per_s,omitempty"`
	OpsPerS      float64 `json:"ops_per_s"`
	EntriesPerS  float64 `json:"entries_per_s,omitempty"`
	LatencyMs    stats   `json:"latency_ms"`
	TTFBMs       *stats  `json:"ttfb_ms,omitempty"` // reads: first byte; list: first page

	// The measured phase, to match it with VM metrics (rustbench-report -vm-cpu).
	MeasureStart time.Time `json:"measure_start"`
	MeasureEnd   time.Time `json:"measure_end"`

	// Process-wide (getrusage): Go and Rust threads alike, measured phase only.
	CPUUserS   float64 `json:"cpu_user_s"`
	CPUSysS    float64 `json:"cpu_sys_s"`
	CPUSPerGiB float64 `json:"cpu_s_per_gib,omitempty"`
	CPUMsPerOp float64 `json:"cpu_ms_per_op"`

	// Memory (KiB values from /proc/self/status, reported in MiB). Baseline: after the harness
	// allocated its buffers, before the client existed; peak: VmHWM of the whole process.
	RSSBaseMiB    float64 `json:"rss_baseline_mib"`
	RSSPeakMiB    float64 `json:"rss_peak_mib"`
	RSSEndMiB     float64 `json:"rss_end_mib"`
	ThreadsMax    int64   `json:"threads_max"`
	GoroutinesMax int     `json:"goroutines_max"`
	GoGCs         uint32  `json:"go_gc_cycles"`
	GoGCPauseMs   float64 `json:"go_gc_pause_ms"`
	GoHeapSysMiB  float64 `json:"go_heap_sys_mib"`

	// Rust core boundary crossings (experimental.RustEngineStats), measured phase only.
	FFICalls int64 `json:"ffi_calls,omitempty"`
	AsyncOps int64 `json:"async_ops,omitempty"`

	Env *envInfo `json:"env,omitempty"`
}

const resultSchema = 1

type stats struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

func summarize(d []time.Duration) stats {
	if len(d) == 0 {
		return stats{}
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	ms := func(x time.Duration) float64 { return float64(x) / float64(time.Millisecond) }
	pct := func(p float64) float64 {
		// Nearest rank.
		i := int(p*float64(len(s))+0.999999) - 1
		return ms(s[min(max(i, 0), len(s)-1)])
	}
	var sum time.Duration
	for _, x := range s {
		sum += x
	}
	return stats{Min: ms(s[0]), Mean: ms(sum / time.Duration(len(s))), P50: pct(0.50), P90: pct(0.90), P99: pct(0.99), Max: ms(s[len(s)-1])}
}

type cpuTimes struct{ user, sys float64 }

func cpuNow() cpuTimes {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return cpuTimes{user: tv(ru.Utime), sys: tv(ru.Stime)}
}

// procStatusKB returns a numeric field of /proc/self/status (KiB for memory fields).
func procStatusKB(key string) int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+":"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n
			}
		}
	}
	return 0
}

func kbToMiB(kb int64) float64 { return float64(kb) / 1024 }

// sampler records the peak thread and goroutine counts while the measured phase runs.
type sampler struct {
	stop       chan struct{}
	done       sync.WaitGroup
	threads    int64
	goroutines int
}

func startSampler() *sampler {
	s := &sampler{stop: make(chan struct{})}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			s.threads = max(s.threads, procStatusKB("Threads"))
			s.goroutines = max(s.goroutines, runtime.NumGoroutine())
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
	return s
}

func (s *sampler) finish() (threads int64, goroutines int) {
	close(s.stop)
	s.done.Wait()
	return s.threads, s.goroutines
}

// envInfo describes where a suite ran; recorded in every result line.
type envInfo struct {
	Host               string `json:"host"`
	MachineType        string `json:"machine_type,omitempty"`
	Zone               string `json:"zone,omitempty"`
	InstanceID         string `json:"instance_id,omitempty"` // GCE instance id (for VM metrics)
	Kernel             string `json:"kernel,omitempty"`
	NumCPU             int    `json:"num_cpu"`
	GOMAXPROCS         int    `json:"gomaxprocs"`
	GoVersion          string `json:"go_version"`
	Revision           string `json:"revision,omitempty"` // VCS revision the binary was built from
	Modified           bool   `json:"modified,omitempty"` // built from a tree with local changes
	Source             string `json:"source,omitempty"`   // $RUSTBENCH_SOURCE (set by the VM scripts)
	DirectPathDisabled bool   `json:"directpath_disabled"`
	Bucket             string `json:"bucket"`
	Profile            string `json:"profile"`
	Seed               int64  `json:"seed"`
}

func collectEnv(ctx context.Context, o suiteOptions) *envInfo {
	e := &envInfo{
		NumCPU:             runtime.NumCPU(),
		GOMAXPROCS:         runtime.GOMAXPROCS(0),
		GoVersion:          runtime.Version(),
		Source:             os.Getenv("RUSTBENCH_SOURCE"),
		DirectPathDisabled: os.Getenv("GOOGLE_CLOUD_DISABLE_DIRECT_PATH") == "true",
		Bucket:             o.bucket,
		Profile:            o.profile,
		Seed:               o.seed,
	}
	e.Host, _ = os.Hostname()
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		e.Kernel = strings.TrimSpace(string(b))
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				e.Revision = s.Value
			case "vcs.modified":
				e.Modified = s.Value == "true"
			}
		}
	}
	if metadata.OnGCE() {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if mt, err := metadata.GetWithContext(mctx, "instance/machine-type"); err == nil {
			e.MachineType = mt[strings.LastIndex(mt, "/")+1:]
		}
		if z, err := metadata.ZoneWithContext(mctx); err == nil {
			e.Zone = z
		}
		if id, err := metadata.InstanceIDWithContext(mctx); err == nil {
			e.InstanceID = id
		}
	}
	return e
}
