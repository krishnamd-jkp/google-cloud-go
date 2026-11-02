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

package rustcore

/*
#include <stdlib.h>
#include "gcs_core.h"

extern void rustcoreDone(uintptr_t user, GcsStatus st, uint64_t n);
extern int32_t rustcoreToken(uintptr_t user, char **tok, int64_t *exp, char **err);
extern int32_t rustcoreRetry(uintptr_t predicate, GcsStatus st, uint32_t attempt);

// Thin trampolines: Go cannot take the address of an exported Go function, and doing the
// integer<->pointer conversions here keeps them out of Go (no unsafe.Pointer(uintptr(x))).
static GcsStatus rc_client_new(GcsClientConfig *cfg, GcsClient **out, int token_cb) {
	if (token_cb) cfg->token_cb = rustcoreToken;
	cfg->retry_cb = rustcoreRetry;
	return gcs_client_new(cfg, out);
}
static int32_t rc_call(const GcsClient *c, const char *method, const uint8_t *req, size_t req_len,
                       const uint8_t *opts, size_t opts_len, uintptr_t user, uint64_t *op,
                       GcsStatus *st) {
	return gcs_call(c, method, req, req_len, opts, opts_len, rustcoreDone, user, op, st);
}
static int32_t rc_read_open(const GcsClient *c, const uint8_t *req, size_t req_len,
                            const uint8_t *opts, size_t opts_len, uintptr_t user, uint64_t *op,
                            GcsStatus *st) {
	return gcs_read_open(c, req, req_len, opts, opts_len, rustcoreDone, user, op, st);
}
static int32_t rc_reader_read(const GcsReader *r, uint8_t *buf, size_t cap, uintptr_t user,
                              size_t *n, GcsStatus *st) {
	return gcs_reader_read(r, buf, cap, rustcoreDone, user, n, st);
}
static int32_t rc_writer_write(const GcsWriter *w, const uint8_t *d, size_t len, uintptr_t user,
                               GcsStatus *st) {
	return gcs_writer_write(w, d, len, rustcoreDone, user, st);
}
static int32_t rc_writer_close(const GcsWriter *w, uintptr_t user, GcsStatus *st) {
	return gcs_writer_close(w, rustcoreDone, user, st);
}
static GcsReader *rc_as_reader(uint64_t v) { return (GcsReader *)(uintptr_t)v; }
static char *rc_as_cstr(uint64_t v) { return (char *)(uintptr_t)v; }
static int32_t rc_async_noop(uintptr_t user) { return gcs_async_noop(rustcoreDone, user); }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"runtime/cgo"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Package rustcore is the Go binding of the Rust core's C ABI (gcs_core.h). It is generic: it
// moves bytes (proto3-JSON requests/responses, options JSON, object data) across the boundary
// and knows nothing about Cloud Storage semantics, so it does not change when the API grows.

// Available reports whether the Rust core is compiled into this binary.
const Available = true

// ABIVersion is the gcs_core ABI this binding was written against.
const ABIVersion = 3

// Status codes (mirror GCS_* in gcs_core.h).
const (
	CodeOK        = 0
	CodeErr       = 1
	CodePanic     = 2
	CodeInvalid   = 3
	CodeCancelled = 4
)

// Error is a failure reported by the core. Both codes are always filled in when known (the
// core derives the missing one), so callers can rebuild gRPC- or HTTP-style errors regardless
// of which transport the core used.
type Error struct {
	Code     int // CodeErr, CodePanic, CodeInvalid, CodeCancelled
	HTTPCode int // HTTP status, 0 if none
	RPCCode  int // google.rpc.Code, -1 if unknown
	Message  string
	Header   http.Header
}

func (e *Error) Error() string {
	if e.HTTPCode != 0 {
		return fmt.Sprintf("gcs rust core: http %d: %s", e.HTTPCode, e.Message)
	}
	return "gcs rust core: " + e.Message
}

// Counters for benchmarks and tests (crossings into the core).
var (
	FFICalls  atomic.Int64 // cgo calls into gcs_core
	AsyncOps  atomic.Int64 // calls that completed asynchronously (Rust->Go callback)
	ErrClosed = errors.New("storage: rust core handle is closed")
)

func takeStatus(st C.GcsStatus) *Error {
	defer func() {
		C.gcs_string_free(st.message)
		C.gcs_string_free(st.headers)
	}()
	if st.code == CodeOK {
		return nil
	}
	e := &Error{Code: int(st.code), HTTPCode: int(st.http_code), RPCCode: int(st.rpc_code)}
	if st.message != nil {
		e.Message = C.GoString(st.message)
	}
	if st.headers != nil {
		e.Header = http.Header{}
		for _, line := range strings.Split(C.GoString(st.headers), "\n") {
			if k, v, ok := strings.Cut(line, ": "); ok {
				e.Header.Add(k, v)
			}
		}
	}
	return e
}

// cmem allocates C memory and frees it together.
type cmem []unsafe.Pointer

func (c *cmem) str(s string) *C.char {
	if s == "" {
		return nil
	}
	p := C.CString(s)
	*c = append(*c, unsafe.Pointer(p))
	return p
}

func (c *cmem) bytes(b []byte) (*C.uint8_t, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	p := C.CBytes(b)
	*c = append(*c, p)
	return (*C.uint8_t)(p), C.size_t(len(b))
}

func (c *cmem) free() {
	for _, p := range *c {
		C.free(p)
	}
}

// takeCString copies and frees a Rust-owned C string delivered as a completion value.
func takeCString(n uint64) []byte {
	cs := C.rc_as_cstr(C.uint64_t(n))
	if cs == nil {
		return nil
	}
	b := []byte(C.GoString(cs))
	C.gcs_string_free(cs)
	return b
}

// CoreABIVersion reports the ABI version of the linked core.
func CoreABIVersion() int { return int(C.gcs_abi_version()) }

// ConfigureRuntime sets the core's worker thread count; must run before the first client.
func ConfigureRuntime(workerThreads int) bool {
	return C.gcs_runtime_configure(C.uint32_t(workerThreads)) == 1
}

// Noop measures the raw cost of one Go->Rust call.
func Noop(x uint64) uint64 { return uint64(C.gcs_noop(C.uint64_t(x))) }

// AsyncNoopper measures one asynchronous completion (spawn + foreign-thread callback + wake-up).
type AsyncNoopper struct {
	done chan result
	h    cgo.Handle
}

// NewAsyncNoopper allocates the reusable completion channel/handle.
func NewAsyncNoopper() *AsyncNoopper {
	a := &AsyncNoopper{done: make(chan result, 1)}
	a.h = cgo.NewHandle(a.done)
	return a
}

// Do runs one async no-op round trip.
func (a *AsyncNoopper) Do() {
	if C.rc_async_noop(C.uintptr_t(a.h)) == 1 {
		<-a.done
	}
}

// Close releases the handle.
func (a *AsyncNoopper) Close() { a.h.Delete() }

// ---- Client ----------------------------------------------------------------------------------

// Auth selects how the core authenticates.
type Auth int

const (
	AuthADC Auth = iota
	AuthAnonymous
	AuthJSON
	AuthTokenSource
)

// ClientConfig configures the core client.
type ClientConfig struct {
	Endpoint        string // JSON/data path, e.g. https://storage.googleapis.com ; empty = default
	GRPCEndpoint    string // control plane, e.g. http://localhost:8888 ; empty = default
	UniverseDomain  string
	Auth            Auth
	CredentialsJSON []byte
	TokenSource     TokenSource // for AuthTokenSource
	QuotaProject    string
	UserAgent       string
	Headers         http.Header // sent on every data-path request
}

// Client is a handle to a core client. Safe for concurrent use.
type Client struct {
	mu     sync.RWMutex
	p      *C.GcsClient
	tokenH cgo.Handle
}

// NewClient creates a core client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if v := CoreABIVersion(); v != ABIVersion {
		return nil, fmt.Errorf("rustcore: core ABI v%d, binding expects v%d", v, ABIVersion)
	}
	var cm cmem
	defer cm.free()
	var hdr strings.Builder
	for k, vs := range cfg.Headers {
		for _, v := range vs {
			fmt.Fprintf(&hdr, "%s: %s\n", k, v)
		}
	}
	ccfg := C.GcsClientConfig{
		endpoint:         cm.str(cfg.Endpoint),
		grpc_endpoint:    cm.str(cfg.GRPCEndpoint),
		universe_domain:  cm.str(cfg.UniverseDomain),
		credentials_json: cm.str(string(cfg.CredentialsJSON)),
		quota_project:    cm.str(cfg.QuotaProject),
		user_agent:       cm.str(cfg.UserAgent),
		custom_headers:   cm.str(hdr.String()),
		auth_mode:        C.int32_t(cfg.Auth),
	}
	c := &Client{}
	useCB := C.int(0)
	if cfg.Auth == AuthTokenSource {
		if cfg.TokenSource == nil {
			return nil, errors.New("rustcore: AuthTokenSource requires a TokenSource")
		}
		c.tokenH = cgo.NewHandle(cfg.TokenSource)
		ccfg.token_user = C.uintptr_t(c.tokenH)
		useCB = 1
	}
	FFICalls.Add(1)
	if err := takeStatus(C.rc_client_new(&ccfg, &c.p, useCB)); err != nil {
		if c.tokenH != 0 {
			c.tokenH.Delete()
		}
		return nil, err
	}
	return c, nil
}

// Close releases the client. Calls, readers and writers still in flight keep working.
// The token handle is intentionally kept alive (in-flight operations may still refresh tokens).
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.p != nil {
		C.gcs_client_free(c.p)
		c.p = nil
	}
}

// ---- Unary calls -----------------------------------------------------------------------------

// Call runs one unary RPC of google.storage.v2.Storage. req and the result are proto3 JSON;
// opts is the options JSON (may be nil). If ctx is done first the call is cancelled.
func (c *Client) Call(ctx context.Context, method string, req, opts []byte) ([]byte, error) {
	var cm cmem
	defer cm.free()
	m := cm.str(method)
	rp, rl := cm.bytes(req)
	op, ol := cm.bytes(opts)

	done := make(chan result, 1)
	h := cgo.NewHandle(done)
	defer h.Delete() // safe: we always receive the single completion before returning

	c.mu.RLock()
	if c.p == nil {
		c.mu.RUnlock()
		return nil, ErrClosed
	}
	var opID C.uint64_t
	var st C.GcsStatus
	FFICalls.Add(1)
	rc := C.rc_call(c.p, m, rp, rl, op, ol, C.uintptr_t(h), &opID, &st)
	c.mu.RUnlock()
	if rc < 0 {
		return nil, takeStatus(st)
	}
	AsyncOps.Add(1)
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		C.gcs_op_cancel(opID)
		res = <-done
		if res.err == nil {
			takeCString(res.n) // completed anyway: free the response
		}
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, res.err
	}
	return takeCString(res.n), nil
}

// ---- Reader ----------------------------------------------------------------------------------

// Reader streams an object. Read must not be called concurrently with itself; Close may be
// called concurrently with Read (the pending Read fails promptly).
type Reader struct {
	// Metadata is the open-time envelope: {"metadata": <v2 Object>, "readOffset", "remaining",
	// "decompressed"}.
	Metadata []byte

	readMu sync.Mutex // serializes Read

	mu      sync.Mutex // guards the fields below
	p       *C.GcsReader
	closed  bool
	pending bool
	h       cgo.Handle
	hFreed  bool
	done    chan result
}

// OpenReader starts a read (req: proto3-JSON ReadObjectRequest). If ctx is done first the
// open is cancelled.
func (c *Client) OpenReader(ctx context.Context, req, opts []byte) (*Reader, error) {
	var cm cmem
	defer cm.free()
	rp, rl := cm.bytes(req)
	op, ol := cm.bytes(opts)

	done := make(chan result, 1)
	h := cgo.NewHandle(done)
	defer h.Delete() // safe: we always receive the single completion before returning

	c.mu.RLock()
	if c.p == nil {
		c.mu.RUnlock()
		return nil, ErrClosed
	}
	var opID C.uint64_t
	var st C.GcsStatus
	FFICalls.Add(1)
	rc := C.rc_read_open(c.p, rp, rl, op, ol, C.uintptr_t(h), &opID, &st)
	c.mu.RUnlock()
	if rc < 0 {
		return nil, takeStatus(st)
	}
	AsyncOps.Add(1)
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		C.gcs_op_cancel(opID)
		res = <-done
		if res.err == nil {
			C.gcs_reader_close(C.rc_as_reader(C.uint64_t(res.n)))
		}
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, res.err
	}
	r := &Reader{p: C.rc_as_reader(C.uint64_t(res.n)), done: make(chan result, 1)}
	r.h = cgo.NewHandle(r.done)
	r.Metadata = []byte(C.GoString(C.gcs_reader_metadata(r.p)))
	runtime.SetFinalizer(r, (*Reader).Close) // backstop for leaked readers
	return r, nil
}

// Read reads into p (io.Reader contract; returns io.EOF at the end). ctx cancels a pending read
// and fails the reader.
func (r *Reader) Read(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.readMu.Lock()
	defer r.readMu.Unlock()

	var pin runtime.Pinner
	pin.Pin(&p[0])
	defer pin.Unpin()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, ErrClosed
	}
	var n C.size_t
	var st C.GcsStatus
	FFICalls.Add(1)
	rc := C.rc_reader_read(r.p, (*C.uint8_t)(unsafe.Pointer(&p[0])), C.size_t(len(p)), C.uintptr_t(r.h), &n, &st)
	if rc == 1 {
		r.pending = true
	}
	r.mu.Unlock()

	switch rc {
	case 0:
		if n == 0 {
			return 0, io.EOF
		}
		return int(n), nil
	case -1:
		return 0, takeStatus(st)
	}
	AsyncOps.Add(1)
	var res result
	select {
	case res = <-r.done:
	case <-ctx.Done():
		r.cancel()
		res = <-r.done
		if res.err != nil || res.n == 0 {
			res = result{err: &Error{Code: CodeCancelled, RPCCode: 1, HTTPCode: 499, Message: ctx.Err().Error()}}
		}
	}
	r.mu.Lock()
	r.pending = false
	if r.closed && !r.hFreed {
		r.h.Delete()
		r.hFreed = true
	}
	r.mu.Unlock()
	if res.err != nil {
		return 0, res.err
	}
	if res.n == 0 {
		return 0, io.EOF
	}
	return int(res.n), nil
}

func (r *Reader) cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		FFICalls.Add(1)
		C.gcs_reader_cancel(r.p)
	}
}

// Close releases the reader; a concurrent pending Read fails promptly. Idempotent.
func (r *Reader) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	FFICalls.Add(1)
	C.gcs_reader_close(r.p)
	r.p = nil
	if !r.pending {
		r.h.Delete()
		r.hFreed = true
	}
	runtime.SetFinalizer(r, nil)
}

// ---- Writer ----------------------------------------------------------------------------------

// Writer uploads an object. Write/Close must not be called concurrently with each other;
// Abort may be called from any goroutine at any time.
type Writer struct {
	opMu sync.Mutex // serializes Write/Close

	mu      sync.Mutex
	p       *C.GcsWriter
	freed   bool
	pending bool
	closing bool // a close is pending: its completion carries the object JSON
	h       cgo.Handle
	hFreed  bool
	done    chan result

	small []byte // coalesces tiny writes (fewer crossings)
}

const (
	coalesceBelow = 16 << 10
	coalesceCap   = 256 << 10
	// maxMsg bounds each message handed to the core. The core's queue is bounded by message
	// count, so splitting large writes bounds Rust-side buffering by *bytes* (queue depth x
	// maxMsg) instead of growing with len(p).
	maxMsg = 1 << 20
)

// OpenWriter prepares an upload (req: proto3-JSON StartResumableWriteRequest shape); nothing
// is sent until data is written or the writer closed.
func (c *Client) OpenWriter(req, opts []byte) (*Writer, error) {
	var cm cmem
	defer cm.free()
	rp, rl := cm.bytes(req)
	op, ol := cm.bytes(opts)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.p == nil {
		return nil, ErrClosed
	}
	w := &Writer{done: make(chan result, 1)}
	var st C.GcsStatus
	FFICalls.Add(1)
	if C.gcs_write_open(c.p, rp, rl, op, ol, &w.p, &st) < 0 {
		return nil, takeStatus(st)
	}
	w.h = cgo.NewHandle(w.done)
	runtime.SetFinalizer(w, (*Writer).Free) // a leaked writer aborts instead of leaking a task
	return w, nil
}

// Write queues p. Returns once the core has copied p (backpressure applies).
func (w *Writer) Write(ctx context.Context, p []byte) (int, error) {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if len(p) < coalesceBelow {
		if len(w.small)+len(p) <= coalesceCap {
			w.small = append(w.small, p...)
			return len(p), nil
		}
		if err := w.flushSmall(ctx); err != nil {
			return 0, err
		}
		w.small = append(w.small, p...)
		return len(p), nil
	}
	if err := w.flushSmall(ctx); err != nil {
		return 0, err
	}
	for off := 0; off < len(p); off += maxMsg {
		if err := w.write(ctx, p[off:min(off+maxMsg, len(p))]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

func (w *Writer) flushSmall(ctx context.Context) error {
	if len(w.small) == 0 {
		return nil
	}
	err := w.write(ctx, w.small)
	w.small = w.small[:0]
	return err
}

func (w *Writer) write(ctx context.Context, p []byte) error {
	var pin runtime.Pinner
	pin.Pin(&p[0])
	defer pin.Unpin()
	w.mu.Lock()
	if w.freed {
		w.mu.Unlock()
		return ErrClosed
	}
	var st C.GcsStatus
	FFICalls.Add(1)
	rc := C.rc_writer_write(w.p, (*C.uint8_t)(unsafe.Pointer(&p[0])), C.size_t(len(p)), C.uintptr_t(w.h), &st)
	if rc == 1 {
		w.pending = true
	}
	w.mu.Unlock()
	switch rc {
	case 0:
		return nil
	case -1:
		return takeStatus(st)
	}
	AsyncOps.Add(1)
	res := w.wait(ctx)
	return errOrNil(res.err)
}

// wait receives the completion of the single pending op; on ctx.Done it aborts the upload
// (never finalizing it) and still waits for the completion before releasing pinned memory.
func (w *Writer) wait(ctx context.Context) result {
	var res result
	select {
	case res = <-w.done:
	case <-ctx.Done():
		w.Abort()
		res = <-w.done
		if res.err == nil && res.n != 0 && w.closing {
			takeCString(res.n) // the close completed anyway: free the object JSON
		}
		res = result{err: &Error{Code: CodeCancelled, RPCCode: 1, HTTPCode: 499, Message: ctx.Err().Error()}}
	}
	w.mu.Lock()
	w.pending = false
	if w.freed && !w.hFreed {
		w.h.Delete()
		w.hFreed = true
	}
	w.mu.Unlock()
	return res
}

func errOrNil(e *Error) error {
	if e == nil {
		return nil
	}
	return e
}

// Close finalizes the upload and returns the created object as proto3 JSON
// (google.storage.v2.Object). If ctx is done first the upload is aborted, not finalized.
func (w *Writer) Close(ctx context.Context) ([]byte, error) {
	w.opMu.Lock()
	defer w.opMu.Unlock()
	if err := w.flushSmall(ctx); err != nil {
		return nil, err
	}
	w.mu.Lock()
	if w.freed {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	var st C.GcsStatus
	FFICalls.Add(1)
	rc := C.rc_writer_close(w.p, C.uintptr_t(w.h), &st)
	if rc == 1 {
		w.pending = true
		w.closing = true
	}
	w.mu.Unlock()
	if rc < 0 {
		return nil, takeStatus(st)
	}
	AsyncOps.Add(1)
	res := w.wait(ctx)
	if res.err != nil {
		return nil, res.err
	}
	return takeCString(res.n), nil
}

// Abort cancels the upload without finalizing the object. Safe from any goroutine; idempotent.
func (w *Writer) Abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.freed {
		FFICalls.Add(1)
		C.gcs_writer_abort(w.p)
	}
}

// Free aborts the upload if it is still running and releases the handle. Idempotent.
func (w *Writer) Free() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.freed {
		return
	}
	w.freed = true
	FFICalls.Add(1)
	C.gcs_writer_free(w.p)
	w.p = nil
	if !w.pending {
		w.h.Delete()
		w.hFreed = true
	}
	runtime.SetFinalizer(w, nil)
}
