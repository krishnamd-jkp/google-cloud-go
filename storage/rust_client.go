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

// Opt-in Rust data path (EXPERIMENTAL proof of concept).
//
// rustStorageClient implements storageClient by routing object *reads and writes* to the Rust
// engine (internal/rustcore) and delegating everything else — and every read/write whose
// options the engine cannot honour faithfully — to the regular JSON/XML client. The public API
// (Client, ObjectHandle, Reader, Writer, errors) is unchanged, so user code, the emulator test
// suite and the official benchmark harness run as-is.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/storage/internal/apiv2/storagepb"
	"cloud.google.com/go/storage/internal/rustcore"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/encoding/protojson"
)

const rustDataPathCompiled = true

// Counters: how often the engine served a request vs. fell back to the Go path.
var rustReads, rustWrites, rustReadFallbacks, rustWriteFallbacks atomic.Int64

type rustStorageClient struct {
	storageClient                    // control plane + fallbacks (JSON/XML client, possibly metrics-wrapped)
	http          *httpStorageClient // for settings
	core          *rustcore.Client
}

// tokenSourceFromAuth adapts cloud.google.com/go/auth credentials (whatever the user configured:
// ADC, WithCredentialsFile/JSON, WithTokenSource, impersonation, WIF, ...) for the engine.
type tokenSourceFromAuth struct{ creds *auth.Credentials }

func (t tokenSourceFromAuth) Token(ctx context.Context) (string, time.Time, error) {
	tok, err := t.creds.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok.Value, tok.Expiry, nil
}

// newRustStorageClient wraps base with the Rust data path. It returns base unchanged (pure Go)
// when the engine cannot authenticate the way the Go client would (e.g. a user-supplied
// http.Client), so enabling the option never changes *who* the client authenticates as.
func newRustStorageClient(ctx context.Context, base storageClient, endpoint string, creds *auth.Credentials, extraHeaders http.Header) (storageClient, error) {
	hc, ok := unwrapHTTPClient(base)
	if !ok {
		return base, nil
	}
	cfg := rustcore.ClientConfig{
		UserAgent: userAgent + " gccl-rust-core",
		Headers:   extraHeaders,
	}
	if host := os.Getenv("STORAGE_EMULATOR_HOST"); host != "" {
		if !strings.Contains(host, "://") {
			host = "http://" + host
		}
		cfg.Endpoint = strings.TrimSuffix(host, "/")
		cfg.Auth = rustcore.AuthAnonymous
	} else {
		if creds == nil {
			return base, nil // custom transport / no Go credentials: keep the pure-Go path
		}
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		cfg.Endpoint = u.Scheme + "://" + u.Host
		cfg.Auth = rustcore.AuthTokenSource
		cfg.TokenSource = tokenSourceFromAuth{creds}
		if qp, err := creds.QuotaProjectID(ctx); err == nil {
			cfg.QuotaProject = qp
		}
		if ud, err := creds.UniverseDomain(ctx); err == nil && ud != "" && ud != "googleapis.com" {
			cfg.UniverseDomain = ud
		}
	}
	core, err := rustcore.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &rustStorageClient{storageClient: base, http: hc, core: core}, nil
}

func unwrapHTTPClient(c storageClient) (*httpStorageClient, bool) {
	switch v := c.(type) {
	case *httpStorageClient:
		return v, true
	case *metricsStorageClient:
		return unwrapHTTPClient(v.storageClient)
	}
	return nil, false
}

func (c *rustStorageClient) Close() error {
	c.core.Close()
	return c.storageClient.Close()
}

func toRustConds(conds *Conditions) rustcore.Conds {
	var rc rustcore.Conds
	if conds == nil {
		return rc
	}
	p := func(v int64) *int64 { return &v }
	switch {
	case conds.DoesNotExist:
		rc.GenerationMatch = p(0)
	case conds.GenerationMatch != 0:
		rc.GenerationMatch = p(conds.GenerationMatch)
	}
	if conds.GenerationNotMatch != 0 {
		rc.GenerationNotMatch = p(conds.GenerationNotMatch)
	}
	if conds.MetagenerationMatch != 0 {
		rc.MetagenerationMatch = p(conds.MetagenerationMatch)
	}
	if conds.MetagenerationNotMatch != 0 {
		rc.MetagenerationNotMatch = p(conds.MetagenerationNotMatch)
	}
	return rc
}

// toRustRetry maps the retry configuration. ok is false for custom retry predicates
// (WithErrorFunc), which only the Go path can evaluate.
func toRustRetry(s *settings) (rr rustcore.Retry, ok bool) {
	r := s.retry
	if r == nil {
		return rr, true
	}
	if r.shouldRetry != nil {
		return rr, false
	}
	switch r.policy {
	case RetryAlways:
		rr.Mode = rustcore.RetryAlways
	case RetryNever:
		rr.Mode = rustcore.RetryNever
	}
	if r.maxAttempts != nil {
		rr.MaxAttempts = *r.maxAttempts
	}
	rr.MaxRetryMillis = r.maxRetryDuration.Milliseconds()
	if b := r.backoff; b != nil {
		rr.BackoffInitMs, rr.BackoffMaxMs, rr.BackoffMult = b.Initial.Milliseconds(), b.Max.Milliseconds(), b.Multiplier
	}
	return rr, true
}

// rustErr converts engine errors into the error types the pure-Go client returns.
func rustErr(err error, ctx context.Context, read bool) error {
	var e *rustcore.Error
	if !errors.As(err, &e) {
		if errors.Is(err, rustcore.ErrClosed) && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	switch {
	case e.Code == rustcore.CodeCancelled:
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return context.Canceled
	case read && e.HTTPCode == http.StatusNotFound:
		return ErrObjectNotExist
	case e.HTTPCode != 0:
		return &googleapi.Error{Code: e.HTTPCode, Message: e.Message, Header: e.Header, Body: e.Message}
	}
	return err
}

// ---- Reads -----------------------------------------------------------------------------------

type rustReaderAttrs struct {
	Size            int64             `json:"size"`
	Generation      int64             `json:"generation"`
	Metageneration  int64             `json:"metageneration"`
	ContentType     string            `json:"contentType"`
	ContentEncoding string            `json:"contentEncoding"`
	CRC32C          *uint32           `json:"crc32c"`
	Metadata        map[string]string `json:"metadata"`
	StartOffset     int64             `json:"startOffset"`
	Remain          int64             `json:"remain"`
	Decompressed    bool              `json:"decompressed"`
}

func (c *rustStorageClient) NewRangeReader(ctx context.Context, params *newRangeReaderParams, opts ...storageOption) (*Reader, error) {
	s := callSettings(c.http.settings, opts...)
	retry, ok := toRustRetry(s)
	// Zero-length reads are metadata reads (HEAD in the XML path); custom retry predicates and
	// requester-pays billing (userProject) stay on the Go path.
	if params.length == 0 || !ok || s.userProject != "" {
		rustReadFallbacks.Add(1)
		return c.storageClient.NewRangeReader(ctx, params, opts...)
	}
	rustReads.Add(1)
	r, err := c.core.OpenReader(ctx, rustcore.ReadParams{
		Bucket:          params.bucket,
		Object:          params.object,
		Generation:      params.gen,
		Offset:          params.offset,
		Length:          params.length,
		Conds:           toRustConds(params.conds),
		EncryptionKey:   params.encryptionKey,
		ReadCompressed:  params.readCompressed,
		DisableCRCCheck: params.disableCRCCheck,
		Retry:           retry,
	})
	if err != nil {
		return nil, rustErr(err, ctx, true)
	}
	var a rustReaderAttrs
	if err := json.Unmarshal([]byte(r.AttrsJSON), &a); err != nil {
		r.Close()
		return nil, err
	}
	var crc uint32
	if a.CRC32C != nil {
		crc = *a.CRC32C
	}
	md := a.Metadata
	if md == nil {
		md = map[string]string{}
	}
	return &Reader{
		Attrs: ReaderObjectAttrs{
			Size:            a.Size,
			StartOffset:     a.StartOffset,
			ContentType:     a.ContentType,
			ContentEncoding: a.ContentEncoding,
			// CacheControl and LastModified are not exposed by the engine's read response.
			Generation:     a.Generation,
			Metageneration: a.Metageneration,
			CRC32C:         crc,
			Decompressed:   a.Decompressed,
		},
		objectMetadata: &md,
		size:           a.Size,
		remain:         a.Remain,
		// CRC32C of full reads is validated inside the engine (no second pass in Go).
		reader: &rustReader{r: r, ctx: ctx},
		bucket: params.bucket,
		object: params.object,
	}, nil
}

type rustReader struct {
	r   *rustcore.Reader
	ctx context.Context
}

func (rr *rustReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(rr.ctx, p)
	if err != nil && err != io.EOF {
		err = rustErr(err, rr.ctx, true)
	}
	return n, err
}

func (rr *rustReader) Close() error {
	rr.r.Close()
	return nil
}

// ---- Writes ----------------------------------------------------------------------------------

func (c *rustStorageClient) OpenWriter(params *openWriterParams, opts ...storageOption) (internalWriter, error) {
	s := callSettings(c.http.settings, opts...)
	retry, ok := toRustRetry(s)
	a := params.attrs
	// Served by the Go path: appends (gRPC only), ChunkSize == 0 (single streaming request with
	// no retries), per-chunk deadlines, custom retry predicates, requester-pays, and object
	// attributes the engine does not map yet.
	if params.append || params.chunkSize <= 0 || params.chunkRetryDeadline != 0 || params.chunkTransferTimeout != 0 ||
		!ok || s.userProject != "" || a.Retention != nil || a.Contexts != nil {
		rustWriteFallbacks.Add(1)
		return c.storageClient.OpenWriter(params, opts...)
	}
	rustWrites.Add(1)
	return &rustInternalWriter{
		core: c.core,
		wp: rustcore.WriteParams{
			Bucket:        params.bucket,
			Object:        a.Name,
			Conds:         toRustConds(params.conds),
			EncryptionKey: params.encryptionKey,
			ChunkSize:     params.chunkSize,
			Retry:         retry,
		},
		params:    params,
		needSniff: a.ContentType == "" && !params.forceEmptyContentType,
	}, nil
}

// writerAttrsJSON maps the editable ObjectAttrs (the same set the JSON path sends).
func writerAttrsJSON(a *ObjectAttrs, sendCRC32C bool, contentType string) ([]byte, error) {
	m := map[string]any{}
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put("contentType", contentType)
	put("contentEncoding", a.ContentEncoding)
	put("contentLanguage", a.ContentLanguage)
	put("contentDisposition", a.ContentDisposition)
	put("cacheControl", a.CacheControl)
	put("storageClass", a.StorageClass)
	put("kmsKeyName", a.KMSKeyName)
	put("predefinedAcl", a.PredefinedACL)
	if len(a.Metadata) > 0 {
		m["metadata"] = a.Metadata
	}
	if !a.CustomTime.IsZero() {
		m["customTimeUnixNanos"] = a.CustomTime.UnixNano()
	}
	if a.TemporaryHold {
		m["temporaryHold"] = true
	}
	if a.EventBasedHold {
		m["eventBasedHold"] = true
	}
	if len(a.MD5) > 0 {
		m["md5"] = base64.StdEncoding.EncodeToString(a.MD5)
	}
	if sendCRC32C {
		m["crc32c"] = a.CRC32C
	}
	if len(a.ACL) > 0 {
		var acl []map[string]string
		for _, r := range a.ACL {
			acl = append(acl, map[string]string{"entity": string(r.Entity), "role": string(r.Role)})
		}
		m["acl"] = acl
	}
	return json.Marshal(m)
}

// rustInternalWriter adapts rustcore.Writer to storage.Writer's internalWriter contract:
// Write/Close drive the upload, CloseWithError (ctx cancellation) aborts it without
// finalizing, and donec is closed exactly once when the upload is over.
type rustInternalWriter struct {
	core   *rustcore.Client
	wp     rustcore.WriteParams
	params *openWriterParams

	needSniff bool
	sniff     []byte // first bytes, for Content-Type detection like googleapi media uploads
	written   int64

	mu       sync.Mutex
	w        *rustcore.Writer
	aborted  bool
	doneOnce sync.Once
}

const sniffLen = 512

func (iw *rustInternalWriter) finish() {
	iw.doneOnce.Do(func() {
		iw.mu.Lock()
		if iw.w != nil {
			iw.w.Free()
		}
		iw.mu.Unlock()
		close(iw.params.donec)
	})
}

func (iw *rustInternalWriter) isAborted() bool {
	iw.mu.Lock()
	defer iw.mu.Unlock()
	return iw.aborted
}

// fail records the upload's terminal error (unless it was aborted: then the abort reason,
// already recorded by storage.Writer, wins) and ends the upload.
func (iw *rustInternalWriter) fail(err error) error {
	err = rustErr(err, iw.params.ctx, false)
	if !iw.isAborted() {
		iw.params.setError(err)
	}
	iw.finish()
	return err
}

// open starts the engine upload once the content type is known.
func (iw *rustInternalWriter) open() (*rustcore.Writer, error) {
	ct := iw.params.attrs.ContentType
	if iw.needSniff {
		ct = http.DetectContentType(iw.sniff)
	}
	js, err := writerAttrsJSON(iw.params.attrs, iw.params.sendCRC32C, ct)
	if err != nil {
		return nil, err
	}
	wp := iw.wp
	wp.AttrsJSON = js
	iw.mu.Lock()
	if iw.aborted {
		iw.mu.Unlock()
		return nil, context.Canceled
	}
	w, err := iw.core.OpenWriter(wp)
	if err != nil {
		iw.mu.Unlock()
		return nil, err
	}
	iw.w = w
	iw.mu.Unlock()
	if len(iw.sniff) > 0 {
		if _, err := w.Write(iw.params.ctx, iw.sniff); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (iw *rustInternalWriter) writer() *rustcore.Writer {
	iw.mu.Lock()
	defer iw.mu.Unlock()
	return iw.w
}

func (iw *rustInternalWriter) Write(p []byte) (int, error) {
	w := iw.writer()
	consumed := 0
	if w == nil {
		if iw.needSniff {
			take := min(sniffLen-len(iw.sniff), len(p))
			iw.sniff = append(iw.sniff, p[:take]...)
			consumed = take
			if len(iw.sniff) < sniffLen {
				return len(p), nil // still collecting the sniff window
			}
		}
		var err error
		if w, err = iw.open(); err != nil {
			return 0, iw.fail(err)
		}
	}
	if rest := p[consumed:]; len(rest) > 0 {
		if _, err := w.Write(iw.params.ctx, rest); err != nil {
			return 0, iw.fail(err)
		}
	}
	// Progress reports bytes accepted by the engine (the JSON path reports bytes sent).
	iw.written += int64(len(p))
	iw.params.progress(iw.written)
	return len(p), nil
}

func (iw *rustInternalWriter) Close() error {
	w := iw.writer()
	if w == nil {
		var err error
		if w, err = iw.open(); err != nil {
			iw.fail(err)
			return nil
		}
	}
	js, err := w.Close(iw.params.ctx)
	if err != nil {
		iw.fail(err) // surfaces through Writer.Close (w.err), as with the JSON path
		return nil
	}
	var po storagepb.Object
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(js, &po); err != nil {
		iw.fail(err)
		return nil
	}
	iw.params.setObj(newObjectFromProto(&po))
	iw.finish()
	return nil
}

// CloseWithError aborts the upload (never finalizing it). Called by storage.Writer when its
// context is done; must not block.
func (iw *rustInternalWriter) CloseWithError(err error) error {
	iw.mu.Lock()
	iw.aborted = true
	w := iw.w
	iw.mu.Unlock()
	if w != nil {
		w.Abort() // a pending Write/Close returns promptly with an error
	}
	iw.params.setError(err)
	iw.finish()
	return nil
}

func (iw *rustInternalWriter) Flush() (int64, error) {
	return 0, errors.New("Writer.Flush is only supported for gRPC-based clients")
}

func (iw *rustInternalWriter) setAppendFinalCRC32C(bool, uint32) {}

// rustDataPathStats reports engine usage (for tests and benchmarks).
func rustDataPathStats() (reads, writes, readFallbacks, writeFallbacks int64) {
	return rustReads.Load(), rustWrites.Load(), rustReadFallbacks.Load(), rustWriteFallbacks.Load()
}

func rustEngineStats() map[string]int64 {
	return map[string]int64{
		"reads": rustReads.Load(), "writes": rustWrites.Load(),
		"read_fallbacks": rustReadFallbacks.Load(), "write_fallbacks": rustWriteFallbacks.Load(),
		"ffi_calls": rustcore.FFICalls.Load(), "async_ops": rustcore.AsyncOps.Load(),
	}
}
