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

// Command rapidbench runs workloads 1-6 of the SDK consolidation benchmark plan through the
// public Go storage API, on the Go SDK's own transports or on the Rust core
// (experimental.WithRustCore). It mirrors the Java PoC's RapidBenchmark (same workloads, sizes,
// measured windows and output files), so that the Go and Java reports can be compared.
//
// The engine is chosen only by --engine=go|rust. The binary, this code and the runtime settings
// are the same for both. Both engines use CloudPath and their default settings, so one gRPC
// connection each: the Rust core follows the Go client's connection-pool setting.
// --transport=bidi uses a gRPC client (BidiReadObject reads, appendable BidiWriteObject uploads),
// --transport=json an HTTP client.
//
//	open (1)          NewRangeReader(0, 0) + Close on an --open-size object (default 100 MiB): open
//	                  a BidiReadObject stream and wait for its first response (the object
//	                  metadata), then close it
//	read50x2x4 (2)    50 reads of 2 MiB at a 4 MiB stride from a 200 MiB object;
//	                  --mode=concurrent (all 50 at once) or serial (one after another)
//	random8k (3)      random 8 KiB reads from a 100 MiB object
//	read (4)          --threads workers read random --range-size ranges of an --object-size object
//	write (5)         --threads workers each keep overwriting their own --object-size object, in
//	                  --write-chunk slices (appendable and finalized on Close for bidi)
//	list-objects (6)  one page of --list-items objects; list-buckets (6): one page of buckets
//
// The Rust core has no MultiRangeDownloader yet, so every Bidi read is one range reader (one
// BidiReadObject stream) per range, on both engines.
//
// Workloads 1-3 and 6 run one operation at a time (--threads=1); 4 and 5 use 48 workers. Latency
// is measured around the outermost API calls of each operation; only operations that end inside
// the measured window count. Results go to --out/<workload>-<transport>-<mode>-<label>-<time>/:
// summary.json, timeline.csv (per second: operations and bytes or items) and ops.csv (every
// operation). See ../../bench/rapid/run_all.sh for the full pass.
//
// Build (cgo, linux/amd64; build the Rust core first with ../../build.sh):
//
//	go build -tags gcsrust -o rapidbench ./internal/rustcore/cmd/rapidbench
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/storage"
	"cloud.google.com/go/storage/experimental"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

const (
	mib = 1 << 20
	// opTimeout bounds one read, open or list, so that a stalled call becomes a counted error
	// instead of hanging the run; writeTimeout bounds one 200 MB upload.
	opTimeout    = 120 * time.Second
	writeTimeout = 600 * time.Second
	// timeLayout is RFC 3339 with microseconds, which every Python version parses.
	timeLayout = "2006-01-02T15:04:05.000000Z07:00"
)

type bench struct {
	engine, workload, mode, label          string
	bucket, bucketLabel, project           string
	json, cleanup                          bool
	threads, durationS, warmupS, listItems int
	objectSize, openSize                   int64
	rangeSize, writeChunk                  int
	objectName, prefix, out                string
	client                                 *storage.Client
	bytesWritten                           atomic.Int64
	windowBytesWritten                     int64 // -1 unless a write workload
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("rapidbench: ")
	fs := flag.NewFlagSet("rapidbench", flag.ExitOnError)
	b := &bench{windowBytesWritten: -1}
	fs.StringVar(&b.engine, "engine", "go", "go (the Go SDK's own transports) or rust (experimental.WithRustCore)")
	fs.StringVar(&b.workload, "workload", "", "open | read50x2x4 | random8k | read | write | list-objects | list-buckets")
	transport := fs.String("transport", "bidi", "bidi (gRPC client) or json (HTTP client)")
	fs.StringVar(&b.mode, "mode", "", "read50x2x4: concurrent or serial")
	fs.StringVar(&b.bucket, "bucket", "", "bucket (not needed for list-buckets)")
	fs.StringVar(&b.bucketLabel, "bucket-label", "", "how summary.json names the bucket (default: the bucket name)")
	fs.StringVar(&b.project, "project", "", "project whose buckets list-buckets lists (default: the VM's project)")
	threads := fs.Int("threads", 0, "workers (default 48 for read and write, else 1)")
	duration := fs.Int("duration", 0, "measured window in seconds (default 300 for read and write, else 180)")
	warmup := fs.Int("warmup", -1, "warm-up in seconds (default 30 for read and write, else 10)")
	fs.Int64Var(&b.objectSize, "object-size", 200_000_000, "object size for read and write")
	fs.Int64Var(&b.openSize, "open-size", 100*mib, "open: object size (another size than the default adds the size to the op name)")
	fs.IntVar(&b.rangeSize, "range-size", 16*mib, "read: bytes per read")
	fs.IntVar(&b.writeChunk, "write-chunk", 2*mib, "write: bytes per Write call")
	fs.IntVar(&b.listItems, "list-items", 100, "list-objects, list-buckets: page size")
	fs.StringVar(&b.label, "label", "", "configuration name for the results (default <engine>-default)")
	fs.StringVar(&b.objectName, "object", "", "source object name (default rapid/read-<size>)")
	fs.StringVar(&b.prefix, "prefix", "", "write, list-objects: object name prefix")
	fs.BoolVar(&b.cleanup, "cleanup", true, "write: delete the written objects afterwards")
	out := fs.String("out", "results", "output directory")
	fs.Parse(os.Args[1:])

	if b.engine != "go" && b.engine != "rust" {
		log.Fatalf("--engine must be go or rust, got %q", b.engine)
	}
	if b.workload == "" {
		log.Fatal("--workload is required")
	}
	if b.bucket == "" && b.workload != "list-buckets" {
		log.Fatal("--bucket is required")
	}
	b.json = *transport == "json"
	heavy := b.workload == "read" || b.workload == "write"
	b.threads, b.durationS, b.warmupS = *threads, *duration, *warmup
	if b.threads == 0 {
		b.threads = map[bool]int{true: 48, false: 1}[heavy]
	}
	if b.durationS == 0 {
		b.durationS = map[bool]int{true: 300, false: 180}[heavy]
	}
	if b.warmupS < 0 {
		b.warmupS = map[bool]int{true: 30, false: 10}[heavy]
	}
	if b.label == "" {
		b.label = b.engine + "-default"
	}
	if b.bucketLabel == "" {
		b.bucketLabel = b.bucket
	}
	mode := b.mode
	if b.workload == "open" {
		mode = strconv.FormatInt(b.openSize, 10)
	}
	if mode == "" {
		mode = "x"
	}
	run := strings.Join([]string{b.workload, map[bool]string{true: "json", false: "bidi"}[b.json], mode, b.label,
		strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339Nano), ":", "-")}, "-")
	b.out = filepath.Join(*out, run)
	if err := os.MkdirAll(b.out, 0o755); err != nil {
		log.Fatal(err)
	}
	// The Rust SDK has no DirectPath: keep the Go gRPC client on CloudPath too.
	os.Setenv("GOOGLE_CLOUD_DISABLE_DIRECT_PATH", "true")

	ctx := context.Background()
	var err error
	if b.client, err = b.newClient(ctx); err != nil {
		log.Fatalf("creating the client: %v", err)
	}
	defer b.client.Close()
	if err := b.run(ctx); err != nil {
		log.Fatal(err)
	}
}

// newClient creates the client under test: the same options on both engines, except the engine.
func (b *bench) newClient(ctx context.Context) (*storage.Client, error) {
	var opts []option.ClientOption
	if b.engine == "rust" {
		opts = append(opts, experimental.WithRustCore())
	}
	if b.json {
		return storage.NewClient(ctx, append(opts, storage.WithJSONReads())...)
	}
	// Go's built-in gRPC client metrics are off: the Rust core has none, so leaving them on would
	// add a cost to the Go runs only.
	opts = append(opts, storage.WithDisabledClientMetrics())
	if b.engine == "go" {
		// BidiReadObject reads, the RPC the Rust SDK reads with (Go's default is ReadObject).
		opts = append(opts, storage.WithGRPCBidiReads())
	}
	return storage.NewGRPCClient(ctx, opts...)
}

func (b *bench) run(ctx context.Context) error {
	switch b.workload {
	case "open":
		return b.runOpen(ctx)
	case "read50x2x4":
		return b.runRead50x2x4(ctx)
	case "random8k":
		return b.runRandom8k(ctx)
	case "read":
		return b.runRead(ctx)
	case "write":
		return b.runWrite(ctx)
	case "list-objects":
		return b.runListObjects(ctx)
	case "list-buckets":
		return b.runListBuckets(ctx)
	}
	return fmt.Errorf("unknown --workload=%s", b.workload)
}

// op is one measured operation; it returns the bytes (or items) it handled.
type op func(ctx context.Context) (int64, error)

// ---- Workloads 1-3 (one operation at a time) ---------------------------------------------------

// runOpen: 1. Open: open a Bidi read stream, wait for the first response (object metadata), close.
// Over gRPC, NewRangeReader(0, 0) asks for the whole object and closes the stream after the first
// response (in both engines), so that response can carry data: --open-size changes the object.
func (b *bench) runOpen(ctx context.Context) error {
	o, err := b.ensureObject(ctx, b.objectOr(fmt.Sprintf("rapid/read-%d", b.openSize)), b.openSize)
	if err != nil {
		return err
	}
	name := "OPEN"
	if b.openSize != 100*mib {
		name = "OPEN_" + strings.ToUpper(sizeLabel(b.openSize))
	}
	return b.runTimed(ctx, name, opTimeout, func(int) (op, error) {
		return func(ctx context.Context) (int64, error) {
			r, err := o.NewRangeReader(ctx, 0, 0)
			if err != nil {
				return 0, err
			}
			return 1, r.Close()
		}, nil
	}, map[string]any{"object": o.ObjectName()}, nil)
}

// runRead50x2x4: 2. Read50x2x4: 50 reads of 2 MiB at a 4 MiB stride from a 200 MiB object. One
// operation = all 50 reads, all at once (concurrent) or one after another (serial), one range
// reader per range.
func (b *bench) runRead50x2x4(ctx context.Context) error {
	const n, length, stride = 50, 2 * mib, 4 * mib
	if b.mode == "" {
		b.mode = "concurrent"
	}
	concurrent := b.mode == "concurrent"
	o, err := b.ensureObject(ctx, b.objectOr(fmt.Sprintf("rapid/read-%d", 200*mib)), 200*mib)
	if err != nil {
		return err
	}
	params := map[string]any{"object": o.ObjectName(), "mode": b.mode, "ranges": n, "range_size": length, "stride": stride}
	name := "READ50X2X4_" + map[bool]string{true: "JSON", false: "BIDI"}[b.json] + "_" + strings.ToUpper(b.mode)
	return b.runTimed(ctx, name, opTimeout, func(int) (op, error) {
		bufs := make([][]byte, n)
		for i := range bufs {
			bufs[i] = make([]byte, length)
		}
		return func(ctx context.Context) (int64, error) {
			if !concurrent {
				var total int64
				for i := range n {
					got, err := readRange(ctx, o, int64(i)*stride, bufs[i])
					if err != nil {
						return 0, err
					}
					total += got
				}
				return total, nil
			}
			var wg sync.WaitGroup
			got := make([]int64, n)
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got[i], errs[i] = readRange(ctx, o, int64(i)*stride, bufs[i])
				}()
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				return 0, err
			}
			var total int64
			for _, g := range got {
				total += g
			}
			return total, nil
		}, nil
	}, params, nil)
}

// runRandom8k: 3. Random8K: random 8 KiB reads from a 100 MiB object, one range reader per read.
func (b *bench) runRandom8k(ctx context.Context) error {
	const length = 8 << 10
	o, err := b.ensureObject(ctx, b.objectOr(fmt.Sprintf("rapid/read-%d", 100*mib)), 100*mib)
	if err != nil {
		return err
	}
	slots := int64(100*mib) / length
	name := "RANDOM8K_" + map[bool]string{true: "JSON", false: "BIDI"}[b.json]
	return b.runTimed(ctx, name, opTimeout, func(thread int) (op, error) {
		rnd := rand.New(rand.NewPCG(uint64(thread), 8))
		buf := make([]byte, length)
		return func(ctx context.Context) (int64, error) {
			return readRange(ctx, o, rnd.Int64N(slots)*length, buf)
		}, nil
	}, map[string]any{"object": o.ObjectName(), "range_size": length}, nil)
}

// ---- Workload 4: 16 MiB reads of a 200 MB object, 48 workers ------------------------------------

func (b *bench) runRead(ctx context.Context) error {
	o, err := b.ensureObject(ctx, b.objectOr(fmt.Sprintf("rapid/read-%d", b.objectSize)), b.objectSize)
	if err != nil {
		return err
	}
	ranges := b.objectSize / int64(b.rangeSize)
	if ranges < 1 {
		return fmt.Errorf("%s is smaller than --range-size", o.ObjectName())
	}
	name := map[bool]string{true: "READ_JSON", false: "BIDI_READ"}[b.json]
	params := map[string]any{"object": o.ObjectName(), "object_size": b.objectSize, "range_size": b.rangeSize}
	return b.runTimed(ctx, name, opTimeout, func(thread int) (op, error) {
		rnd := rand.New(rand.NewPCG(uint64(thread), 4))
		buf := make([]byte, b.rangeSize)
		return func(ctx context.Context) (int64, error) {
			return readRange(ctx, o, rnd.Int64N(ranges)*int64(b.rangeSize), buf)
		}, nil
	}, params, nil)
}

// ---- Workload 5: 200 MB writes, 48 workers ------------------------------------------------------

// runWrite: each worker keeps overwriting its own object name, so the bytes stored stay at about
// workers x object size (a zonal bucket counts stored bytes against a per-zone quota).
func (b *bench) runWrite(ctx context.Context) error {
	payload := randomPayload(b.objectSize)
	prefix := b.prefix
	if prefix == "" {
		prefix = "rapid/" + map[bool]string{true: "jsonwrite", false: "write"}[b.json] + "-" + b.label + "/"
	}
	bkt := b.client.Bucket(b.bucket)
	name := map[bool]string{true: "WRITE_JSON", false: "BIDI_APPENDABLE_WRITE"}[b.json]
	params := map[string]any{"object_size": b.objectSize, "write_chunk": b.writeChunk, "prefix": prefix}
	err := b.runTimed(ctx, name, writeTimeout, func(thread int) (op, error) {
		obj := bkt.Object(prefix + strconv.Itoa(thread))
		return func(ctx context.Context) (int64, error) {
			w := obj.NewWriter(ctx)
			if !b.json {
				w.Append, w.FinalizeOnClose = true, true
			}
			for off := 0; off < len(payload); off += b.writeChunk {
				n, err := w.Write(payload[off:min(off+b.writeChunk, len(payload))])
				b.bytesWritten.Add(int64(n))
				if err != nil {
					w.Close()
					return 0, err
				}
			}
			if err := w.Close(); err != nil {
				return 0, err
			}
			if a := w.Attrs(); a != nil {
				return a.Size, nil
			}
			return int64(len(payload)), nil
		}, nil
	}, params, nil)
	if b.cleanup {
		for t := range b.threads {
			dctx, cancel := context.WithTimeout(ctx, opTimeout)
			if err := bkt.Object(prefix + strconv.Itoa(t)).Delete(dctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
				log.Printf("cleanup %s%d: %v", prefix, t, err)
			}
			cancel()
		}
	}
	return err
}

// ---- Workload 6: List Objects / List Buckets ----------------------------------------------------

func (b *bench) runListObjects(ctx context.Context) error {
	prefix := b.prefix
	if prefix == "" {
		prefix = fmt.Sprintf("rapid/list-%d/", b.listItems)
	}
	bkt := b.client.Bucket(b.bucket)
	have := map[string]bool{}
	it := bkt.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		have[a.Name] = true
	}
	for i := range b.listItems {
		if name := fmt.Sprintf("%s%05d", prefix, i); !have[name] {
			if _, err := b.ensureObject(ctx, name, 1); err != nil {
				return err
			}
		}
	}
	return b.runTimed(ctx, "LIST_OBJECTS", opTimeout, func(int) (op, error) {
		return func(ctx context.Context) (int64, error) {
			var page []*storage.ObjectAttrs
			if _, err := iterator.NewPager(bkt.Objects(ctx, &storage.Query{Prefix: prefix}), b.listItems, "").NextPage(&page); err != nil {
				return 0, err
			}
			if len(page) != b.listItems {
				return 0, fmt.Errorf("listed %d objects, expected %d", len(page), b.listItems)
			}
			return int64(len(page)), nil
		}, nil
	}, map[string]any{"prefix": prefix, "page_size": b.listItems}, nil)
}

func (b *bench) runListBuckets(ctx context.Context) error {
	project := b.project
	if project == "" && metadata.OnGCE() {
		project, _ = metadata.ProjectIDWithContext(ctx)
	}
	if project == "" {
		return errors.New("--project is required off GCE")
	}
	return b.runTimed(ctx, "LIST_BUCKETS", opTimeout, func(int) (op, error) {
		return func(ctx context.Context) (int64, error) {
			var page []*storage.BucketAttrs
			if _, err := iterator.NewPager(b.client.Buckets(ctx, project), b.listItems, "").NextPage(&page); err != nil {
				return 0, err
			}
			return int64(len(page)), nil
		}, nil
	}, map[string]any{"page_size": b.listItems}, nil)
}

// ---- Helpers ------------------------------------------------------------------------------------

// readRange reads [off, off+len(buf)) with one range reader and returns the bytes read.
func readRange(ctx context.Context, o *storage.ObjectHandle, off int64, buf []byte) (int64, error) {
	r, err := o.NewRangeReader(ctx, off, int64(len(buf)))
	if err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r, buf)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("read %s at %d: %d of %d bytes: %w", o.ObjectName(), off, n, len(buf), err)
	}
	return int64(n), nil
}

func (b *bench) objectOr(def string) string {
	if b.objectName != "" {
		return b.objectName
	}
	return def
}

// sizeLabel is a byte count as "100KiB", "2MiB" or "512B".
func sizeLabel(n int64) string {
	switch {
	case n >= mib && n%mib == 0:
		return fmt.Sprintf("%dMiB", n/mib)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return fmt.Sprintf("%dB", n)
}

// ensureObject returns the object, creating it (random data) if it is missing or has another size.
func (b *bench) ensureObject(ctx context.Context, name string, size int64) (*storage.ObjectHandle, error) {
	o := b.client.Bucket(b.bucket).Object(name)
	if a, err := o.Attrs(ctx); err == nil && a.Size == size {
		return o, nil
	}
	log.Printf("creating %s (%d bytes)", name, size)
	w := o.NewWriter(ctx)
	if _, err := w.Write(randomPayload(size)); err != nil {
		w.Close()
		return nil, fmt.Errorf("creating %s: %w", name, err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("creating %s: %w", name, err)
	}
	return o, nil
}

// randomPayload returns size bytes: one seeded random MiB, repeated (as the Java benchmark).
func randomPayload(size int64) []byte {
	block := make([]byte, mib)
	rand.NewChaCha8([32]byte{42}).Read(block)
	b := make([]byte, size)
	for off := 0; off < len(b); off += mib {
		copy(b[off:], block)
	}
	return b
}

// ---- Timed runner and metrics -------------------------------------------------------------------

// opLog is one worker's log: end time and latency of every operation, the bytes or items it
// handled, and its errors.
type opLog struct {
	end, lat, amount []int64 // end: ns since t0
	errors           int64
	firstErr         string
}

func (b *bench) runTimed(ctx context.Context, name string, timeout time.Duration, factory func(thread int) (op, error),
	params map[string]any, before func()) error {
	logs := make([]*opLog, b.threads)
	var stop atomic.Bool
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	var t0 time.Time
	for t := range b.threads {
		logs[t] = &opLog{}
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			l := logs[t]
			o, err := factory(t)
			ready.Done()
			if err != nil {
				l.firstErr = "setup: " + err.Error()
				return
			}
			<-start
			for !stop.Load() {
				s := time.Now()
				octx, cancel := context.WithTimeout(ctx, timeout)
				amount, err := o(octx)
				cancel()
				e := time.Now()
				if err != nil {
					l.errors++
					if l.firstErr == "" {
						l.firstErr = err.Error()
						log.Printf("worker %d: %v", t, err)
					}
					time.Sleep(10 * time.Millisecond) // no hot loop on a persistent failure
					continue
				}
				l.end = append(l.end, int64(e.Sub(t0)))
				l.lat = append(l.lat, int64(e.Sub(s)))
				l.amount = append(l.amount, amount)
			}
		}()
	}
	ready.Wait()
	t0 = time.Now()
	close(start)
	log.Printf("%s on %s: %d workers, %d s warm-up + %d s measured", name, b.engine, b.threads, b.warmupS, b.durationS)
	time.Sleep(time.Duration(b.warmupS) * time.Second)
	measureStart := time.Now()
	cpuStart := cpuSeconds()
	writtenStart := b.bytesWritten.Load()
	nicStart := nicBytes()
	tcpStart := established443()
	time.Sleep(time.Duration(b.durationS) * time.Second / 2)
	tcpMid := established443()
	time.Sleep(time.Until(measureStart.Add(time.Duration(b.durationS) * time.Second)))
	measureEnd := time.Now()
	writtenEnd := b.bytesWritten.Load()
	nicEnd := nicBytes()
	tcpEnd := established443()
	secs := measureEnd.Sub(measureStart).Seconds()
	cpuPct := 100 * (cpuSeconds() - cpuStart) / secs / float64(runtime.NumCPU())
	stop.Store(true)
	done.Wait()
	if before != nil {
		before()
	}
	params["network"] = map[string]any{
		"tcp_established_443_start": tcpStart,
		"tcp_established_443_mid":   tcpMid,
		"tcp_established_443_end":   tcpEnd,
		"nic_rx_Gbps":               float64(nicEnd[0]-nicStart[0]) * 8 / secs / 1e9,
		"nic_tx_Gbps":               float64(nicEnd[1]-nicStart[1]) * 8 / secs / 1e9,
	}
	if name == "BIDI_APPENDABLE_WRITE" || name == "WRITE_JSON" {
		b.windowBytesWritten = writtenEnd - writtenStart
	}
	return b.report(name, logs, int64(measureStart.Sub(t0)), int64(measureEnd.Sub(t0)), measureStart, measureEnd, cpuPct, params)
}

func (b *bench) report(name string, logs []*opLog, mStart, mEnd int64, wStart, wEnd time.Time, cpuPct float64,
	params map[string]any) error {
	var lat []int64
	var amount, errorsN int64
	firstErr := ""
	perSecond := map[int64][2]int64{}
	opsCSV, err := os.Create(filepath.Join(b.out, "ops.csv"))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(opsCSV)
	fmt.Fprintln(w, "thread,end_ms,latency_ms,amount,measured")
	for t, l := range logs {
		errorsN += l.errors
		if firstErr == "" {
			firstErr = l.firstErr
		}
		for i := range l.end {
			measured := l.end[i] >= mStart && l.end[i] <= mEnd
			fmt.Fprintf(w, "%d,%.3f,%.3f,%d,%t\n", t, float64(l.end[i])/1e6, float64(l.lat[i])/1e6, l.amount[i], measured)
			s := perSecond[l.end[i]/int64(time.Second)]
			s[0]++
			s[1] += l.amount[i]
			perSecond[l.end[i]/int64(time.Second)] = s
			if measured {
				lat = append(lat, l.lat[i])
				amount += l.amount[i]
			}
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	opsCSV.Close()
	tl, err := os.Create(filepath.Join(b.out, "timeline.csv"))
	if err != nil {
		return err
	}
	fmt.Fprintln(tl, "second,ops,amount")
	secsKeys := make([]int64, 0, len(perSecond))
	for k := range perSecond {
		secsKeys = append(secsKeys, k)
	}
	slices.Sort(secsKeys)
	for _, k := range secsKeys {
		fmt.Fprintf(tl, "%d,%d,%d\n", k, perSecond[k][0], perSecond[k][1])
	}
	tl.Close()

	slices.Sort(lat)
	secs := float64(mEnd-mStart) / 1e9
	isList := strings.HasPrefix(name, "LIST") || strings.HasPrefix(name, "OPEN")
	s := orderedMap{}
	s.put("op", name)
	s.put("engine", b.engine)
	s.put("transport", map[bool]string{true: "json", false: "bidi"}[b.json])
	s.put("label", b.label)
	s.put("bucket", b.bucketLabel)
	s.put("threads", b.threads)
	s.put("clients", 1)
	s.put("grpc_connections", "client default (1)")
	s.put("params", params)
	s.put("window_start", wStart.UTC().Format(timeLayout))
	s.put("window_end", wEnd.UTC().Format(timeLayout))
	s.put("measured_seconds", secs)
	s.put("ops", len(lat))
	s.put("errors", errorsN)
	if firstErr != "" {
		s.put("first_error", firstErr)
	} else {
		s.put("first_error", nil)
	}
	s.put("latency_ms_p50", pct(lat, 50))
	s.put("latency_ms_p90", pct(lat, 90))
	s.put("latency_ms_p99", pct(lat, 99))
	s.put("latency_ms_p999", pct(lat, 99.9))
	maxLat, meanLat := 0.0, 0.0
	if len(lat) > 0 {
		maxLat = float64(lat[len(lat)-1]) / 1e6
		var sum float64
		for _, v := range lat {
			sum += float64(v)
		}
		meanLat = sum / float64(len(lat)) / 1e6
	}
	s.put("latency_ms_max", maxLat)
	s.put("latency_ms_mean", meanLat)
	s.put("ops_per_sec", float64(len(lat))/secs)
	switch {
	case isList:
		s.put("items_per_sec", float64(amount)/secs)
	case b.windowBytesWritten >= 0:
		s.put("bytes", b.windowBytesWritten)
		s.put("throughput_MBps", float64(b.windowBytesWritten)/secs/1e6)
		s.put("throughput_Gbps", float64(b.windowBytesWritten)*8/secs/1e9)
		s.put("completed_objects_Gbps", float64(amount)*8/secs/1e9)
	default:
		s.put("bytes", amount)
		s.put("throughput_MBps", float64(amount)/secs/1e6)
		s.put("throughput_Gbps", float64(amount)*8/secs/1e9)
	}
	s.put("process_cpu_percent_of_all_cores", cpuPct)
	s.put("process_peak_rss_mib", float64(procStatusKB("VmHWM"))/1024)
	s.put("available_processors", runtime.NumCPU())
	s.put("go_version", runtime.Version())
	for _, kv := range gceInstance() {
		s.put(kv[0], kv[1])
	}
	js, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(b.out, "summary.json"), append(js, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(string(js))
	fmt.Println("results:", b.out)
	return nil
}

// orderedMap marshals its keys in insertion order (as the Java benchmark's summary.json), with
// floats rounded to 3 decimals.
type orderedMap []struct {
	k string
	v any
}

func (m *orderedMap) put(k string, v any) {
	*m = append(*m, struct {
		k string
		v any
	}{k, v})
}

func (m orderedMap) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, kv := range m {
		if i > 0 {
			sb.WriteByte(',')
		}
		k, _ := json.Marshal(kv.k)
		sb.Write(k)
		sb.WriteByte(':')
		v, err := json.Marshal(round3(kv.v))
		if err != nil {
			return nil, err
		}
		sb.Write(v)
	}
	sb.WriteByte('}')
	return []byte(sb.String()), nil
}

func round3(v any) any {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return math.Round(x*1000) / 1000
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = round3(e)
		}
		return out
	}
	return v
}

// pct is the nearest-rank percentile of sorted latencies, in ms.
func pct(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return float64(sorted[max(0, min(len(sorted)-1, i))]) / 1e6
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return tv(ru.Utime) + tv(ru.Stime)
}

func procStatusKB(key string) int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, key+":"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n
			}
		}
	}
	return -1
}

// established443 counts this process's established TCP connections to remote port 443 (the
// storage endpoint), from /proc/self/fd and /proc/net/tcp{,6}: how many connections an engine
// really opened. -1 if unavailable.
func established443() int {
	mine := map[string]bool{}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	for _, fd := range fds {
		if t, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && strings.HasPrefix(t, "socket:[") {
			mine[strings.TrimSuffix(strings.TrimPrefix(t, "socket:["), "]")] = true
		}
	}
	n := 0
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			return -1
		}
		for _, line := range strings.Split(string(b), "\n") {
			// c[2] = remote "ADDR:PORT" in hex, c[3] = state (01 = ESTABLISHED), c[9] = inode.
			if c := strings.Fields(line); len(c) > 9 && strings.HasSuffix(c[2], ":01BB") && c[3] == "01" && mine[c[9]] {
				n++
			}
		}
	}
	return n
}

// nicBytes returns the total [rx, tx] bytes of all non-loopback interfaces, from /proc/net/dev.
func nicBytes() [2]int64 {
	var out [2]int64
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		if c := strings.Fields(rest); len(c) > 8 {
			rx, _ := strconv.ParseInt(c[0], 10, 64)
			tx, _ := strconv.ParseInt(c[8], 10, 64)
			out[0] += rx
			out[1] += tx
		}
	}
	return out
}

// gceInstance is the VM's identity, used to fetch its Cloud Monitoring metrics afterwards.
func gceInstance() [][2]string {
	if !metadata.OnGCE() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	get := func(p string) string { v, _ := metadata.GetWithContext(ctx, p); return v }
	last := func(s string) string { return s[strings.LastIndex(s, "/")+1:] }
	return [][2]string{
		{"gce_instance_id", get("instance/id")},
		{"gce_instance_name", get("instance/name")},
		{"gce_zone", last(get("instance/zone"))},
		{"gce_machine_type", last(get("instance/machine-type"))},
		{"gce_project", get("project/project-id")},
	}
}
