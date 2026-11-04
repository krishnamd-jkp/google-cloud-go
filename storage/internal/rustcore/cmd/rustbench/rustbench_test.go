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

// Hermetic tests of the harness itself (no bucket, no network): go test ./internal/rustcore/cmd/rustbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProfilesAreConsistent(t *testing.T) {
	for name, cases := range profiles {
		seen := map[string]bool{}
		for _, c := range cases {
			if seen[c.Name] {
				t.Errorf("%s: duplicate case %q", name, c.Name)
			}
			seen[c.Name] = true
			if c.Concurrency < 1 || (c.Ops < 1 && c.DurationS <= 0) || c.Warmup < 0 {
				t.Errorf("%s/%s: concurrency %d, ops %d, duration %ds, warmup %d", name, c.Name, c.Concurrency, c.Ops, c.DurationS, c.Warmup)
			}
			switch c.Workload {
			case wRead, wWrite, wOpen:
				if c.Size <= 0 {
					t.Errorf("%s/%s: size %d", name, c.Name, c.Size)
				}
			case wRange:
				if c.RangeSize <= 0 || c.RangeSize > c.Size {
					t.Errorf("%s/%s: range %d of %d", name, c.Name, c.RangeSize, c.Size)
				}
			case wStrided:
				if c.Ranges < 1 || c.RangeSize <= 0 || int64(c.Ranges-1)*c.Stride+c.RangeSize > c.Size || (c.Parallel && !c.CopyIO) {
					t.Errorf("%s/%s: %d windows of %d, stride %d, in %d (parallel %v, copy_io %v)", name, c.Name,
						c.Ranges, c.RangeSize, c.Stride, c.Size, c.Parallel, c.CopyIO)
				}
			case wList:
				if c.Objects <= 0 || c.PageSize <= 0 || c.PageSize > 1000 {
					t.Errorf("%s/%s: objects %d, page size %d", name, c.Name, c.Objects, c.PageSize)
				}
			default:
				t.Errorf("%s/%s: unknown workload %q", name, c.Name, c.Workload)
			}
		}
	}
}

// The ssb profile mirrors the shared benchmarks' workload definitions.
func TestSSBProfile(t *testing.T) {
	want := map[string]benchCase{
		"open":                {Workload: wOpen},
		"read50x2x4":          {Workload: wStrided, Size: 200 * mib, Ranges: 50, RangeSize: 2 * mib, Stride: 4 * mib},
		"read50x2x4-parallel": {Workload: wStrided, Size: 200 * mib, Ranges: 50, RangeSize: 2 * mib, Stride: 4 * mib, Parallel: true},
		"random8k":            {Workload: wRange, Size: 100 * mib, RangeSize: 8 * kib},
	}
	for _, profile := range []string{"ssb", "ssb-smoke"} {
		cases := mustProfile(profile)
		if len(cases) != len(want) {
			t.Fatalf("%s: %d cases, want %d", profile, len(cases), len(want))
		}
		for _, c := range cases {
			w, ok := want[c.Name]
			if !ok {
				t.Errorf("%s: unexpected case %q", profile, c.Name)
				continue
			}
			if c.Workload != w.Workload || c.Ranges != w.Ranges || c.Stride != w.Stride || c.Parallel != w.Parallel ||
				(w.Size != 0 && (c.Size != w.Size || c.RangeSize != w.RangeSize)) || c.Concurrency != 1 || c.DurationS <= 0 {
				t.Errorf("%s/%s = %+v, want %+v (one worker, timed)", profile, c.Name, c, w)
			}
		}
	}
	if s := mustProfile("ssb-smoke"); s[0].DurationS >= mustProfile("ssb")[0].DurationS {
		t.Errorf("ssb-smoke must be shorter than ssb")
	}
}

func TestMatrix(t *testing.T) {
	be, err := lookupBackend("rust-grpc")
	if err != nil {
		t.Fatal(err)
	}
	if _, skip := be.skip[wWrite]; !skip {
		t.Error("rust-grpc must skip writes (no gRPC upload in the Rust SDK)")
	}
	for _, name := range []string{"go-json", "go-json-h1", "go-grpc", "rust-json"} {
		b, err := lookupBackend(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, wl := range workloads {
			if _, skip := b.skip[wl]; skip {
				t.Errorf("%s must run %s", name, wl)
			}
		}
	}
	var out bytes.Buffer
	printMatrix(&out, "default")
	if !strings.Contains(out.String(), "no gRPC upload") {
		t.Errorf("matrix output lacks the skip reasons:\n%s", out.String())
	}
}

func TestDatasetPlan(t *testing.T) {
	plan := datasetPlan("p", []benchCase{
		{Workload: wRead, Size: 64 * kib},
		{Workload: wRange, Size: 256 * mib, RangeSize: mib},
		{Workload: wList, Objects: 1000},
		{Workload: wWrite, Size: 8 * mib},
	})
	reads, lists := 0, 0
	for name, size := range plan {
		switch {
		case strings.HasPrefix(name, "p/data/read/64KiB/"):
			reads++
			if size != 64*kib {
				t.Errorf("%s: size %d", name, size)
			}
		case strings.HasPrefix(name, "p/data/read/256MiB/"):
			reads++
		case strings.HasPrefix(name, listPrefix("p", 1000)):
			lists++
		default:
			t.Errorf("unexpected dataset object %q", name)
		}
	}
	if reads != datasetCount(64*kib)+datasetCount(256*mib) || lists != 1000 {
		t.Errorf("plan: %d read objects, %d list objects", reads, lists)
	}
	dirs := map[string]bool{}
	for i := range 1000 {
		name := strings.TrimPrefix(listObjectName("p", 1000, i), listPrefix("p", 1000))
		dirs[name[:strings.Index(name, "/")]] = true
	}
	if len(dirs) != listDirs(1000) {
		t.Errorf("list dataset spans %d directories, want %d", len(dirs), listDirs(1000))
	}
	if err := checkPrefix("/"); err == nil {
		t.Error("an empty prefix must be refused (cleanup would delete the whole bucket)")
	}

	// Sources overrides the number of source objects (and they are shared by size).
	plan = datasetPlan("p", []benchCase{
		{Workload: wOpen, Size: 100 * kib, Sources: 8},
		{Workload: wStrided, Size: 200 * mib, Sources: 1},
		{Workload: wRange, Size: 200 * mib, Sources: 1},
	})
	if len(plan) != 9 || plan["p/data/read/200MiB/000"] != 200*mib || plan["p/data/read/100KiB/007"] != 100*kib {
		t.Errorf("plan with sources: %v", plan)
	}
}

func TestSummarize(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	s := summarize(d)
	if s.Min != 1 || s.Max != 100 || s.P50 != 50 || s.P90 != 90 || s.P99 != 99 || s.Mean != 50.5 {
		t.Errorf("summarize(1..100 ms) = %+v", s)
	}
	if one := summarize([]time.Duration{7 * time.Millisecond}); one.P50 != 7 || one.P99 != 7 {
		t.Errorf("summarize(7 ms) = %+v", one)
	}
}

func TestCaseJSONRoundTrip(t *testing.T) {
	for _, profile := range profileNames() {
		for _, c := range profiles[profile] {
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolveCase("", string(b), "")
			if err != nil || got != c {
				t.Errorf("round trip of %s/%s: %+v, %v", profile, c.Name, got, err)
			}
		}
	}
}

// A timed phase ends at its deadline, and a worker whose ops keep failing gives up.
func TestRunWorkersDeadlineAndFailures(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int64
	ok := func(context.Context, int, int) (opResult, error) {
		calls.Add(1)
		time.Sleep(time.Millisecond)
		return opResult{bytes: 1}, nil
	}
	rec := newRecorder()
	start := time.Now()
	if failed := runWorkers(ctx, 2, math.MaxInt, start.Add(100*time.Millisecond), ok, rec); failed != 0 {
		t.Errorf("failed = %d", failed)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("timed phase took %v, want about 100ms", d)
	}
	if n := len(rec.lat); n < 10 || int64(n) != calls.Load() {
		t.Errorf("recorded %d ops, %d calls", n, calls.Load())
	}

	bad := func(context.Context, int, int) (opResult, error) { return opResult{}, errors.New("denied") }
	rec = newRecorder()
	if failed := runWorkers(ctx, 3, math.MaxInt, time.Now().Add(time.Minute), bad, rec); failed != 3*maxConsecutiveFailures {
		t.Errorf("failed = %d, want %d (each worker stops after %d failures in a row)", failed, 3*maxConsecutiveFailures, maxConsecutiveFailures)
	}
	if rec.firstErr != "denied" || len(rec.lat) != 0 {
		t.Errorf("recorder: first error %q, %d latencies", rec.firstErr, len(rec.lat))
	}
	if failed := runWorkers(ctx, 1, 0, time.Time{}, bad, nil); failed != 0 {
		t.Errorf("zero ops (no warmup) ran ops: %d failures", failed)
	}
}
