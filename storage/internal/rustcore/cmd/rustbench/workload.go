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
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"cloud.google.com/go/storage/experimental"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// opTimeout bounds every single operation, so a hung request fails the op instead of the run.
const opTimeout = 10 * time.Minute

type opResult struct {
	bytes   int64
	entries int64
	ttfb    time.Duration // reads: first byte; list: first page; 0 = not applicable
}

// opFunc performs one operation. i < 0 marks warmup ops.
type opFunc func(ctx context.Context, worker, i int) (opResult, error)

// target is where a case runs: the bucket (datasets, uploads), the zonal bucket (appendable
// uploads), the project (bucket listing) and the object name prefix.
type target struct {
	bucket, zonalBucket, project, prefix string
}

// runCase measures one case with one backend in this process.
func runCase(ctx context.Context, t target, backendName string, c benchCase) (*result, error) {
	be, err := lookupBackend(backendName)
	if err != nil {
		return nil, err
	}
	if why, skip := be.skip[c.Workload]; skip {
		return nil, fmt.Errorf("pair is skipped: %s", why)
	}
	if !c.runsOn(be) {
		return nil, fmt.Errorf("case %s runs only on %v", c.Name, c.Backends)
	}
	if c.Concurrency < 1 || (c.Ops < 1 && c.DurationS <= 0) {
		return nil, errors.New("concurrency must be at least 1, and ops too unless duration_s is set")
	}
	if c.Workload == wStrided && (c.Ranges < 1 || c.RangeSize < 1 || int64(c.Ranges-1)*c.Stride+c.RangeSize > c.Size) {
		return nil, fmt.Errorf("strided: %d windows of %d bytes, stride %d, do not fit in %d bytes", c.Ranges, c.RangeSize, c.Stride, c.Size)
	}
	if c.Workload == wStrided && c.Parallel && !c.CopyIO {
		return nil, errors.New("strided: parallel windows need copy_io (one application buffer per worker)")
	}
	if c.Workload == wChunked && (c.RangeSize < 1 || c.RangeSize > c.Size) {
		return nil, fmt.Errorf("chunked: chunks of %d bytes do not fit in %d bytes", c.RangeSize, c.Size)
	}
	if c.Workload == wAppend && t.zonalBucket == "" {
		return nil, errors.New("append: -zonal-bucket (or $RUSTBENCH_ZONAL_BUCKET) is required")
	}
	if c.Workload == wListBuckets && (t.project == "" || c.PageSize < 1) {
		return nil, errors.New("list-buckets: -project (or $RUSTBENCH_PROJECT) and page_size are required")
	}

	// Harness memory first, so that the baseline RSS includes it and peak - baseline is the
	// memory the client itself used.
	var payload []byte
	if c.Workload == wWrite || c.Workload == wAppend {
		payload = randomBytes(int(c.Size), 1)
	}
	bufs := make([][]byte, c.Concurrency)
	for i := range bufs {
		bufs[i] = make([]byte, c.ioBuf())
	}
	runtime.GC()
	rssBase := procStatusKB("VmRSS")

	var copts []option.ClientOption
	if c.ConnPool > 0 && be.transport == "grpc" {
		copts = append(copts, option.WithGRPCConnectionPool(c.ConnPool))
	}
	t0 := time.Now()
	client, err := be.newClient(ctx, copts...)
	if err != nil {
		return nil, fmt.Errorf("creating the client: %w", err)
	}
	defer client.Close()
	initMs := float64(time.Since(t0)) / float64(time.Millisecond)

	w := &workload{client: client, bkt: client.Bucket(t.bucket), wbkt: client.Bucket(t.bucket), project: t.project,
		prefix: t.prefix, be: be, c: c, payload: payload, bufs: bufs, token: fmt.Sprintf("%d", time.Now().UnixNano())}
	if c.Workload == wAppend {
		w.wbkt = client.Bucket(t.zonalBucket)
	}
	var op opFunc
	switch c.Workload {
	case wWrite, wAppend:
		op = w.write
	case wRead:
		op = w.read
	case wRange:
		op = w.rangeRead
	case wList:
		op = w.list
	case wOpen:
		op = w.open
	case wStrided:
		op = w.strided
	case wChunked:
		op = w.chunked
	case wListBuckets:
		op = w.listBuckets
	default:
		return nil, fmt.Errorf("unknown workload %q", c.Workload)
	}

	res := &result{Schema: resultSchema, Time: time.Now().UTC(), Backend: be.name, Engine: be.engine, Transport: be.transport,
		Case: c, ClientInitMs: initMs, RSSBaseMiB: kbToMiB(rssBase)}

	// Warmup: connections, TLS, tokens, caches. Not measured.
	res.WarmupErrors = runWorkers(ctx, c.Concurrency, c.Warmup, time.Time{}, op, nil)

	// Measured phase: all workers start together, after every worker finished its warmup.
	var mem0, mem1 runtime.MemStats
	runtime.ReadMemStats(&mem0)
	ffi0 := experimental.RustEngineStats()
	cpu0 := cpuNow()
	smp := startSampler()
	rec := newRecorder()
	start := time.Now()
	ops, deadline := c.Ops, time.Time{}
	if c.DurationS > 0 {
		deadline = start.Add(time.Duration(c.DurationS) * time.Second)
		if ops < 1 {
			ops = math.MaxInt
		}
	}
	res.Errors = runWorkers(ctx, c.Concurrency, ops, deadline, op, rec)
	wall := time.Since(start)
	cpu1 := cpuNow()
	res.ThreadsMax, res.GoroutinesMax = smp.finish()
	ffi1 := experimental.RustEngineStats()
	runtime.ReadMemStats(&mem1)
	res.MeasureStart, res.MeasureEnd = start.UTC(), start.Add(wall).UTC()

	res.Ops, res.FirstError = len(rec.lat), rec.firstErr
	res.Bytes, res.Entries = rec.bytes, rec.entries
	res.WallS = wall.Seconds()
	res.OpsPerS = float64(res.Ops) / res.WallS
	if res.Bytes > 0 {
		res.MiBPerS = float64(res.Bytes) / mib / res.WallS
	}
	if res.Entries > 0 {
		res.EntriesPerS = float64(res.Entries) / res.WallS
	}
	res.LatencyMs = summarize(rec.lat)
	if len(rec.ttfb) > 0 {
		s := summarize(rec.ttfb)
		res.TTFBMs = &s
	}
	res.CPUUserS, res.CPUSysS = cpu1.user-cpu0.user, cpu1.sys-cpu0.sys
	cpu := res.CPUUserS + res.CPUSysS
	if res.Bytes > 0 {
		res.CPUSPerGiB = cpu / (float64(res.Bytes) / (1 << 30))
	}
	if res.Ops > 0 {
		res.CPUMsPerOp = cpu * 1000 / float64(res.Ops)
	}
	res.RSSPeakMiB, res.RSSEndMiB = kbToMiB(procStatusKB("VmHWM")), kbToMiB(procStatusKB("VmRSS"))
	res.GoGCs = mem1.NumGC - mem0.NumGC
	res.GoGCPauseMs = float64(mem1.PauseTotalNs-mem0.PauseTotalNs) / 1e6
	res.GoHeapSysMiB = float64(mem1.HeapSys) / mib
	res.FFICalls = ffi1["ffi_calls"] - ffi0["ffi_calls"]
	res.AsyncOps = ffi1["async_ops"] - ffi0["async_ops"]

	w.deleteWritten(ctx) // after the measurements: not part of the results
	return res, nil
}

// recorder collects the outcome of measured ops from all workers.
type recorder struct {
	mu        sync.Mutex
	lat, ttfb []time.Duration
	bytes     int64
	entries   int64
	firstErr  string
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) add(d time.Duration, o opResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.firstErr == "" {
			r.firstErr = err.Error()
		}
		return
	}
	r.lat = append(r.lat, d)
	if o.ttfb > 0 {
		r.ttfb = append(r.ttfb, o.ttfb)
	}
	r.bytes += o.bytes
	r.entries += o.entries
}

// maxConsecutiveFailures stops a worker whose ops keep failing (e.g. a missing permission), so
// a timed phase does not spin on errors until its deadline.
const maxConsecutiveFailures = 20

// runWorkers runs ops ops on each of n workers concurrently, or fewer if the deadline (when not
// zero) passes first, and returns the number of failed ops. With rec == nil (warmup) op indexes
// are negative and nothing is recorded.
func runWorkers(ctx context.Context, n, ops int, deadline time.Time, op opFunc, rec *recorder) int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for wk := 0; wk < n; wk++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			consecutive := 0
			for i := 0; i < ops; i++ {
				if !deadline.IsZero() && !time.Now().Before(deadline) {
					return
				}
				idx := i
				if rec == nil {
					idx = -1 - i
				}
				octx, cancel := context.WithTimeout(ctx, opTimeout)
				t := time.Now()
				o, err := op(octx, wk, idx)
				d := time.Since(t)
				cancel()
				if err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					if rec == nil {
						log.Printf("warmup op failed: %v", err)
					}
				}
				if rec != nil {
					rec.add(d, o, err)
				}
				if err == nil {
					consecutive = 0
				} else if consecutive++; consecutive >= maxConsecutiveFailures {
					log.Printf("worker %d: %d failures in a row, stopping (last: %v)", wk, consecutive, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return failed
}

type workload struct {
	client  *storage.Client
	bkt     *storage.BucketHandle // datasets
	wbkt    *storage.BucketHandle // uploads (the zonal bucket for appendable uploads)
	project string
	prefix  string
	be      backend
	c       benchCase
	payload []byte
	bufs    [][]byte // one per worker: the application's Read buffer
	token   string   // makes written object names unique per process

	mu      sync.Mutex
	written []string
}

// write uploads the payload in IOBuf-sized Write calls. Appendable uploads (wAppend) create a
// new appendable object in the zonal bucket and finalize it on Close.
func (w *workload) write(ctx context.Context, worker, i int) (opResult, error) {
	name := fmt.Sprintf("%s/write/%s/%s/%s/w%02d-%05d", w.prefix, w.c.Name, w.be.name, w.token, worker, i)
	if i < 0 {
		name = fmt.Sprintf("%s/write/%s/%s/%s/w%02d-warm%d", w.prefix, w.c.Name, w.be.name, w.token, worker, -i)
	}
	if w.c.ReuseNames {
		name = fmt.Sprintf("%s/write/%s/%s/%s/w%02d", w.prefix, w.c.Name, w.be.name, w.token, worker)
	}
	ow := w.wbkt.Object(name).NewWriter(ctx)
	if w.c.Workload == wAppend {
		ow.Append, ow.FinalizeOnClose = true, true
	}
	step := w.c.ioBuf()
	for off := 0; off < len(w.payload); off += step {
		if _, err := ow.Write(w.payload[off:min(off+step, len(w.payload))]); err != nil {
			ow.Close()
			return opResult{}, fmt.Errorf("write %s: %w", name, err)
		}
	}
	err := ow.Close()
	w.mu.Lock()
	if !w.c.ReuseNames || i <= 0 { // also after a failed Close: the object may exist; duplicates are harmless
		w.written = append(w.written, name)
	}
	w.mu.Unlock()
	if err != nil {
		return opResult{}, fmt.Errorf("close %s: %w", name, err)
	}
	return opResult{bytes: int64(len(w.payload))}, nil
}

// read downloads a whole dataset object with IOBuf-sized Read calls.
func (w *workload) read(ctx context.Context, worker, i int) (opResult, error) {
	n := w.c.sources()
	name := readObjectName(w.prefix, w.c.Size, (worker+max(i, 0)*w.c.Concurrency)%n)
	return w.download(ctx, w.bkt.Object(name), 0, -1, w.c.Size, w.bufs[worker])
}

// rangeRead downloads RangeSize bytes at a random offset of a dataset object.
func (w *workload) rangeRead(ctx context.Context, worker, i int) (opResult, error) {
	n := w.c.sources()
	name := readObjectName(w.prefix, w.c.Size, worker%n)
	off := rand.Int64N(w.c.Size - w.c.RangeSize + 1)
	return w.download(ctx, w.bkt.Object(name), off, w.c.RangeSize, w.c.RangeSize, w.bufs[worker])
}

// open opens a zero-length reader at a random offset of a dataset object and closes it: the
// shared benchmarks' Open workload without a MultiRangeDownloader. Over gRPC both clients open
// a read stream and return once its first response (with the object) arrived.
func (w *workload) open(ctx context.Context, worker, i int) (opResult, error) {
	name := readObjectName(w.prefix, w.c.Size, (worker+max(i, 0)*w.c.Concurrency)%w.c.sources())
	r, err := w.bkt.Object(name).NewRangeReader(ctx, rand.Int64N(w.c.Size), 0)
	if err != nil {
		return opResult{}, fmt.Errorf("open %s: %w", name, err)
	}
	size := r.Attrs.Size
	if err := r.Close(); err != nil {
		return opResult{}, fmt.Errorf("close %s: %w", name, err)
	}
	if size != w.c.Size {
		return opResult{}, fmt.Errorf("open %s: object size %d, want %d (run `rustbench setup`)", name, size, w.c.Size)
	}
	return opResult{}, nil
}

// strided downloads Ranges windows of RangeSize bytes, Stride apart from offset 0, of a dataset
// object, with one range reader per window: the shared benchmarks' Read50x2x4 workload without
// a MultiRangeDownloader. The windows are read one after the other, or all at once (Parallel).
func (w *workload) strided(ctx context.Context, worker, i int) (opResult, error) {
	o := w.bkt.Object(readObjectName(w.prefix, w.c.Size, worker%w.c.sources()))
	window := func(k int) (opResult, error) {
		return w.download(ctx, o, int64(k)*w.c.Stride, w.c.RangeSize, w.c.RangeSize, w.bufs[worker])
	}
	var res opResult
	if !w.c.Parallel {
		for k := range w.c.Ranges {
			r, err := window(k)
			if err != nil {
				return opResult{}, err
			}
			if k == 0 {
				res.ttfb = r.ttfb
			}
			res.bytes += r.bytes
		}
		return res, nil
	}
	results := make([]opResult, w.c.Ranges)
	errs := make([]error, w.c.Ranges)
	var wg sync.WaitGroup
	for k := range w.c.Ranges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[k], errs[k] = window(k)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return opResult{}, err
	}
	for _, r := range results {
		if res.ttfb == 0 || r.ttfb < res.ttfb {
			res.ttfb = r.ttfb
		}
		res.bytes += r.bytes
	}
	return res, nil
}

// chunked downloads the next RangeSize chunk of the worker's own dataset object: each worker
// reads its object front to back in RangeSize chunks, one range reader per chunk, and starts
// over at the end. A tail shorter than RangeSize is skipped, so every op moves RangeSize bytes.
func (w *workload) chunked(ctx context.Context, worker, i int) (opResult, error) {
	o := w.bkt.Object(readObjectName(w.prefix, w.c.Size, worker%w.c.sources()))
	k := int64(max(i, 0)) % (w.c.Size / w.c.RangeSize)
	return w.download(ctx, o, k*w.c.RangeSize, w.c.RangeSize, w.c.RangeSize, w.bufs[worker])
}

func (w *workload) download(ctx context.Context, o *storage.ObjectHandle, off, length, want int64, buf []byte) (opResult, error) {
	t0 := time.Now()
	r, err := o.NewRangeReader(ctx, off, length)
	if err != nil {
		return opResult{}, fmt.Errorf("open %s: %w", o.ObjectName(), err)
	}
	defer r.Close()
	var res opResult
	if w.c.CopyIO {
		// As the shared benchmarks (io.Copy to a discarding writer): io.Copy uses
		// Reader.WriteTo, i.e. each client's own copy path.
		d := &discard{t0: t0}
		n, err := io.Copy(d, r)
		if err != nil {
			return opResult{}, fmt.Errorf("read %s: %w", o.ObjectName(), err)
		}
		res.bytes, res.ttfb = n, d.ttfb
	} else {
		for {
			// Plain Read calls: every backend sees the same call pattern (no WriterTo shortcuts).
			n, err := r.Read(buf)
			if n > 0 && res.bytes == 0 {
				res.ttfb = time.Since(t0)
			}
			res.bytes += int64(n)
			if err == io.EOF {
				break
			}
			if err != nil {
				return opResult{}, fmt.Errorf("read %s: %w", o.ObjectName(), err)
			}
		}
	}
	if res.bytes != want {
		return opResult{}, fmt.Errorf("read %s: got %d bytes, want %d", o.ObjectName(), res.bytes, want)
	}
	return res, nil
}

// discard is io.Discard that records when the first byte arrived.
type discard struct {
	t0   time.Time
	ttfb time.Duration
}

func (d *discard) Write(p []byte) (int, error) {
	if d.ttfb == 0 && len(p) > 0 {
		d.ttfb = time.Since(d.t0)
	}
	return len(p), nil
}

// list lists the whole list dataset (every page), or only its first page (FirstPageOnly).
func (w *workload) list(ctx context.Context, worker, i int) (opResult, error) {
	t0 := time.Now()
	q := &storage.Query{Prefix: listPrefix(w.prefix, w.c.Objects), Delimiter: w.c.Delimiter}
	p := iterator.NewPager(w.bkt.Objects(ctx, q), w.c.PageSize, "")
	var res opResult
	for {
		var page []*storage.ObjectAttrs
		tok, err := p.NextPage(&page)
		if err != nil {
			return opResult{}, fmt.Errorf("list %s: %w", q.Prefix, err)
		}
		if res.ttfb == 0 {
			res.ttfb = time.Since(t0)
		}
		res.entries += int64(len(page))
		if tok == "" || w.c.FirstPageOnly {
			break
		}
	}
	want := int64(w.c.Objects)
	if w.c.Delimiter != "" {
		want = int64(listDirs(w.c.Objects))
	}
	if w.c.FirstPageOnly {
		want = min(want, int64(w.c.PageSize))
	}
	if res.entries != want {
		return opResult{}, fmt.Errorf("list %s: got %d entries, want %d (run `rustbench setup`)", q.Prefix, res.entries, want)
	}
	return res, nil
}

// listBuckets lists one page of PageSize buckets of the project: one ListBuckets RPC.
func (w *workload) listBuckets(ctx context.Context, worker, i int) (opResult, error) {
	t0 := time.Now()
	p := iterator.NewPager(w.client.Buckets(ctx, w.project), w.c.PageSize, "")
	var page []*storage.BucketAttrs
	if _, err := p.NextPage(&page); err != nil {
		return opResult{}, fmt.Errorf("list buckets of %s: %w", w.project, err)
	}
	if len(page) != w.c.PageSize {
		return opResult{}, fmt.Errorf("list buckets of %s: got %d buckets, want %d (the project needs at least that many)", w.project, len(page), w.c.PageSize)
	}
	return opResult{entries: int64(len(page)), ttfb: time.Since(t0)}, nil
}

// deleteWritten removes the objects this process uploaded.
func (w *workload) deleteWritten(ctx context.Context) {
	w.mu.Lock()
	names := w.written
	w.mu.Unlock()
	if len(names) == 0 {
		return
	}
	failed := parallelDo(ctx, len(names), 32, func(ctx context.Context, i int) error {
		err := w.wbkt.Object(names[i]).Delete(ctx)
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return err
	})
	if failed > 0 {
		log.Printf("could not delete %d of %d written objects (rustbench cleanup removes them)", failed, len(names))
	}
}

// randomBytes returns n bytes of seeded pseudo-random (incompressible) data.
func randomBytes(n int, seed uint64) []byte {
	b := make([]byte, n)
	r := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8), 'r', 'u', 's', 't', 'b', 'e', 'n', 'c', 'h'})
	r.Read(b)
	return b
}

// parallelDo runs f(0..n-1) on `workers` goroutines and returns the number of failures.
func parallelDo(ctx context.Context, n, workers int, f func(context.Context, int) error) int {
	idx := make(chan int)
	var failed int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				octx, cancel := context.WithTimeout(ctx, opTimeout)
				err := f(octx, i)
				cancel()
				if err != nil {
					mu.Lock()
					if failed == 0 {
						log.Printf("first failure: %v", err)
					}
					failed++
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return failed
}
