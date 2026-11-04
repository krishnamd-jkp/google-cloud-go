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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"cloud.google.com/go/storage"
	"cloud.google.com/go/storage/experimental"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

// Workloads.
const (
	wWrite   = "write"   // upload Size bytes per op (Writer)
	wRead    = "read"    // download a whole Size-byte object per op (Reader)
	wRange   = "range"   // download a random RangeSize window of a Size-byte object per op
	wList    = "list"    // list every object under a dataset prefix per op (all pages)
	wOpen    = "open"    // open a zero-length reader at a random offset of a Size-byte object, close it
	wStrided = "strided" // download Ranges windows of RangeSize bytes, Stride apart, one reader per window
)

var workloads = []string{wWrite, wRead, wRange, wList, wOpen, wStrided}

// backend is one client configuration under test.
type backend struct {
	name      string
	engine    string // "go" (the Go SDK's own transports) or "rust" (the Rust core)
	transport string // "json", "xml" or "grpc"
	desc      string
	newClient func(context.Context) (*storage.Client, error)
	// skip lists workloads this backend does not run, with the reason. A skipped pair would
	// either be impossible or exactly repeat another backend's measurement.
	skip map[string]string
}

var backends = []backend{
	{
		name: "go-json", engine: "go", transport: "json",
		desc: "NewClient + WithJSONReads: JSON API, Go's default HTTP transport (HTTP/2 when negotiated)",
		newClient: func(ctx context.Context) (*storage.Client, error) {
			return storage.NewClient(ctx, storage.WithJSONReads())
		},
	},
	{
		name: "go-json-h1", engine: "go", transport: "json",
		desc:      "go-json restricted to HTTP/1.1, the protocol of the Rust SDK's JSON transport",
		newClient: newGoJSONHTTP1,
	},
	{
		name: "go-xml", engine: "go", transport: "xml",
		desc: "NewClient defaults: XML API reads (what most Go users run today)",
		newClient: func(ctx context.Context) (*storage.Client, error) {
			return storage.NewClient(ctx, storage.WithXMLReads())
		},
		skip: map[string]string{
			wWrite: "NewClient uploads over the JSON API (= go-json)",
			wList:  "NewClient lists over the JSON API (= go-json)",
		},
	},
	{
		name: "go-grpc", engine: "go", transport: "grpc",
		desc: "NewGRPCClient, CloudPath: ReadObject (server-streaming) reads, BidiWriteObject uploads",
		newClient: func(ctx context.Context) (*storage.Client, error) {
			return storage.NewGRPCClient(ctx)
		},
	},
	{
		name: "go-grpc-bidi", engine: "go", transport: "grpc",
		desc: "go-grpc + WithGRPCBidiReads: BidiReadObject reads, the RPC the Rust SDK reads with",
		newClient: func(ctx context.Context) (*storage.Client, error) {
			return storage.NewGRPCClient(ctx, storage.WithGRPCBidiReads())
		},
		skip: map[string]string{
			wWrite: "bidi reads do not change uploads (= go-grpc)",
			wList:  "bidi reads do not change listing (= go-grpc)",
		},
	},
	{
		name: "rust-json", engine: "rust", transport: "json",
		desc: "NewClient + WithRustCore: every operation in the Rust core, JSON API over HTTP/1.1",
		newClient: func(ctx context.Context) (*storage.Client, error) {
			return storage.NewClient(ctx, experimental.WithRustCore())
		},
	},
	{
		name: "rust-grpc", engine: "rust", transport: "grpc",
		desc: "NewGRPCClient + WithRustCore: BidiReadObject reads and v2 ListObjects over gRPC (CloudPath)",
		newClient: func(ctx context.Context) (*storage.Client, error) {
			return storage.NewGRPCClient(ctx, experimental.WithRustCore())
		},
		skip: map[string]string{
			wWrite: "the Rust SDK (1.20) has no gRPC upload for standard buckets; uploads use the JSON API (= rust-json)",
		},
	},
}

func lookupBackend(name string) (backend, error) {
	for _, b := range backends {
		if b.name == name {
			return b, nil
		}
	}
	var names []string
	for _, b := range backends {
		names = append(names, b.name)
	}
	return backend{}, fmt.Errorf("unknown backend %q (have %s)", name, strings.Join(names, ", "))
}

// newGoJSONHTTP1 builds go-json on a transport that never negotiates HTTP/2: a non-nil, empty
// TLSNextProto disables net/http's automatic HTTP/2 (see net/http.Transport), and the
// transport is not handed to x/net/http2 (which the default storage transport is).
func newGoJSONHTTP1(ctx context.Context) (*storage.Client, error) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConnsPerHost = 100 // as the default storage transport
	base.ForceAttemptHTTP2 = false
	base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	rt, err := htransport.NewTransport(ctx, base,
		option.WithScopes(storage.ScopeFullControl, "https://www.googleapis.com/auth/cloud-platform"))
	if err != nil {
		return nil, err
	}
	return storage.NewClient(ctx, option.WithHTTPClient(&http.Client{Transport: rt}), storage.WithJSONReads())
}

// benchCase is one workload configuration. Every op of a case does the same work on every
// backend.
type benchCase struct {
	Name        string `json:"name"`
	Workload    string `json:"workload"`
	Size        int64  `json:"size,omitempty"`       // object size (write, read), source object size (range, open, strided)
	RangeSize   int64  `json:"range_size,omitempty"` // range: bytes per op, at a random offset; strided: bytes per window
	Concurrency int    `json:"concurrency"`          // workers; each runs its ops one after the other
	Ops         int    `json:"ops"`                  // measured ops per worker (with DurationS: a cap, 0 = none)
	Warmup      int    `json:"warmup"`               // unmeasured ops per worker (connections, tokens)
	IOBuf       int    `json:"io_buf,omitempty"`     // bytes per Write/Read call by the application (default 1 MiB)
	Objects     int    `json:"objects,omitempty"`    // list: objects in the listed dataset
	PageSize    int    `json:"page_size,omitempty"`  // list: page size
	Delimiter   string `json:"delimiter,omitempty"`  // list: "/" lists the dataset's 100 directories
	Desc        string `json:"desc,omitempty"`

	Sources   int   `json:"sources,omitempty"`    // read, range, open, strided: source objects (default datasetCount(Size))
	Ranges    int   `json:"ranges,omitempty"`     // strided: windows per op
	Stride    int64 `json:"stride,omitempty"`     // strided: distance between the starts of two windows
	Parallel  bool  `json:"parallel,omitempty"`   // strided: all windows' readers at once (default: one after the other)
	CopyIO    bool  `json:"copy_io,omitempty"`    // drain readers with io.Copy (Reader.WriteTo) instead of IOBuf-sized Read calls
	DurationS int   `json:"duration_s,omitempty"` // measured phase length in seconds (default: Ops ops per worker)
}

// sources is the number of dataset objects a read-type case reads from.
func (c benchCase) sources() int {
	if c.Sources > 0 {
		return c.Sources
	}
	return datasetCount(c.Size)
}

func (c benchCase) ioBuf() int {
	if c.IOBuf > 0 {
		return c.IOBuf
	}
	return 1 << 20
}

const (
	kib = 1 << 10
	mib = 1 << 20
)

// profiles: "default" is the full comparison (about an hour for 5 rounds on a 16-vCPU VM),
// "quick" a smoke test of the whole pipeline (minutes), "ssb" the shared benchmarks' read
// workloads ("ssb-smoke": the same for 15 seconds each).
var profiles = map[string][]benchCase{
	"default": {
		{Name: "read-64KiB-c16", Workload: wRead, Size: 64 * kib, Concurrency: 16, Ops: 100, Warmup: 5,
			Desc: "small-object reads: per-request overhead and latency"},
		{Name: "read-16MiB-c8", Workload: wRead, Size: 16 * mib, Concurrency: 8, Ops: 8, Warmup: 1,
			Desc: "medium objects, 8 concurrent streams"},
		{Name: "read-256MiB-c1", Workload: wRead, Size: 256 * mib, Concurrency: 1, Ops: 4, Warmup: 1,
			Desc: "large object, single-stream throughput"},
		{Name: "read-256MiB-c4", Workload: wRead, Size: 256 * mib, Concurrency: 4, Ops: 3, Warmup: 1,
			Desc: "large objects, 4 concurrent streams"},
		{Name: "read-16MiB-c8-buf32KiB", Workload: wRead, Size: 16 * mib, Concurrency: 8, Ops: 8, Warmup: 1, IOBuf: 32 * kib,
			Desc: "as read-16MiB-c8 with 32 KiB Read calls (io.Copy's buffer): per-call overhead"},
		{Name: "range-64KiB-c16", Workload: wRange, Size: 256 * mib, RangeSize: 64 * kib, Concurrency: 16, Ops: 60, Warmup: 3,
			Desc: "random 64 KiB windows of 256 MiB objects"},
		{Name: "range-1MiB-c16", Workload: wRange, Size: 256 * mib, RangeSize: 1 * mib, Concurrency: 16, Ops: 30, Warmup: 2,
			Desc: "random 1 MiB windows of 256 MiB objects"},
		{Name: "write-64KiB-c16", Workload: wWrite, Size: 64 * kib, Concurrency: 16, Ops: 50, Warmup: 2,
			Desc: "small uploads (single request): per-request overhead"},
		{Name: "write-8MiB-c8", Workload: wWrite, Size: 8 * mib, Concurrency: 8, Ops: 5, Warmup: 1,
			Desc: "single-request uploads below the 16 MiB chunk size"},
		{Name: "write-256MiB-c1", Workload: wWrite, Size: 256 * mib, Concurrency: 1, Ops: 3, Warmup: 1,
			Desc: "resumable upload in 16 MiB chunks, single stream"},
		{Name: "write-64MiB-c4", Workload: wWrite, Size: 64 * mib, Concurrency: 4, Ops: 3, Warmup: 1,
			Desc: "resumable uploads, 4 concurrent streams"},
		{Name: "write-8MiB-c8-buf32KiB", Workload: wWrite, Size: 8 * mib, Concurrency: 8, Ops: 5, Warmup: 1, IOBuf: 32 * kib,
			Desc: "as write-8MiB-c8 with 32 KiB Write calls (io.Copy's buffer): per-call overhead"},
		{Name: "list-10k-c1", Workload: wList, Objects: 10000, PageSize: 1000, Concurrency: 1, Ops: 5, Warmup: 1,
			Desc: "list 10,000 objects in pages of 1,000"},
		{Name: "list-10k-c8", Workload: wList, Objects: 10000, PageSize: 1000, Concurrency: 8, Ops: 3, Warmup: 1,
			Desc: "8 concurrent full listings of 10,000 objects"},
		{Name: "list-10k-delim", Workload: wList, Objects: 10000, PageSize: 1000, Delimiter: "/", Concurrency: 4, Ops: 10, Warmup: 1,
			Desc: "delimiter listing: the 100 directories of the 10,000-object dataset"},
	},
	"quick": {
		{Name: "read-64KiB-c4", Workload: wRead, Size: 64 * kib, Concurrency: 4, Ops: 10, Warmup: 1},
		{Name: "read-16MiB-c2", Workload: wRead, Size: 16 * mib, Concurrency: 2, Ops: 2, Warmup: 1},
		{Name: "range-64KiB-c4", Workload: wRange, Size: 16 * mib, RangeSize: 64 * kib, Concurrency: 4, Ops: 10, Warmup: 1},
		{Name: "write-64KiB-c4", Workload: wWrite, Size: 64 * kib, Concurrency: 4, Ops: 5, Warmup: 1},
		{Name: "write-32MiB-c1", Workload: wWrite, Size: 32 * mib, Concurrency: 1, Ops: 1, Warmup: 1},
		{Name: "list-1k-c1", Workload: wList, Objects: 1000, PageSize: 200, Concurrency: 1, Ops: 2, Warmup: 1},
	},
	// The Open, Read50x2x4 and Random8K workloads of the shared storage benchmarks
	// (storage-shared-benchmarking, w1r3/go), with one worker as there. Those use a
	// MultiRangeDownloader, which the Rust core does not have yet, so both clients read with one
	// range reader per range here (as the shared benchmarks' non-MRD variants do). Every case
	// runs for DurationS, so that the 1-minute VM metrics
	// (compute.googleapis.com/instance/cpu/utilization) can be attributed to it: run the suite
	// with -cooldown 60s.
	"ssb": {
		{Name: "open", Workload: wOpen, Size: 100 * kib, Sources: 8, Concurrency: 1, Warmup: 5, DurationS: 240,
			Desc: "NewRangeReader(random offset, 0) + Close: open a read stream and wait for the first response (with the object)"},
		{Name: "read50x2x4", Workload: wStrided, Size: 200 * mib, Sources: 1, Ranges: 50, RangeSize: 2 * mib, Stride: 4 * mib,
			Concurrency: 1, Warmup: 1, CopyIO: true, DurationS: 240,
			Desc: "one NewRangeReader per range, one after the other (the shared benchmarks' non-MRD variant)"},
		{Name: "read50x2x4-parallel", Workload: wStrided, Size: 200 * mib, Sources: 1, Ranges: 50, RangeSize: 2 * mib, Stride: 4 * mib,
			Parallel: true, Concurrency: 1, Warmup: 1, CopyIO: true, DurationS: 240,
			Desc: "all 50 range readers at once: closer to a MultiRangeDownloader with 50 ranges"},
		{Name: "random8k", Workload: wRange, Size: 100 * mib, Sources: 1, RangeSize: 8 * kib, Concurrency: 1, Warmup: 5,
			CopyIO: true, DurationS: 240,
			Desc: "one NewRangeReader per 8 KiB range"},
	},
}

func init() {
	smoke := slices.Clone(profiles["ssb"])
	for i := range smoke {
		smoke[i].DurationS, smoke[i].Warmup = 15, min(smoke[i].Warmup, 1)
	}
	profiles["ssb-smoke"] = smoke
}

func profileNames() []string {
	var names []string
	for n := range profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func printMatrix(w io.Writer, profile string) {
	cases := mustProfile(profile)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BACKEND\tENGINE\tTRANSPORT\tDESCRIPTION")
	for _, b := range backends {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.name, b.engine, b.transport, b.desc)
	}
	tw.Flush()

	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "WORKLOAD")
	for _, b := range backends {
		fmt.Fprintf(tw, "\t%s", b.name)
	}
	fmt.Fprintln(tw)
	for _, wl := range workloads {
		fmt.Fprint(tw, wl)
		for _, b := range backends {
			mark := "run"
			if _, skip := b.skip[wl]; skip {
				mark = "skip"
			}
			fmt.Fprintf(tw, "\t%s", mark)
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nSkipped pairs:")
	for _, b := range backends {
		for _, wl := range workloads {
			if why, skip := b.skip[wl]; skip {
				fmt.Fprintf(w, "  %-13s %-6s %s\n", b.name, wl, why)
			}
		}
	}

	fmt.Fprintf(w, "\nCases of profile %q:\n", profile)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CASE\tWORKLOAD\tSIZE\tRANGE\tWORKERS\tOPS/WORKER\tIO BUF\tDESCRIPTION")
	for _, c := range cases {
		size, rng, iobuf := humanBytes(c.Size), humanBytes(c.RangeSize), humanBytes(int64(c.ioBuf()))
		if c.Workload == wList {
			size, iobuf = fmt.Sprintf("%d objects", c.Objects), "-"
		}
		if c.Workload == wStrided {
			rng = fmt.Sprintf("%d x %s, stride %s", c.Ranges, humanBytes(c.RangeSize), humanBytes(c.Stride))
			if c.Parallel {
				rng += ", parallel"
			}
		}
		if c.CopyIO {
			iobuf = "io.Copy"
		}
		ops := fmt.Sprintf("%d (+%d warmup)", c.Ops, c.Warmup)
		if c.DurationS > 0 {
			ops = fmt.Sprintf("%ds (+%d warmup)", c.DurationS, c.Warmup)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			c.Name, c.Workload, size, rng, c.Concurrency, ops, iobuf, c.Desc)
	}
	tw.Flush()
}

func humanBytes(n int64) string {
	switch {
	case n == 0:
		return "-"
	case n%mib == 0:
		return fmt.Sprintf("%dMiB", n/mib)
	case n%kib == 0:
		return fmt.Sprintf("%dKiB", n/kib)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
