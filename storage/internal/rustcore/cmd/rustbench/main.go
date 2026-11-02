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

// Command rustbench compares the pure-Go client with the Rust core
// (experimental.WithRustCore) on the same W1R3 workload: 1 write + 3 reads per iteration.
// Run each backend in its own process so CPU accounting (getrusage, Go + Rust threads) is clean:
//
//	go build -tags gcsrust -o /tmp/rustbench ./internal/rustcore/cmd/rustbench
//	/tmp/rustbench -backend go   -bucket BUCKET -size 16777216 -iters 3
//	/tmp/rustbench -backend rust -bucket BUCKET -size 16777216 -iters 3
//
// Objects are written under rustbench/<backend>/... and deleted at the end (-keep to keep them).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"cloud.google.com/go/storage/experimental"
)

func cpuSeconds() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func procStatus(key string) int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, key+":") {
			if f := strings.Fields(l); len(f) >= 2 {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				return n
			}
		}
	}
	return 0
}

// Hide io.ReaderFrom / io.WriterTo so io.CopyBuffer really issues 32 KiB calls.
type noWT struct{ io.Reader }
type noRF struct{ io.Writer }

func p50(d []time.Duration) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func main() {
	backend := flag.String("backend", "go", "go | rust")
	bucket := flag.String("bucket", "", "bucket (required)")
	size := flag.Int("size", 16<<20, "object size in bytes")
	iters := flag.Int("iters", 3, "iterations (1 write + 3 reads each)")
	style := flag.String("style", "whole", "whole (one Write/ReadFull) | 32k (io.CopyBuffer with 32 KiB calls)")
	keep := flag.Bool("keep", false, "keep the objects")
	flag.Parse()
	if *bucket == "" {
		log.Fatal("-bucket is required")
	}
	ctx := context.Background()
	var c *storage.Client
	var err error
	switch *backend {
	case "go":
		c, err = storage.NewClient(ctx)
	case "rust":
		c, err = storage.NewClient(ctx, experimental.WithRustCore())
	default:
		log.Fatalf("unknown -backend %q", *backend)
	}
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	data := make([]byte, *size)
	rand.Read(data)
	buf := make([]byte, *size)
	cbuf := make([]byte, 32<<10)
	stats0 := experimental.RustEngineStats()
	cpu0 := cpuSeconds()
	maxThreads := procStatus("Threads")
	var wd, rd []time.Duration
	var names []string
	for i := 0; i < *iters; i++ {
		name := fmt.Sprintf("rustbench/%s/%s/%d/%d", *backend, *style, *size, i)
		names = append(names, name)
		obj := c.Bucket(*bucket).Object(name)
		t := time.Now()
		w := obj.NewWriter(ctx)
		if *style == "whole" {
			_, err = w.Write(data)
		} else {
			_, err = io.CopyBuffer(noRF{w}, noWT{bytes.NewReader(data)}, cbuf)
		}
		if err == nil {
			err = w.Close()
		}
		if err != nil {
			log.Fatalf("write %s: %v", name, err)
		}
		wd = append(wd, time.Since(t))
		for k := 0; k < 3; k++ {
			t = time.Now()
			r, err := obj.NewReader(ctx)
			if err != nil {
				log.Fatalf("open %s: %v", name, err)
			}
			var n int64
			if *style == "whole" {
				m, e := io.ReadFull(r, buf)
				n, err = int64(m), e
			} else {
				n, err = io.CopyBuffer(noRF{io.Discard}, noWT{r}, cbuf)
			}
			r.Close()
			if err != nil || n != int64(*size) {
				log.Fatalf("read %s: n=%d err=%v", name, n, err)
			}
			rd = append(rd, time.Since(t))
			maxThreads = max(maxThreads, procStatus("Threads"))
		}
	}
	cpu := cpuSeconds() - cpu0
	stats := experimental.RustEngineStats()
	gib := float64(*size) * float64(4**iters) / (1 << 30)
	ops := float64(4 * *iters)
	engine := "pure Go"
	if stats["ffi_calls"] > stats0["ffi_calls"] {
		engine = "Rust core"
	}
	fmt.Printf("backend=%s (%s) style=%s size=%dMiB iters=%d\n", *backend, engine, *style, *size>>20, *iters)
	fmt.Printf("  write p50=%v  read p50=%v  cpu=%.2f s/GiB  ffi/op=%.0f  async/op=%.0f  max_threads=%d  peak_rss=%dMiB\n",
		p50(wd).Round(time.Millisecond), p50(rd).Round(time.Millisecond), cpu/gib,
		float64(stats["ffi_calls"]-stats0["ffi_calls"])/ops, float64(stats["async_ops"]-stats0["async_ops"])/ops,
		maxThreads, procStatus("VmHWM")>>10)
	if !*keep {
		for _, n := range names {
			if err := c.Bucket(*bucket).Object(n).Delete(ctx); err != nil {
				log.Printf("cleanup %s: %v", n, err)
			}
		}
	}
}
