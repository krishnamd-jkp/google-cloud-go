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

//go:build linux

// Command gpucopy is a proof of concept for "direct copy of data into GPU buffers": it reads
// object data through the public Go storage API, on the Rust core (--engine=rust,
// experimental.WithRustCore) or on the Go SDK's own transports (--engine=go), and hands each range
// to a GPU.
//
// Each of --workers workers reads random --range-size ranges of --object (NewRangeReader and
// io.ReadFull) for --warmup plus --duration:
//
//	--mode=host      into a Go heap buffer, with no device copy: the network-only baseline
//	--mode=pageable  into a Go heap buffer, then a blocking host-to-device copy. CUDA copies
//	                 pageable memory into a pinned staging buffer first, so this costs one more
//	                 CPU copy of every byte.
//	--mode=pinned    straight into page-locked host memory allocated outside the Go heap (two
//	                 buffers per worker), then an asynchronous DMA that overlaps the next read
//
// The Rust core copies each range once, from the Rust SDK's receive buffers into the buffer passed
// to Reader.Read (gcs_reader_read). With --mode=pinned that buffer is the DMA source, so the data
// takes one CPU copy and one DMA between the SDK and the GPU. runtime.Pinner, which the binding
// uses for Go buffers, does nothing for memory outside the Go heap.
//
// --device=cuda uses the first GPU through the CUDA driver API. libcuda.so.1 is loaded at run time,
// so building needs no CUDA toolkit. --device=sim needs no GPU and keeps only the CPU work of each
// copy: pinned memory is anonymous memory that is mmap'ed, pre-faulted and mlock'ed; a pageable copy
// is a memcpy into a pinned staging buffer; a pinned copy costs no CPU, since it would be a DMA.
//
// --verify=N checks every Nth range of each worker, as read back from the device, against the
// object's known content. The object must hold rapidbench's payload: one ChaCha8 MiB, repeated.
//
// --workload=copycost measures, without the network, the CPU time per GiB of each hand-off, on one
// thread: a memcpy (the core's copy into the caller's buffer), a Unix domain socket hop (the
// hand-off of a sidecar, both sides), and with --device=cuda the pageable and pinned
// host-to-device copies.
//
// Results are appended to --out, one JSON object per run.
//
// Build (cgo, linux/amd64; build the Rust core first with ../../build.sh):
//
//	go build -tags gcsrust -o gpucopy ./internal/rustcore/cmd/gpucopy
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"cloud.google.com/go/storage/experimental"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

const (
	mib       = 1 << 20
	gib       = 1 << 30
	opTimeout = 120 * time.Second
)

type config struct {
	workload, engine, transport, mode, device, label string
	bucket, object, out                              string
	workers, rangeSize, verifyEvery, grpcConns       int
	jsonHTTP1                                        bool
	warmup, duration                                 time.Duration
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("gpucopy: ")
	var c config
	flag.StringVar(&c.workload, "workload", "read", "read (object ranges to the device) or copycost (hand-off costs, no network)")
	flag.StringVar(&c.engine, "engine", "rust", "go (the Go SDK's own transports) or rust (experimental.WithRustCore)")
	flag.StringVar(&c.transport, "transport", "json", "json (HTTP client) or bidi (gRPC client, BidiReadObject)")
	flag.StringVar(&c.mode, "mode", "pinned", "host, pageable or pinned (see the command's doc)")
	flag.StringVar(&c.device, "device", "sim", "sim (no GPU) or cuda (the first GPU, through libcuda.so.1)")
	flag.StringVar(&c.bucket, "bucket", "", "bucket (read)")
	flag.StringVar(&c.object, "object", "rapid/read-200000000", "object to read; it must hold rapidbench's payload for --verify")
	flag.IntVar(&c.workers, "workers", 48, "parallel readers")
	flag.IntVar(&c.rangeSize, "range-size", 16*mib, "bytes per read")
	flag.DurationVar(&c.warmup, "warmup", 10*time.Second, "warm-up, not measured")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "measured window (copycost: per hand-off)")
	flag.IntVar(&c.verifyEvery, "verify", 0, "check every Nth range of each worker against the object's content (0: none)")
	flag.IntVar(&c.grpcConns, "grpc-conns", 0, "bidi: gRPC connections on both engines (option.WithGRPCConnectionPool); 0 = the client default (1)")
	flag.BoolVar(&c.jsonHTTP1, "json-http1", false, "json, engine go: HTTP/1.1 only (one connection per in-flight read, as the Rust core), instead of one HTTP/2 connection")
	flag.StringVar(&c.label, "label", "", "free-form label stored with the result")
	flag.StringVar(&c.out, "out", "gpucopy.jsonl", "results file: one JSON object per run, appended")
	flag.Parse()

	dev, err := openDevice(c.device)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("device: %s", dev.name())
	switch c.workload {
	case "read":
		res, err := runRead(c, dev)
		if err != nil {
			log.Fatal(err)
		}
		appendResult(c.out, res)
	case "copycost":
		for _, res := range runCopyCost(c, dev) {
			appendResult(c.out, res)
		}
	default:
		log.Fatalf("--workload must be read or copycost, got %q", c.workload)
	}
}

func openDevice(name string) (device, error) {
	switch name {
	case "sim":
		return simDevice{}, nil
	case "cuda":
		return openCUDA(0)
	}
	return nil, fmt.Errorf("--device must be sim or cuda, got %q", name)
}

// ---- Devices ------------------------------------------------------------------------------------

// device is where the data goes: a GPU (cudaDevice) or a stand-in for one (simDevice).
type device interface {
	name() string
	// allocPinned returns n bytes of page-locked host memory outside the Go heap, which a GPU can
	// DMA from. locked reports whether the pages are really locked.
	allocPinned(n int) (buf []byte, locked bool, free func(), err error)
	// newQueue returns one worker's copy queue: slots device buffers of n bytes, each with its own
	// stream.
	newQueue(slots, n int) (queue, error)
}

// queue copies host buffers into one worker's device buffers.
type queue interface {
	// copyPageable copies src (any host memory, such as the Go heap) into slot's device buffer and
	// returns once src may be reused.
	copyPageable(slot int, src []byte) error
	// copyPinned starts copying src (memory from allocPinned) into slot's device buffer and returns
	// at once. src must not change until wait(slot) returns.
	copyPinned(slot int, src []byte) error
	// wait blocks until slot's last copy is done.
	wait(slot int) error
	// readBack returns the first len(dst) bytes of slot's device buffer, copied into dst if they are
	// on a GPU.
	readBack(slot int, dst []byte) ([]byte, error)
	close()
}

// simDevice stands in for a GPU when there is none, and keeps only the CPU work of each copy.
// CUDA copies pageable memory by first copying it, on the calling thread, into a pinned staging
// buffer that it then DMAs; it DMAs pinned memory directly, with no CPU work.
type simDevice struct{}

func (simDevice) name() string {
	return "sim (no GPU: pinned = mmap+mlock, pageable copy = memcpy to a pinned staging buffer, pinned copy = DMA, no CPU)"
}

func (simDevice) allocPinned(n int) ([]byte, bool, func(), error) { return mmapLocked(n) }

func (simDevice) newQueue(slots, n int) (queue, error) {
	staging, _, free, err := mmapLocked(n)
	if err != nil {
		return nil, err
	}
	return &simQueue{staging: staging, freeStaging: free, last: make([][]byte, slots)}, nil
}

type simQueue struct {
	staging     []byte
	freeStaging func()
	last        [][]byte // per slot: what the device buffer would hold
}

func (q *simQueue) copyPageable(slot int, src []byte) error {
	n := copy(q.staging, src)
	q.last[slot] = q.staging[:n]
	return nil
}

func (q *simQueue) copyPinned(slot int, src []byte) error {
	q.last[slot] = src
	return nil
}

func (q *simQueue) wait(int) error { return nil }

func (q *simQueue) readBack(slot int, dst []byte) ([]byte, error) {
	if len(q.last[slot]) < len(dst) {
		return nil, fmt.Errorf("slot %d holds %d bytes, want %d", slot, len(q.last[slot]), len(dst))
	}
	return q.last[slot][:len(dst)], nil
}

func (q *simQueue) close() { q.freeStaging() }

// mmapLocked maps n bytes of anonymous memory outside the Go heap, pre-faults it and locks it in
// RAM. If RLIMIT_MEMLOCK doesn't allow the lock, the memory stays unlocked and locked is false.
func mmapLocked(n int) ([]byte, bool, func(), error) {
	b, err := syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS|syscall.MAP_POPULATE)
	if err != nil {
		return nil, false, nil, fmt.Errorf("mmap %d bytes: %w", n, err)
	}
	locked := syscall.Mlock(b) == nil
	return b, locked, func() {
		if locked {
			syscall.Munlock(b)
		}
		syscall.Munmap(b)
	}, nil
}

// ---- Read workload -------------------------------------------------------------------------------

type readRun struct {
	c        config
	dev      device
	obj      *storage.ObjectHandle
	ranges   int64
	block    []byte // the object's repeated MiB
	wStart   time.Time
	wEnd     time.Time
	ops      atomic.Int64
	bytes    atomic.Int64
	readNs   atomic.Int64
	copyNs   atomic.Int64
	verified atomic.Int64
	verBytes atomic.Int64
	bad      atomic.Int64
	errs     atomic.Int64
	locked   atomic.Int64 // pinned buffers that are really locked
	pinned   atomic.Int64 // pinned buffers
	mu       sync.Mutex
	firstErr string
}

func runRead(c config, dev device) (map[string]any, error) {
	if c.bucket == "" {
		return nil, errors.New("--bucket is required")
	}
	if c.mode != "host" && c.mode != "pageable" && c.mode != "pinned" {
		return nil, fmt.Errorf("--mode must be host, pageable or pinned, got %q", c.mode)
	}
	// The Rust SDK has no DirectPath: keep the Go gRPC client on CloudPath too.
	os.Setenv("GOOGLE_CLOUD_DISABLE_DIRECT_PATH", "true")
	ctx := context.Background()
	client, err := newClient(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("creating the client: %w", err)
	}
	defer client.Close()
	r := &readRun{c: c, dev: dev, obj: client.Bucket(c.bucket).Object(c.object), block: payloadBlock()}
	attrs, err := r.obj.Attrs(ctx)
	if err != nil {
		return nil, fmt.Errorf("object %s: %w", c.object, err)
	}
	if r.ranges = attrs.Size / int64(c.rangeSize); r.ranges < 1 {
		return nil, fmt.Errorf("%s (%d bytes) is smaller than --range-size", c.object, attrs.Size)
	}

	start := time.Now()
	r.wStart = start.Add(c.warmup)
	r.wEnd = r.wStart.Add(c.duration)
	runCtx, cancel := context.WithDeadline(ctx, r.wEnd)
	defer cancel()
	var wg sync.WaitGroup
	werrs := make([]error, c.workers)
	for i := range c.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			werrs[i] = r.worker(runCtx, i)
		}()
	}
	time.Sleep(time.Until(r.wStart))
	cpu0, gc0 := cpuSeconds(), goGC()
	time.Sleep(c.duration / 2)
	conns := established443()
	time.Sleep(time.Until(r.wEnd))
	cpu1, gc1 := cpuSeconds(), goGC()
	cancel()
	wg.Wait()
	if err := errors.Join(werrs...); err != nil {
		return nil, err
	}

	ops, n := r.ops.Load(), r.bytes.Load()
	secs := c.duration.Seconds()
	cpu := cpu1 - cpu0
	res := map[string]any{
		"time":            start.UTC().Format(time.RFC3339),
		"workload":        "read",
		"engine":          c.engine,
		"transport":       c.transport,
		"mode":            c.mode,
		"device":          c.device,
		"device_name":     dev.name(),
		"label":           c.label,
		"workers":         c.workers,
		"grpc_conns":      c.grpcConns,
		"json_http1":      c.jsonHTTP1 && c.transport == "json",
		"range_size":      c.rangeSize,
		"object_size":     attrs.Size,
		"warmup_s":        c.warmup.Seconds(),
		"duration_s":      secs,
		"ops":             ops,
		"bytes":           n,
		"gbps":            round3(float64(n) * 8 / secs / 1e9),
		"mib_per_s":       round3(float64(n) / secs / mib),
		"cpu_s":           round3(cpu),
		"cpu_cores":       round3(cpu / secs),
		"cpu_s_per_gib":   round3(perGiB(cpu, n)),
		"go_gc_cycles":    gc1.cycles - gc0.cycles,
		"go_gc_cpu_s":     round3(gc1.cpuS - gc0.cpuS),
		"read_ms_avg":     round3(avgMs(r.readNs.Load(), ops)),
		"copy_ms_avg":     round3(avgMs(r.copyNs.Load(), ops)),
		"verified_ranges": r.verified.Load(),
		"verified_mib":    round3(float64(r.verBytes.Load()) / mib),
		"mismatches":      r.bad.Load(),
		"errors":          r.errs.Load(),
		"first_error":     r.firstErr,
		"conns_443":       conns,
		"rss_peak_mib":    round3(float64(procStatusKB("VmHWM")) / 1024),
		"pinned_buffers":  r.pinned.Load(),
		"pinned_locked":   r.locked.Load(),
		"gomaxprocs":      runtime.GOMAXPROCS(0),
	}
	log.Printf("%s %s %s/%s x%d: %.2f Gb/s, %.3f CPU-s/GiB (%.2f cores), read %.1f ms + device copy %.2f ms per %d MiB range, %d verified (%d mismatches), %d errors, %d connections, %d Go GC cycles (%.2f CPU-s)",
		c.engine, c.transport, c.mode, c.device, c.workers, res["gbps"], res["cpu_s_per_gib"], res["cpu_cores"],
		res["read_ms_avg"], res["copy_ms_avg"], c.rangeSize/mib, r.verified.Load(), r.bad.Load(), r.errs.Load(), conns,
		gc1.cycles-gc0.cycles, gc1.cpuS-gc0.cpuS)
	return res, nil
}

// newClient creates the client under test: the same options on both engines, except the engine
// (as rapidbench does).
func newClient(ctx context.Context, c config) (*storage.Client, error) {
	var opts []option.ClientOption
	switch c.engine {
	case "rust":
		opts = append(opts, experimental.WithRustCore())
	case "go":
	default:
		return nil, fmt.Errorf("--engine must be go or rust, got %q", c.engine)
	}
	switch c.transport {
	case "json":
		if c.jsonHTTP1 && c.engine == "go" {
			// A non-nil, empty TLSNextProto keeps net/http from speaking HTTP/2, and this
			// transport is not handed to x/net/http2 (the default storage transport is, so
			// GODEBUG=http2client=0 has no effect on it). Clone() copies DefaultTransport's TLS
			// config, whose ALPN offers h2 once DefaultTransport is set up (Clone does that), so
			// offer only http/1.1: otherwise the server picks h2 and the HTTP/1 transport fails.
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.MaxIdleConnsPerHost = 100 // as the default storage transport
			base.ForceAttemptHTTP2 = false
			base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			if base.TLSClientConfig == nil {
				base.TLSClientConfig = &tls.Config{}
			}
			base.TLSClientConfig.NextProtos = []string{"http/1.1"}
			rt, err := htransport.NewTransport(ctx, base,
				option.WithScopes(storage.ScopeFullControl, "https://www.googleapis.com/auth/cloud-platform"))
			if err != nil {
				return nil, err
			}
			opts = append(opts, option.WithHTTPClient(&http.Client{Transport: rt}))
		}
		return storage.NewClient(ctx, append(opts, storage.WithJSONReads())...)
	case "bidi":
		opts = append(opts, storage.WithDisabledClientMetrics())
		if c.grpcConns > 0 {
			// The Rust core opens the same number (with_grpc_subchannel_count).
			opts = append(opts, option.WithGRPCConnectionPool(c.grpcConns))
		}
		if c.engine == "go" {
			// BidiReadObject reads, the RPC the Rust SDK reads with (Go's default is ReadObject).
			opts = append(opts, storage.WithGRPCBidiReads())
		}
		return storage.NewGRPCClient(ctx, opts...)
	}
	return nil, fmt.Errorf("--transport must be json or bidi, got %q", c.transport)
}

func (r *readRun) worker(ctx context.Context, id int) error {
	slots := 1
	if r.c.mode == "pinned" {
		slots = 2 // read into one buffer while the other one's DMA runs
	}
	bufs := make([][]byte, slots)
	for s := range bufs {
		if r.c.mode != "pinned" {
			bufs[s] = make([]byte, r.c.rangeSize)
			continue
		}
		b, locked, free, err := r.dev.allocPinned(r.c.rangeSize)
		if err != nil {
			return err
		}
		defer free()
		bufs[s] = b
		r.pinned.Add(1)
		if locked {
			r.locked.Add(1)
		}
	}
	var q queue
	if r.c.mode != "host" {
		var err error
		if q, err = r.dev.newQueue(slots, r.c.rangeSize); err != nil {
			return err
		}
		defer q.close()
	}
	var vbuf []byte
	if r.c.verifyEvery > 0 {
		vbuf = make([]byte, r.c.rangeSize)
	}
	rnd := rand.New(rand.NewPCG(uint64(id), 16))
	for i := 0; ; i++ {
		slot := i % slots
		buf := bufs[slot]
		if r.c.mode == "pinned" {
			// This buffer's previous DMA (two ranges ago) must be done before it is overwritten.
			if err := q.wait(slot); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			break
		}
		off := rnd.Int64N(r.ranges) * int64(r.c.rangeSize)
		t0 := time.Now()
		n, err := readRange(ctx, r.obj, off, buf)
		t1 := time.Now()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.addError(err)
			continue
		}
		switch r.c.mode {
		case "pageable":
			err = q.copyPageable(slot, buf[:n])
		case "pinned":
			err = q.copyPinned(slot, buf[:n])
		}
		if err != nil {
			return err
		}
		t2 := time.Now()
		if r.c.verifyEvery > 0 && i%r.c.verifyEvery == 0 {
			got := buf[:n]
			if q != nil {
				if err := q.wait(slot); err != nil {
					return err
				}
				if got, err = q.readBack(slot, vbuf[:n]); err != nil {
					return err
				}
			}
			r.verify(off, got)
		}
		if !t2.Before(r.wStart) && !t2.After(r.wEnd) {
			r.ops.Add(1)
			r.bytes.Add(int64(n))
			r.readNs.Add(int64(t1.Sub(t0)))
			r.copyNs.Add(int64(t2.Sub(t1)))
		}
	}
	if q != nil {
		for s := range slots {
			if err := q.wait(s); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *readRun) addError(err error) {
	r.errs.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstErr == "" {
		r.firstErr = err.Error()
		log.Printf("first error: %v", err)
	}
}

// verify checks got, read from the object at off, against the object's content.
func (r *readRun) verify(off int64, got []byte) {
	r.verified.Add(1)
	r.verBytes.Add(int64(len(got)))
	if at := mismatch(got, off, r.block); at >= 0 {
		if r.bad.Add(1) == 1 {
			log.Printf("MISMATCH: range at %d differs at byte %d", off, at)
		}
	}
}

// mismatch returns the index of the first byte of b (read at object offset off) that differs from
// the payload (block repeated), or -1.
func mismatch(b []byte, off int64, block []byte) int {
	for i := 0; i < len(b); {
		bo := int((off + int64(i)) % int64(len(block)))
		n := min(len(b)-i, len(block)-bo)
		if !bytes.Equal(b[i:i+n], block[bo:bo+n]) {
			for j := range n {
				if b[i+j] != block[bo+j] {
					return i + j
				}
			}
		}
		i += n
	}
	return -1
}

// payloadBlock is the MiB that rapidbench's objects repeat.
func payloadBlock() []byte {
	b := make([]byte, mib)
	rand.NewChaCha8([32]byte{42}).Read(b)
	return b
}

// readRange reads [off, off+len(buf)) with one range reader and returns the bytes read.
func readRange(ctx context.Context, o *storage.ObjectHandle, off int64, buf []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	rd, err := o.NewRangeReader(ctx, off, int64(len(buf)))
	if err != nil {
		return 0, err
	}
	n, err := io.ReadFull(rd, buf)
	if cerr := rd.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, fmt.Errorf("read %s at %d: %d of %d bytes: %w", o.ObjectName(), off, n, len(buf), err)
	}
	return n, nil
}

// ---- Copy-cost workload --------------------------------------------------------------------------

// runCopyCost measures each hand-off on one thread for --duration: wall-clock bandwidth and the
// process's CPU time per GiB.
func runCopyCost(c config, dev device) []map[string]any {
	n := c.rangeSize
	heapSrc, heapDst := make([]byte, n), make([]byte, n)
	rand.NewChaCha8([32]byte{7}).Read(heapSrc)
	pinned, locked, freePinned, err := dev.allocPinned(n)
	if err != nil {
		log.Fatal(err)
	}
	defer freePinned()
	copy(pinned, heapSrc)

	type handoff struct {
		name, what string
		run        func(until time.Time) (int64, error)
	}
	loop := func(f func() error) func(until time.Time) (int64, error) {
		return func(until time.Time) (int64, error) {
			var total int64
			for time.Now().Before(until) {
				if err := f(); err != nil {
					return total, err
				}
				total += int64(n)
			}
			return total, nil
		}
	}
	handoffs := []handoff{
		{"memcpy-heap", "copy a range between two Go heap buffers: the Rust core's copy into the caller's buffer",
			loop(func() error { copy(heapDst, heapSrc); return nil })},
		{"memcpy-to-pinned", "copy a range from the Go heap into pinned memory: the CPU part of a pageable host-to-device copy",
			loop(func() error { copy(pinned, heapSrc); return nil })},
		{"uds-default", "send a range through a Unix domain socket pair, default socket buffers: a sidecar's hand-off, both sides",
			func(until time.Time) (int64, error) { return udsHop(heapSrc, heapDst, 0, until) }},
		{"uds-4mib", "the same with 4 MiB socket buffers",
			func(until time.Time) (int64, error) { return udsHop(heapSrc, heapDst, 4*mib, until) }},
	}
	if c.device == "cuda" {
		q, err := dev.newQueue(1, n)
		if err != nil {
			log.Fatal(err)
		}
		defer q.close()
		handoffs = append(handoffs,
			handoff{"h2d-pageable", "blocking host-to-device copy from a Go heap buffer",
				loop(func() error { return q.copyPageable(0, heapSrc) })},
			handoff{"h2d-pinned", "asynchronous host-to-device copy from pinned memory, then a stream sync",
				loop(func() error {
					if err := q.copyPinned(0, pinned); err != nil {
						return err
					}
					return q.wait(0)
				})},
		)
	}
	var out []map[string]any
	for _, h := range handoffs {
		runtime.GC()
		t0, cpu0 := time.Now(), cpuSeconds()
		total, err := h.run(t0.Add(c.duration))
		secs, cpu := time.Since(t0).Seconds(), cpuSeconds()-cpu0
		res := map[string]any{
			"time":          t0.UTC().Format(time.RFC3339),
			"workload":      "copycost",
			"handoff":       h.name,
			"what":          h.what,
			"device":        c.device,
			"device_name":   dev.name(),
			"label":         c.label,
			"chunk":         n,
			"seconds":       round3(secs),
			"bytes":         total,
			"gb_per_s":      round3(float64(total) / secs / 1e9),
			"cpu_s":         round3(cpu),
			"cpu_s_per_gib": round3(perGiB(cpu, total)),
			"pinned_locked": locked,
		}
		if err != nil {
			res["error"] = err.Error()
		}
		log.Printf("%-16s %6.2f GB/s  %.4f CPU-s/GiB", h.name, res["gb_per_s"], res["cpu_s_per_gib"])
		out = append(out, res)
	}
	return out
}

// udsHop streams src through a connected pair of Unix domain sockets until the deadline, one
// goroutine per side, and returns the bytes received (into dst, round robin). sockBuf > 0 sets
// SO_SNDBUF and SO_RCVBUF.
func udsHop(src, dst []byte, sockBuf int, until time.Time) (int64, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if sockBuf > 0 {
		syscall.SetsockoptInt(fds[0], syscall.SOL_SOCKET, syscall.SO_SNDBUF, sockBuf)
		syscall.SetsockoptInt(fds[1], syscall.SOL_SOCKET, syscall.SO_RCVBUF, sockBuf)
	}
	werr := make(chan error, 1)
	go func() {
		defer syscall.Shutdown(fds[0], syscall.SHUT_WR)
		for time.Now().Before(until) {
			for off := 0; off < len(src); {
				n, err := syscall.Write(fds[0], src[off:])
				if err == syscall.EINTR {
					continue
				}
				if err != nil {
					werr <- err
					return
				}
				off += n
			}
		}
		werr <- nil
	}()
	var got int64
	for pos := 0; ; {
		n, err := syscall.Read(fds[1], dst[pos:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return got, err
		}
		if n == 0 {
			break
		}
		got += int64(n)
		if pos += n; pos == len(dst) {
			pos = 0
		}
	}
	return got, <-werr
}

// ---- Helpers ------------------------------------------------------------------------------------

func appendResult(path string, res map[string]any) {
	b, err := json.Marshal(res)
	if err != nil {
		log.Fatal(err)
	}
	if dir := filepath.Dir(path); dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Fatal(err)
	}
}

func perGiB(cpu float64, n int64) float64 {
	if n == 0 {
		return 0
	}
	return cpu / (float64(n) / gib)
}

func avgMs(ns, ops int64) float64 {
	if ops == 0 {
		return 0
	}
	return float64(ns) / float64(ops) / 1e6
}

func round3(v float64) float64 {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return tv(ru.Utime) + tv(ru.Stime)
}

type gcStats struct {
	cycles uint64
	cpuS   float64
}

// goGC returns the Go GC cycles so far and the runtime's estimate of the CPU time they took.
// Buffers outside the Go heap shrink the live heap, so the GC runs more often (GOGC).
func goGC() gcStats {
	s := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/cpu/classes/gc/total:cpu-seconds"}}
	metrics.Read(s)
	var g gcStats
	if s[0].Value.Kind() == metrics.KindUint64 {
		g.cycles = s[0].Value.Uint64()
	}
	if s[1].Value.Kind() == metrics.KindFloat64 {
		g.cpuS = s[1].Value.Float64()
	}
	return g
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

// established443 counts this process's established TCP connections to remote port 443, from
// /proc/self/fd and /proc/net/tcp{,6}. -1 if unavailable.
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
