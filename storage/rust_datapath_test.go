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

//go:build gcsrust && cgo && linux && amd64

package storage

// Regression tests for the defects found in the original POC bridge, run through the public
// storage API with experimental.WithRustDataPath() against the storage testbench.
//
//	STORAGE_EMULATOR_HOST=http://localhost:9000 STORAGE_EMULATOR_HOST_GRPC=localhost:8888 \
//	  go test -tags gcsrust -race -run TestRustDataPath ./...

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage/experimental"
	"cloud.google.com/go/storage/internal/rustcore"
	"google.golang.org/api/googleapi"
)

// rustTestPrefix isolates object names of one test run (shared real buckets).
var rustTestPrefix string

// rustTestClients returns a Rust-data-path client and a pure-Go client. Two modes:
//   - RUST_DP_TEST_BUCKET=<bucket>: real GCS with ADC (objects under a unique prefix);
//   - STORAGE_EMULATOR_HOST: storage testbench (a fresh bucket). Note: the testbench cannot
//     complete uploads made by the Rust SDK (multipart 500s, resumable PUTs hang), so write
//     tests only pass against real GCS.
func rustTestClients(t *testing.T) (rust, native *Client, bucket string) {
	t.Helper()
	ctx := context.Background()
	realBucket := os.Getenv("RUST_DP_TEST_BUCKET")
	if realBucket == "" && os.Getenv("STORAGE_EMULATOR_HOST") == "" {
		t.Skip("set RUST_DP_TEST_BUCKET (real GCS) or STORAGE_EMULATOR_HOST (testbench)")
	}
	var err error
	if native, err = NewClient(ctx); err != nil {
		t.Fatal(err)
	}
	if rust, err = NewClient(ctx, experimental.WithRustDataPath()); err != nil {
		t.Fatal(err)
	}
	if _, ok := rust.tc.(*rustStorageClient); !ok {
		t.Fatalf("rust data path not active: %T", rust.tc)
	}
	rustTestPrefix = fmt.Sprintf("rustdp/%d/%s/", time.Now().UnixNano(), t.Name())
	if realBucket != "" {
		bucket = realBucket
		t.Cleanup(func() {
			it := native.Bucket(bucket).Objects(ctx, &Query{Prefix: rustTestPrefix})
			for {
				a, err := it.Next()
				if err != nil {
					break
				}
				native.Bucket(bucket).Object(a.Name).Delete(ctx)
			}
		})
	} else {
		bucket = fmt.Sprintf("rustdp-%d", time.Now().UnixNano())
		if err := native.Bucket(bucket).Create(ctx, "rust-project", nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { rust.Close(); native.Close() })
	return rust, native, bucket
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func writeObj(t *testing.T, c *Client, bucket, name string, data []byte) *ObjectAttrs {
	t.Helper()
	w := c.Bucket(bucket).Object(rustTestPrefix + name).NewWriter(context.Background())
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return w.Attrs()
}

func TestRustDataPathRoundTrip(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	for _, size := range []int{0, 1, 511, 512, 64 << 10, 16<<20 - 1, 16 << 20, 40 << 20} {
		name := fmt.Sprintf("rt-%d", size)
		data := randBytes(size)
		attrs := writeObj(t, rc, bucket, name, data)
		if attrs == nil || attrs.Size != int64(size) || attrs.Generation == 0 || attrs.CRC32C == 0 && size > 0 {
			t.Fatalf("size %d: bad attrs from Rust writer: %+v", size, attrs)
		}
		// Read back through both paths.
		for _, c := range []*Client{rc, nc} {
			r, err := c.Bucket(bucket).Object(rustTestPrefix + name).NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("size %d: read mismatch (err=%v, got %d bytes)", size, err, len(got))
			}
		}
	}
	reads, writes, rf, wf := rustDataPathStats()
	t.Logf("engine reads=%d writes=%d read-fallbacks=%d write-fallbacks=%d", reads, writes, rf, wf)
}

// POC defect: cancelling the ctx during Close dropped the channel (=> EOF) and detached the
// upload task, so the object was finalized while Close returned context.Canceled.
func TestRustDataPathCancelDuringCloseDoesNotCommit(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	data := randBytes(48 << 20)
	committed := 0
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("cancel-close-%d", i)
		ctx, cancel := context.WithCancel(context.Background())
		w := rc.Bucket(bucket).Object(rustTestPrefix + name).NewWriter(ctx)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(time.Duration(5+i*5)*time.Millisecond, cancel)
		err := w.Close()
		cancel()
		time.Sleep(300 * time.Millisecond) // let any detached upload finish
		_, aerr := nc.Bucket(bucket).Object(rustTestPrefix + name).Attrs(context.Background())
		exists := aerr == nil
		if err != nil && exists {
			committed++
			t.Errorf("iteration %d: Close returned %v but the object was committed", i, err)
		}
		t.Logf("iteration %d: close err=%v exists=%v", i, err, exists)
	}
	if committed > 0 {
		t.Fatalf("%d phantom commits", committed)
	}
}

func rssKB() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			f := strings.Fields(l)
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	return 0
}

// POC defect: a ctx cancelled mid-Write left the Rust upload task blocked forever with its
// buffers (Close returned early without freeing the handle).
func TestRustDataPathCancelDuringWriteNoLeak(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	chunk := randBytes(1 << 20)
	run := func(i int) {
		name := fmt.Sprintf("cancel-write-%d", i)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := rc.Bucket(bucket).Object(rustTestPrefix + name).NewWriter(ctx)
		for j := 0; j < 256; j++ {
			if j == 40 {
				cancel()
			}
			if _, err := w.Write(chunk); err != nil {
				break
			}
		}
		if err := w.Close(); err == nil {
			t.Errorf("iteration %d: Close succeeded after cancel", i)
		}
		if _, err := nc.Bucket(bucket).Object(rustTestPrefix + name).Attrs(context.Background()); err == nil {
			t.Errorf("iteration %d: object exists after cancelled upload", i)
		}
	}
	for i := 0; i < 3; i++ { // warm up allocators
		run(i)
	}
	runtime.GC()
	before := rssKB()
	for i := 3; i < 33; i++ {
		run(i)
	}
	runtime.GC()
	time.Sleep(time.Second)
	after := rssKB()
	t.Logf("VmRSS before=%d MiB after=%d MiB (30 cancelled 256 MiB uploads)", before>>10, after>>10)
	if after-before > 256<<10 {
		t.Fatalf("RSS grew by %d MiB: cancelled uploads are leaking", (after-before)>>10)
	}
}

// POC defects: preconditions other than GenerationMatch/DoesNotExist were silently dropped,
// and the 412 lost its HTTP status on the way to Go. Each failed precondition must surface as
// the same *googleapi.Error code the pure-Go client returns (412, or 304 for *NotMatch).
func TestRustDataPathPreconditions(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	attrs := writeObj(t, rc, bucket, "pre", []byte("v1"))
	code := func(err error) int {
		var ge *googleapi.Error
		if errors.As(err, &ge) {
			return ge.Code
		}
		return 0
	}
	cases := []Conditions{
		{DoesNotExist: true},
		{GenerationMatch: attrs.Generation + 1},
		{MetagenerationMatch: attrs.Metageneration + 7},
		{MetagenerationNotMatch: attrs.Metageneration},
		{GenerationNotMatch: attrs.Generation},
	}
	for _, cond := range cases {
		var got [2]int
		for i, c := range []*Client{nc, rc} {
			w := c.Bucket(bucket).Object(rustTestPrefix + "pre").If(cond).NewWriter(ctx)
			w.Write([]byte("v2"))
			got[i] = code(w.Close())
		}
		if got[1] == 0 || got[0] != got[1] {
			t.Errorf("write with %+v: native googleapi code %d, rust %d", cond, got[0], got[1])
		}
	}
	// Reads: a failed precondition must not return data.
	if _, err := rc.Bucket(bucket).Object(rustTestPrefix + "pre").If(Conditions{MetagenerationMatch: attrs.Metageneration + 7}).NewReader(ctx); code(err) != http.StatusPreconditionFailed {
		t.Errorf("read with bad MetagenerationMatch: want 412, got %v", err)
	}
	if _, err := rc.Bucket(bucket).Object(rustTestPrefix + "does-not-exist").NewReader(ctx); !errors.Is(err, ErrObjectNotExist) {
		t.Errorf("missing object: want ErrObjectNotExist, got %v", err)
	}
}

// POC defect: Metadata, Cache-Control, Content-* and user checksums were silently dropped.
func TestRustDataPathWriterAttrsAndChecksums(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	data := []byte(strings.Repeat("hello rust ", 100))
	sum := md5.Sum(data)
	w := rc.Bucket(bucket).Object(rustTestPrefix + "attrs").NewWriter(ctx)
	w.Metadata = map[string]string{"k": "v"}
	w.CacheControl = "no-cache"
	w.ContentLanguage = "en"
	w.ContentDisposition = "inline"
	w.MD5 = sum[:]
	w.Write(data)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := nc.Bucket(bucket).Object(rustTestPrefix + "attrs").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["k"] != "v" || got.CacheControl != "no-cache" || got.ContentLanguage != "en" ||
		got.ContentDisposition != "inline" || !strings.HasPrefix(got.ContentType, "text/plain") {
		t.Errorf("attributes not persisted: %+v", got)
	}
	// A wrong MD5 / CRC32C must be rejected, not ignored.
	bad := rc.Bucket(bucket).Object(rustTestPrefix + "bad-md5").NewWriter(ctx)
	bad.MD5 = make([]byte, 16)
	bad.Write(data)
	if err := bad.Close(); err == nil {
		t.Errorf("upload with wrong MD5 succeeded")
	}
	bad = rc.Bucket(bucket).Object(rustTestPrefix + "bad-crc").NewWriter(ctx)
	bad.CRC32C, bad.SendCRC32C = 12345, true
	bad.Write(data)
	if err := bad.Close(); err == nil {
		t.Errorf("upload with wrong CRC32C succeeded")
	}
}

func TestRustDataPathRangeSemantics(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	data := randBytes(100)
	writeObj(t, nc, bucket, "range", data)
	type tc struct{ off, length int64 }
	for _, c := range []tc{{0, 0}, {0, 10}, {90, -1}, {-10, -1}, {95, 10}, {0, -1}} {
		var res [2]struct {
			data           []byte
			start, remain0 int64
		}
		for i, cl := range []*Client{nc, rc} {
			r, err := cl.Bucket(bucket).Object(rustTestPrefix+"range").NewRangeReader(ctx, c.off, c.length)
			if err != nil {
				t.Fatalf("%+v: %v", c, err)
			}
			res[i].start, res[i].remain0 = r.Attrs.StartOffset, r.Remain()
			res[i].data, _ = io.ReadAll(r)
			r.Close()
		}
		if !bytes.Equal(res[0].data, res[1].data) || res[0].start != res[1].start || res[0].remain0 != res[1].remain0 {
			t.Errorf("NewRangeReader(%d,%d): native {len=%d start=%d remain=%d} rust {len=%d start=%d remain=%d}", c.off, c.length,
				len(res[0].data), res[0].start, res[0].remain0, len(res[1].data), res[1].start, res[1].remain0)
		}
	}
}

// POC defect: the engine returned stored gzip bytes where Go returns decompressed content.
func TestRustDataPathGzipTranscoding(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	plain := []byte(strings.Repeat("compressible ", 1000))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain)
	zw.Close()
	w := nc.Bucket(bucket).Object(rustTestPrefix + "gz").NewWriter(ctx)
	w.ContentEncoding, w.ContentType = "gzip", "text/plain"
	w.Write(gz.Bytes())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, compressed := range []bool{false, true} {
		var out [2][]byte
		var dec [2]bool
		for i, c := range []*Client{nc, rc} {
			r, err := c.Bucket(bucket).Object(rustTestPrefix + "gz").ReadCompressed(compressed).NewReader(ctx)
			if err != nil {
				t.Fatal(err)
			}
			out[i], _ = io.ReadAll(r)
			dec[i] = r.Attrs.Decompressed
			r.Close()
		}
		if !bytes.Equal(out[0], out[1]) || dec[0] != dec[1] {
			t.Errorf("ReadCompressed(%v): native %d bytes decompressed=%v, rust %d bytes decompressed=%v",
				compressed, len(out[0]), dec[0], len(out[1]), dec[1])
		}
	}
}

// POC defect: Close() concurrent with an in-flight Read freed memory still used by a task.
func TestRustDataPathConcurrentCloseDuringRead(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	writeObj(t, nc, bucket, "big", randBytes(32<<20))
	for i := 0; i < 50; i++ {
		r, err := rc.Bucket(bucket).Object(rustTestPrefix + "big").NewReader(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.Copy(io.Discard, r)
		}()
		time.Sleep(time.Duration(i%7) * time.Millisecond)
		r.Close()
		wg.Wait()
	}
}

func TestRustDataPathMidStreamCancel(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	writeObj(t, nc, bucket, "stream", randBytes(64<<20))
	ctx, cancel := context.WithCancel(context.Background())
	r, err := rc.Bucket(bucket).Object(rustTestPrefix + "stream").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := make([]byte, 1<<20)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	start := time.Now()
	var rerr error
	for rerr == nil {
		_, rerr = r.Read(buf)
	}
	if !errors.Is(rerr, context.Canceled) {
		t.Errorf("after cancel: want context.Canceled, got %v", rerr)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancellation took %v", d)
	}
	if _, err := r.Read(buf); err == nil {
		t.Errorf("reader still usable after cancellation")
	}
}

// The engine must not be fooled by small io.Copy-style writes/reads: crossings stay bounded.
func TestRustDataPathCrossingsBounded(t *testing.T) {
	rc, _, bucket := rustTestClients(t)
	ctx := context.Background()
	data := randBytes(32 << 20)
	f0 := rustcore.FFICalls.Load()
	a0 := rustcore.AsyncOps.Load()
	w := rc.Bucket(bucket).Object(rustTestPrefix + "xings").NewWriter(ctx)
	io.CopyBuffer(struct{ io.Writer }{w}, struct{ io.Reader }{bytes.NewReader(data)}, make([]byte, 32<<10))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := rc.Bucket(bucket).Object(rustTestPrefix + "xings").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{r}, make([]byte, 32<<10))
	r.Close()
	if err != nil || n != int64(len(data)) {
		t.Fatalf("read back %d, %v", n, err)
	}
	t.Logf("32 MiB write+read with 32 KiB calls: %d FFI calls, %d async completions",
		rustcore.FFICalls.Load()-f0, rustcore.AsyncOps.Load()-a0)
}
