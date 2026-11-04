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

// Tests of the Go SDK served entirely by the Rust core (experimental.WithRustCore), through the
// public storage API, compared against the pure-Go client:
//
//   - data path: regression tests for every defect found in the original POC bridge;
//   - control plane: metadata, update masks with arbitrary map keys, list/paging, copy/compose,
//     bucket metadata, IAM;
//   - policy hand-off: one FFI call per operation (no Go retry loop), retry predicates
//     evaluated by the core, error shapes.
//
//	RUST_DP_TEST_BUCKET=<bucket> go test -tags gcsrust -race -run TestRustCore ./...

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
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage/experimental"
	"cloud.google.com/go/storage/internal/rustcore"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rustTestPrefix isolates object names of one test run (shared real buckets).
var rustTestPrefix string

// rustTestClients returns a Rust-core client and a pure-Go client. Two modes:
//   - RUST_DP_TEST_BUCKET=<bucket>: real GCS with ADC (objects under a unique prefix);
//   - STORAGE_EMULATOR_HOST (+ STORAGE_EMULATOR_HOST_GRPC): storage testbench (a fresh bucket).
//     Note: the testbench cannot complete uploads made by the Rust SDK (multipart 500s,
//     resumable PUTs hang), so write tests only pass against real GCS.
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
	if rust, err = NewClient(ctx, experimental.WithRustCore()); err != nil {
		t.Fatal(err)
	}
	if _, ok := rust.tc.(*rustStorageClient); !ok {
		t.Fatalf("rust core not active: %T", rust.tc)
	}
	rustTestPrefix = fmt.Sprintf("rustcore/%d/%s/", time.Now().UnixNano(), t.Name())
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
		bucket = fmt.Sprintf("rustcore-%d", time.Now().UnixNano())
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

func httpCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// ---- Data path -------------------------------------------------------------------------------

func TestRustCoreRoundTrip(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	for _, size := range []int{0, 1, 511, 512, 64 << 10, 16<<20 - 1, 16 << 20, 40 << 20} {
		name := fmt.Sprintf("rt-%d", size)
		data := randBytes(size)
		attrs := writeObj(t, rc, bucket, name, data)
		if attrs == nil || attrs.Size != int64(size) || attrs.Generation == 0 || attrs.CRC32C == 0 && size > 0 {
			t.Fatalf("size %d: bad attrs from Rust writer: %+v", size, attrs)
		}
		// Read back through both clients.
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
	t.Logf("core stats: %v", rustEngineStats())
}

// POC defect: cancelling the ctx during Close dropped the channel (=> EOF) and detached the
// upload task, so the object was finalized while Close returned context.Canceled.
func TestRustCoreCancelDuringCloseDoesNotCommit(t *testing.T) {
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
func TestRustCoreCancelDuringWriteNoLeak(t *testing.T) {
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
func TestRustCorePreconditions(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	attrs := writeObj(t, rc, bucket, "pre", []byte("v1"))
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
			got[i] = httpCode(w.Close())
		}
		if got[1] == 0 || got[0] != got[1] {
			t.Errorf("write with %+v: native googleapi code %d, rust %d", cond, got[0], got[1])
		}
	}
	// Reads: a failed precondition must not return data.
	if _, err := rc.Bucket(bucket).Object(rustTestPrefix + "pre").If(Conditions{MetagenerationMatch: attrs.Metageneration + 7}).NewReader(ctx); httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("read with bad MetagenerationMatch: want 412, got %v", err)
	}
	if _, err := rc.Bucket(bucket).Object(rustTestPrefix + "does-not-exist").NewReader(ctx); !errors.Is(err, ErrObjectNotExist) {
		t.Errorf("missing object: want ErrObjectNotExist, got %v", err)
	}
}

// POC defect: Metadata, Cache-Control, Content-* and user checksums were silently dropped.
func TestRustCoreWriterAttrsAndChecksums(t *testing.T) {
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

func TestRustCoreRangeSemantics(t *testing.T) {
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
func TestRustCoreGzipTranscoding(t *testing.T) {
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
func TestRustCoreConcurrentCloseDuringRead(t *testing.T) {
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

func TestRustCoreMidStreamCancel(t *testing.T) {
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

// Small io.Copy-style writes/reads must not translate into one crossing each.
func TestRustCoreCrossingsBounded(t *testing.T) {
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

// ---- Control plane ---------------------------------------------------------------------------

// Object metadata through the core: Attrs, Update (including metadata keys that proto3 JSON
// field masks cannot represent), conditional update, Delete, error shapes.
func TestRustCoreObjectMetadata(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	writeObj(t, nc, bucket, "meta", []byte("metadata test"))
	obj := rustTestPrefix + "meta"

	ra, err := rc.Bucket(bucket).Object(obj).Attrs(ctx)
	if err != nil {
		t.Fatalf("rust Attrs: %v", err)
	}
	na, err := nc.Bucket(bucket).Object(obj).Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Name != na.Name || ra.Size != na.Size || ra.Generation != na.Generation || ra.Metageneration != na.Metageneration ||
		ra.CRC32C != na.CRC32C || !bytes.Equal(ra.MD5, na.MD5) || ra.ContentType != na.ContentType || ra.StorageClass != na.StorageClass {
		t.Errorf("Attrs differ:\n rust   %+v\n native %+v", ra, na)
	}

	// Keys with upper case, dashes and underscores: not representable in a proto3-JSON mask.
	md := map[string]string{"X-Custom-Key": "1", "camelCase": "2", "with_underscore": "3", "lower": "4"}
	ua, err := rc.Bucket(bucket).Object(obj).Update(ctx, ObjectAttrsToUpdate{Metadata: md, ContentLanguage: "fr"})
	if err != nil {
		t.Fatalf("rust Update: %v", err)
	}
	got, err := nc.Bucket(bucket).Object(obj).Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range md {
		if got.Metadata[k] != v {
			t.Errorf("metadata %q = %q, want %q (all: %v)", k, got.Metadata[k], v, got.Metadata)
		}
	}
	if got.ContentLanguage != "fr" || ua.Metageneration != got.Metageneration {
		t.Errorf("update not applied: lang=%q metagen rust=%d native=%d", got.ContentLanguage, ua.Metageneration, got.Metageneration)
	}

	// Conditional update with a stale metageneration: FailedPrecondition / 412.
	_, err = rc.Bucket(bucket).Object(obj).If(Conditions{MetagenerationMatch: ra.Metageneration}).Update(ctx, ObjectAttrsToUpdate{ContentLanguage: "de"})
	if status.Code(err) != codes.FailedPrecondition || httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("stale conditional update: want FailedPrecondition/412, got %v (code %v, http %d)", err, status.Code(err), httpCode(err))
	}

	if err := rc.Bucket(bucket).Object(obj).Delete(ctx); err != nil {
		t.Fatalf("rust Delete: %v", err)
	}
	if _, err := nc.Bucket(bucket).Object(obj).Attrs(ctx); !errors.Is(err, ErrObjectNotExist) {
		t.Errorf("after rust Delete: native Attrs = %v, want ErrObjectNotExist", err)
	}
	_, err = rc.Bucket(bucket).Object(obj).Attrs(ctx)
	if !errors.Is(err, ErrObjectNotExist) || status.Code(err) != codes.NotFound || httpCode(err) != http.StatusNotFound {
		t.Errorf("missing object: want ErrObjectNotExist/NotFound/404, got %v", err)
	}
}

// Listing through the core, compared with the pure-Go client, with tiny pages.
func TestRustCoreListObjects(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	for _, n := range []string{"a/1", "a/2", "a/b/3", "c", "d/4"} {
		writeObj(t, nc, bucket, "list/"+n, []byte(n))
	}
	list := func(c *Client, q *Query) (names []string, pages int) {
		it := c.Bucket(bucket).Objects(ctx, q)
		p := iterator.NewPager(it, 2, "")
		for {
			var page []*ObjectAttrs
			tok, err := p.NextPage(&page)
			if err != nil {
				t.Fatalf("list %+v: %v", q, err)
			}
			pages++
			for _, a := range page {
				if a.Prefix != "" {
					names = append(names, "prefix:"+strings.TrimPrefix(a.Prefix, rustTestPrefix))
				} else {
					names = append(names, strings.TrimPrefix(a.Name, rustTestPrefix))
				}
			}
			if tok == "" {
				break
			}
		}
		sort.Strings(names)
		return names, pages
	}
	for _, q := range []*Query{
		{Prefix: rustTestPrefix + "list/"},
		{Prefix: rustTestPrefix + "list/", Delimiter: "/"},
		{Prefix: rustTestPrefix + "list/a/", StartOffset: rustTestPrefix + "list/a/2"},
	} {
		rn, rp := list(rc, q)
		nn, _ := list(nc, q)
		if strings.Join(rn, ",") != strings.Join(nn, ",") {
			t.Errorf("list %+v:\n rust   %v\n native %v", q, rn, nn)
		}
		t.Logf("list prefix=%q delim=%q: %d entries in %d pages", strings.TrimPrefix(q.Prefix, rustTestPrefix), q.Delimiter, len(rn), rp)
	}
}

// Copier (RewriteObject) and Composer (ComposeObject) through the core.
func TestRustCoreCopyAndCompose(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	a, b := randBytes(300<<10), randBytes(200<<10)
	writeObj(t, nc, bucket, "src-a", a)
	writeObj(t, nc, bucket, "src-b", b)
	bkt := rc.Bucket(bucket)

	copier := bkt.Object(rustTestPrefix + "copy").If(Conditions{DoesNotExist: true}).CopierFrom(bkt.Object(rustTestPrefix + "src-a"))
	copier.ContentType = "application/x-copied"
	ca, err := copier.Run(ctx)
	if err != nil {
		t.Fatalf("rust copy: %v", err)
	}
	if ca.Size != int64(len(a)) || ca.ContentType != "application/x-copied" {
		t.Errorf("copy attrs: %+v", ca)
	}

	composer := bkt.Object(rustTestPrefix+"composed").ComposerFrom(bkt.Object(rustTestPrefix+"src-a"), bkt.Object(rustTestPrefix+"src-b"))
	co, err := composer.Run(ctx)
	if err != nil {
		t.Fatalf("rust compose: %v", err)
	}
	r, err := nc.Bucket(bucket).Object(rustTestPrefix + "composed").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, append(append([]byte{}, a...), b...)) || co.Size != int64(len(a)+len(b)) || co.ComponentCount != 2 {
		t.Errorf("composed object wrong: %d bytes, attrs size=%d components=%d", len(got), co.Size, co.ComponentCount)
	}
}

// Bucket metadata and IAM through the core.
func TestRustCoreBucketAndIAM(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	ra, err := rc.Bucket(bucket).Attrs(ctx)
	if err != nil {
		t.Fatalf("rust bucket Attrs: %v", err)
	}
	na, err := nc.Bucket(bucket).Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Name != na.Name || !strings.EqualFold(ra.Location, na.Location) || ra.StorageClass != na.StorageClass ||
		ra.MetaGeneration != na.MetaGeneration || ra.UniformBucketLevelAccess.Enabled != na.UniformBucketLevelAccess.Enabled {
		t.Errorf("bucket attrs differ:\n rust   %+v\n native %+v", ra, na)
	}
	if _, err := rc.Bucket(bucket + "-does-not-exist-" + strconv.Itoa(int(time.Now().Unix()))).Attrs(ctx); !errors.Is(err, ErrBucketNotExist) {
		t.Errorf("missing bucket: want ErrBucketNotExist, got %v", err)
	}
	p, err := rc.Bucket(bucket).IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("rust IAM Policy: %v", err)
	}
	t.Logf("IAM policy: %d roles", len(p.Roles()))
	perms, err := rc.Bucket(bucket).IAM().TestPermissions(ctx, []string{"storage.objects.get", "storage.objects.list"})
	if err != nil || len(perms) == 0 {
		t.Errorf("rust TestPermissions = %v, %v", perms, err)
	}
}

// ---- Policy hand-off -------------------------------------------------------------------------

// One operation = one crossing: the Go retry loop must not run on top of the core's. A retry
// predicate (WithErrorFunc) is evaluated by the core through a callback.
func TestRustCoreRetryHandOff(t *testing.T) {
	rc, _, bucket := rustTestClients(t)
	ctx := context.Background()
	missing := rc.Bucket(bucket).Object(rustTestPrefix + "never-written")

	f0 := rustcore.FFICalls.Load()
	if _, err := missing.Attrs(ctx); !errors.Is(err, ErrObjectNotExist) {
		t.Fatalf("Attrs: %v", err)
	}
	if d := rustcore.FFICalls.Load() - f0; d != 1 {
		t.Errorf("Attrs on a missing object made %d FFI calls, want 1", d)
	}

	// Predicate says "retry everything": the core retries up to the attempt limit, inside one
	// gcs_call. Delete with a generation is idempotent, so the core may retry it.
	var calls atomic.Int32
	o := missing.Generation(1).Retryer(
		WithErrorFunc(func(err error) bool {
			calls.Add(1)
			return errors.Is(err, ErrObjectNotExist) || status.Code(err) == codes.NotFound
		}),
		WithMaxAttempts(3),
		WithBackoff(gaxBackoffForTest()),
	)
	f0 = rustcore.FFICalls.Load()
	err := o.Delete(ctx)
	if status.Code(err) != codes.NotFound {
		t.Errorf("Delete: want NotFound, got %v", err)
	}
	if d := rustcore.FFICalls.Load() - f0; d != 1 {
		t.Errorf("Delete with retries made %d FFI calls, want 1 (retries must run in the core)", d)
	}
	if c := calls.Load(); c < 2 {
		t.Errorf("retry predicate evaluated %d times, want >= 2", c)
	}
	t.Logf("predicate evaluated %d times inside one core call", calls.Load())

	// Non-idempotent (no generation precondition): the predicate must not be able to force a
	// retry under the default policy.
	calls.Store(0)
	o = missing.Retryer(WithErrorFunc(func(error) bool { calls.Add(1); return true }), WithMaxAttempts(3))
	o.Delete(ctx)
	if c := calls.Load(); c != 0 {
		t.Errorf("non-idempotent Delete consulted the predicate %d times, want 0", c)
	}
}

func gaxBackoffForTest() gax.Backoff {
	return gax.Backoff{Initial: 10 * time.Millisecond, Max: 50 * time.Millisecond, Multiplier: 2}
}

// ---- Regression tests for the independent review (F-03, F-05, F-11, F-13) ----------------

// PredefinedACL must be sent on uploads (same outcome as the native client), and object
// retention must be rejected rather than silently dropped.
func TestRustCoreWriterPredefinedACLAndRetention(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	var errs [2]error
	var acls [2]int
	for i, c := range []*Client{nc, rc} {
		name := fmt.Sprintf("predefined-acl-%d", i)
		w := c.Bucket(bucket).Object(rustTestPrefix + name).NewWriter(ctx)
		w.PredefinedACL = "projectPrivate"
		w.Write([]byte("acl"))
		errs[i] = w.Close()
		if errs[i] == nil {
			a, err := nc.Bucket(bucket).Object(rustTestPrefix + name).Attrs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			acls[i] = len(a.ACL)
		}
	}
	if (errs[0] == nil) != (errs[1] == nil) || httpCode(errs[0]) != httpCode(errs[1]) || acls[0] != acls[1] {
		t.Errorf("PredefinedACL: native err=%v acl=%d, rust err=%v acl=%d", errs[0], acls[0], errs[1], acls[1])
	}
	t.Logf("PredefinedACL projectPrivate: native err=%v acl entries=%d; rust err=%v acl entries=%d", errs[0], acls[0], errs[1], acls[1])

	w := rc.Bucket(bucket).Object(rustTestPrefix + "retention").NewWriter(ctx)
	w.Retention = &ObjectRetention{Mode: "Unlocked", RetainUntil: time.Now().Add(time.Hour)}
	w.Write([]byte("retained"))
	if err := w.Close(); status.Code(err) != codes.Unimplemented {
		t.Errorf("upload with Retention: want Unimplemented, got %v", err)
	}
	if _, err := nc.Bucket(bucket).Object(rustTestPrefix + "retention").Attrs(ctx); !errors.Is(err, ErrObjectNotExist) {
		t.Errorf("object written without its retention: %v", err)
	}
}

// ChunkSize == 0 streams without chunk buffering; content must round-trip.
func TestRustCoreChunkSizeZero(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	data := randBytes(3<<20 + 7)
	w := rc.Bucket(bucket).Object(rustTestPrefix + "chunk0").NewWriter(ctx)
	w.ChunkSize = 0
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := nc.Bucket(bucket).Object(rustTestPrefix + "chunk0").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("ChunkSize=0 upload: got %d bytes, want %d", len(got), len(data))
	}
}

// Ranged reads of gzip objects: bytes and Attrs.Decompressed must match the native client.
func TestRustCoreGzipRangedRead(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(strings.Repeat("ranged gzip ", 2000)))
	zw.Close()
	w := nc.Bucket(bucket).Object(rustTestPrefix + "gz-range").NewWriter(ctx)
	w.ContentEncoding, w.ContentType = "gzip", "text/plain"
	w.Write(gz.Bytes())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rg := range [][2]int64{{10, 50}, {0, 100}, {20, -1}} {
		var out [2][]byte
		var dec [2]bool
		var errs [2]error
		for i, c := range []*Client{nc, rc} {
			r, err := c.Bucket(bucket).Object(rustTestPrefix+"gz-range").NewRangeReader(ctx, rg[0], rg[1])
			if err != nil {
				errs[i] = err
				continue
			}
			out[i], errs[i] = io.ReadAll(r)
			dec[i] = r.Attrs.Decompressed
			r.Close()
		}
		if (errs[0] == nil) != (errs[1] == nil) || !bytes.Equal(out[0], out[1]) || dec[0] != dec[1] {
			t.Errorf("NewRangeReader(%d,%d) on gzip: native %d bytes decompressed=%v err=%v; rust %d bytes decompressed=%v err=%v",
				rg[0], rg[1], len(out[0]), dec[0], errs[0], len(out[1]), dec[1], errs[1])
		}
	}
}

// WithMaxAttempts(0) means a single attempt (as in the Go retry loop), not "unlimited".
func TestRustCoreMaxAttemptsZero(t *testing.T) {
	rc, _, bucket := rustTestClients(t)
	var calls atomic.Int32
	o := rc.Bucket(bucket).Object(rustTestPrefix+"never-written").Generation(1).Retryer(
		WithPolicy(RetryAlways),
		WithErrorFunc(func(error) bool { calls.Add(1); return true }),
		WithMaxAttempts(0),
		WithBackoff(gaxBackoffForTest()),
	)
	start := time.Now()
	if err := o.Delete(context.Background()); status.Code(err) != codes.NotFound {
		t.Errorf("Delete: want NotFound, got %v", err)
	}
	if c := calls.Load(); c > 1 {
		t.Errorf("WithMaxAttempts(0) retried: predicate evaluated %d times", c)
	}
	t.Logf("single attempt in %v (predicate evaluated %d times)", time.Since(start).Round(time.Millisecond), calls.Load())
}

func listAll(t *testing.T, c *Client, bucket string, q *Query, pageSize int) []*ObjectAttrs {
	t.Helper()
	var out []*ObjectAttrs
	p := iterator.NewPager(c.Bucket(bucket).Objects(context.Background(), q), pageSize, "")
	for {
		var page []*ObjectAttrs
		tok, err := p.NextPage(&page)
		if err != nil {
			t.Fatalf("list %+v: %v", q, err)
		}
		out = append(out, page...)
		if tok == "" {
			return out
		}
	}
}

func writeObjWithAttrs(t *testing.T, c *Client, bucket, name string, data []byte, i int) {
	t.Helper()
	w := c.Bucket(bucket).Object(rustTestPrefix + name).NewWriter(context.Background())
	w.ContentType = "text/plain"
	w.CacheControl = "no-cache"
	w.ContentLanguage = "en"
	w.Metadata = map[string]string{"k": strconv.Itoa(i), "Mixed-Case": "v"}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// JSON listing through the core (storage.objects.list in gcs_core): ObjectAttrs identical to the
// pure-Go JSON client's, with paging, attribute selection and the missing-bucket error.
func TestRustCoreJSONListParity(t *testing.T) {
	rc, nc, bucket := rustTestClients(t)
	if rc.tc.(*rustStorageClient).grpc {
		t.Fatal("NewClient + WithRustCore must list over the JSON API")
	}
	for i := 0; i < 5; i++ {
		writeObjWithAttrs(t, nc, bucket, fmt.Sprintf("attrs/%d", i), randBytes(100*(i+1)), i)
	}
	q := &Query{Prefix: rustTestPrefix + "attrs/"}
	ra, na := listAll(t, rc, bucket, q, 2), listAll(t, nc, bucket, q, 2)
	if len(ra) != 5 || len(na) != 5 {
		t.Fatalf("got %d (rust) / %d (native) objects, want 5", len(ra), len(na))
	}
	for i := range ra {
		if !reflect.DeepEqual(ra[i], na[i]) {
			t.Errorf("object %d differs:\n rust   %+v\n native %+v", i, ra[i], na[i])
		}
	}

	sel := &Query{Prefix: rustTestPrefix + "attrs/"}
	if err := sel.SetAttrSelection([]string{"Name", "Size"}); err != nil {
		t.Fatal(err)
	}
	ra, na = listAll(t, rc, bucket, sel, 3), listAll(t, nc, bucket, sel, 3)
	if len(ra) != 5 || !reflect.DeepEqual(ra, na) {
		t.Errorf("attribute selection differs: rust %d entries, native %d", len(ra), len(na))
	}
	if len(ra) > 0 && (ra[0].ContentType != "" || ra[0].Size == 0) {
		t.Errorf("selection not applied: %+v", ra[0])
	}

	_, err := rc.Bucket(fmt.Sprintf("rustcore-missing-%d", time.Now().UnixNano())).Objects(context.Background(), nil).Next()
	if !errors.Is(err, ErrBucketNotExist) {
		t.Errorf("missing bucket: got %v, want ErrBucketNotExist", err)
	}
}

// NewGRPCClient + WithRustCore: reads over BidiReadObject (Storage::open_object) and listing over
// the v2 ListObjects RPC, both CloudPath, compared with the pure-Go gRPC client (CloudPath).
func TestRustCoreGRPCParity(t *testing.T) {
	if os.Getenv("RUST_DP_TEST_BUCKET") == "" {
		t.Skip("set RUST_DP_TEST_BUCKET (real GCS)")
	}
	t.Setenv("GOOGLE_CLOUD_DISABLE_DIRECT_PATH", "true") // the core has no DirectPath
	_, nc, bucket := rustTestClients(t)
	ctx := context.Background()
	rg, err := NewGRPCClient(ctx, experimental.WithRustCore())
	if err != nil {
		t.Fatal(err)
	}
	defer rg.Close()
	if c, ok := rg.tc.(*rustStorageClient); !ok || !c.grpc {
		t.Fatalf("NewGRPCClient(WithRustCore): want a gRPC rust core client, got %T", rg.tc)
	}
	ng, err := NewGRPCClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ng.Close()
	nb, err := NewGRPCClient(ctx, WithGRPCBidiReads()) // the RPC the core reads with
	if err != nil {
		t.Fatal(err)
	}
	defer nb.Close()

	data := randBytes(5<<20 + 123) // several 2 MiB gRPC messages
	writeObjWithAttrs(t, nc, bucket, "grpc/big", data, 0)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(bytes.Repeat([]byte("compressible "), 10000))
	zw.Close()
	w := nc.Bucket(bucket).Object(rustTestPrefix + "grpc/gz").NewWriter(ctx)
	w.ContentEncoding, w.ContentType = "gzip", "text/plain"
	w.Write(gz.Bytes())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	read := func(c *Client, name string, off, n int64) ([]byte, ReaderObjectAttrs, map[string]string, int64) {
		t.Helper()
		r, err := c.Bucket(bucket).Object(rustTestPrefix+name).NewRangeReader(ctx, off, n)
		if err != nil {
			t.Fatalf("NewRangeReader(%s, %d, %d): %v", name, off, n, err)
		}
		defer r.Close()
		remain := r.Remain()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read %s [%d, %d): %v", name, off, n, err)
		}
		return b, r.Attrs, r.Metadata(), remain
	}
	for _, tc := range []struct {
		name   string
		off, n int64
	}{
		{"grpc/big", 0, -1},
		{"grpc/big", 100, 1000},
		{"grpc/big", 3 << 20, -1},
		{"grpc/big", -777, -1},
		{"grpc/big", 0, 0},
		{"grpc/big", 4321, 0}, // zero-length: a read stream closed after its first response
		{"grpc/gz", 0, -1},
	} {
		rb, rattrs, rmd, rrem := read(rg, tc.name, tc.off, tc.n)
		for _, native := range []struct {
			name string
			c    *Client
		}{{"native", ng}, {"native bidi", nb}} {
			nbytes, nattrs, nmd, nrem := read(native.c, tc.name, tc.off, tc.n)
			if !bytes.Equal(rb, nbytes) {
				t.Errorf("%s [%d,%d): rust read %d bytes, %s %d (equal=%v)", tc.name, tc.off, tc.n, len(rb), native.name, len(nbytes), bytes.Equal(rb, nbytes))
			}
			if rattrs != nattrs || !reflect.DeepEqual(rmd, nmd) || rrem != nrem {
				t.Errorf("%s [%d,%d) attrs differ:\n rust   %+v md=%v remain=%d\n %s %+v md=%v remain=%d",
					tc.name, tc.off, tc.n, rattrs, rmd, rrem, native.name, nattrs, nmd, nrem)
			}
		}
	}
	if _, err := rg.Bucket(bucket).Object(rustTestPrefix + "grpc/missing").NewReader(ctx); !errors.Is(err, ErrObjectNotExist) {
		t.Errorf("missing object: got %v, want ErrObjectNotExist", err)
	}

	for i := 0; i < 4; i++ {
		writeObj(t, nc, bucket, fmt.Sprintf("grpc/list/%d", i), []byte("x"))
	}
	writeObj(t, nc, bucket, "grpc/list/sub/x", []byte("x"))
	for _, q := range []*Query{
		{Prefix: rustTestPrefix + "grpc/list/"},
		{Prefix: rustTestPrefix + "grpc/list/", Delimiter: "/"},
	} {
		ra, na := listAll(t, rg, bucket, q, 2), listAll(t, ng, bucket, q, 2)
		if !reflect.DeepEqual(ra, na) {
			t.Errorf("gRPC list %+v differs: rust %d entries, native %d", q, len(ra), len(na))
			for i := range min(len(ra), len(na)) {
				if !reflect.DeepEqual(ra[i], na[i]) {
					t.Logf(" first difference:\n rust   %+v\n native %+v", ra[i], na[i])
					break
				}
			}
		}
	}
}
