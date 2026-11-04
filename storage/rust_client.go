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

// EXPERIMENTAL: the Go SDK on the shared Rust core.
//
// With experimental.WithRustCore(), every storageClient method is served by the Rust core
// (internal/rustcore, gcs_core/), the same core every language SDK binds to:
//
//   - Metadata RPCs (buckets, objects, list, compose, rewrite, move, restore, IAM, ACLs) run
//     through grpcStorageClient — the existing mapping between the Go API and the
//     google.storage.v2 protos — with its gRPC stub replaced by rustRPC (rust_transport.go).
//     The Go retry loop does not run: the core executes the caller's retry policy and decides
//     idempotency (see run() in invoke.go).
//   - Object reads and writes use the core's streaming API (this file).
//
// What remains in Go is inherently per-language: the public API, the Go<->proto mapping (shared
// with the gRPC transport) and Go-specific API semantics (e.g. Writer's Content-Type sniffing).
// Transport, auth, retries, idempotency, resumable uploads, checksums, read resumption,
// decompression and error classification live in the core.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/storage/internal/apiv2/storagepb"
	"cloud.google.com/go/storage/internal/rustcore"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option/internaloption"
	raw "google.golang.org/api/storage/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const rustCoreCompiled = true

type rustStorageClient struct {
	*grpcStorageClient // Go API <-> v2 proto mapping; RPCs served by rustRPC
	core               *rustcore.Client
	// grpc selects gRPC (CloudPath) for object reads and listing: NewGRPCClient with
	// experimental.WithRustCore. Otherwise (NewClient) reads and listing use the JSON API.
	// Writes use the JSON API either way: the Rust SDK has no gRPC upload for standard buckets.
	grpc bool
}

// rustUseGRPC switches a Rust core client to gRPC reads and listing (see rustStorageClient).
func rustUseGRPC(tc storageClient) {
	if c, ok := tc.(*rustStorageClient); ok {
		c.grpc = true
	}
}

// tokenSourceFromAuth adapts cloud.google.com/go/auth credentials (whatever the user configured:
// ADC, WithCredentialsFile/JSON, WithTokenSource, impersonation, WIF, ...) for the core.
type tokenSourceFromAuth struct{ creds *auth.Credentials }

func (t tokenSourceFromAuth) Token(ctx context.Context) (string, time.Time, error) {
	tok, err := t.creds.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok.Value, tok.Expiry, nil
}

func emulatorURL(host string) string {
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	return strings.TrimSuffix(host, "/")
}

// rustEndpoints derives the core's endpoints from the resolved JSON endpoint (the default
// "https://storage.googleapis.com/storage/v1/", a regional, PSC or restricted endpoint, or a
// proxy with a path prefix): the data path keeps any path prefix, the control plane uses the
// same scheme and host.
func rustEndpoints(endpoint string) (jsonEP, grpcEP string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("storage: endpoint %q is not an absolute URL", endpoint)
	}
	jsonEP = strings.TrimSuffix(strings.TrimSuffix(endpoint, "/"), "/storage/v1")
	return jsonEP, u.Scheme + "://" + u.Host, nil
}

func rustWithoutAuthentication(s *settings) bool {
	res, err := internaloption.NewUnsafeResolver(s.clientOption...)
	return err == nil && res.ResolvedWithoutAuthentication()
}

// rustGRPCConnPoolSize is the number of gRPC connections the core opens for the data path: the
// client's option.WithGRPCConnectionPool, else the Go SDK's default, so that a Rust core client
// holds as many connections as the Go gRPC client it stands in for (the Rust SDK's own default
// is one connection per CPU).
func rustGRPCConnPoolSize(s *settings) int {
	if res, err := internaloption.NewUnsafeResolver(s.clientOption...); err == nil {
		if n := res.ResolvedGRPCConnPoolSize(); n > 0 {
			return n
		}
	}
	return defaultConnPoolSize
}

// newRustCoreClient returns a storageClient served entirely by the Rust core. endpoint and
// creds are the JSON endpoint and credentials NewClient resolved from the user's options.
func newRustCoreClient(ctx context.Context, endpoint string, creds *auth.Credentials, extraHeaders http.Header, opts ...storageOption) (storageClient, error) {
	s := initSettings(opts...)
	s.engineRetries = true
	config := newStorageConfig(s.clientOption...)

	cfg := rustcore.ClientConfig{UserAgent: userAgent + " gccl-rust-core", Headers: extraHeaders,
		GRPCSubchannels: rustGRPCConnPoolSize(s)}
	if host := os.Getenv("STORAGE_EMULATOR_HOST"); host != "" {
		cfg.Endpoint = emulatorURL(host)
		cfg.GRPCEndpoint = cfg.Endpoint
		if g := os.Getenv("STORAGE_EMULATOR_HOST_GRPC"); g != "" {
			cfg.GRPCEndpoint = emulatorURL(g)
		}
		cfg.Auth = rustcore.AuthAnonymous
	} else {
		jsonEP, grpcEP, err := rustEndpoints(endpoint)
		if err != nil {
			return nil, err
		}
		// Both planes follow the configured endpoint (option.WithEndpoint, universe domain).
		cfg.Endpoint, cfg.GRPCEndpoint = jsonEP, grpcEP
		switch {
		case rustWithoutAuthentication(s):
			cfg.Auth = rustcore.AuthAnonymous
		case creds == nil:
			return nil, errors.New("storage: the rust core cannot use this client's credentials; custom HTTP clients, gRPC connections and API keys are not supported")
		default:
			cfg.Auth = rustcore.AuthTokenSource
			cfg.TokenSource = tokenSourceFromAuth{creds}
			if qp, err := creds.QuotaProjectID(ctx); err == nil {
				cfg.QuotaProject = qp
			}
			if ud, err := creds.UniverseDomain(ctx); err == nil && ud != "" && ud != "googleapis.com" {
				cfg.UniverseDomain = ud
			}
		}
	}
	core, err := rustcore.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	gc := &grpcStorageClient{raw: &rustRPC{core: core}, settings: s, config: &config}
	return &rustStorageClient{grpcStorageClient: gc, core: core}, nil
}

func (c *rustStorageClient) Close() error {
	return c.grpcStorageClient.Close() // closes rustRPC, i.e. the core client
}

// NewMultiRangeDownloader is not wired to the core yet (the Rust SDK has the primitive:
// Storage::open_object + ObjectDescriptor::read_range).
func (c *rustStorageClient) NewMultiRangeDownloader(context.Context, *newMultiRangeDownloaderParams, ...storageOption) (*MultiRangeDownloader, error) {
	return nil, status.Error(codes.Unimplemented, "storage: MultiRangeDownloader is not yet supported with the rust core")
}

// ---- Listing ---------------------------------------------------------------------------------

// rustListObjectsRequest is the core's "storage.objects.list" request: the JSON API query
// parameters (gcs_core/src/json_api.rs). The response is the JSON API Objects resource.
type rustListObjectsRequest struct {
	Bucket                   string `json:"bucket"`
	Prefix                   string `json:"prefix,omitempty"`
	Delimiter                string `json:"delimiter,omitempty"`
	PageToken                string `json:"pageToken,omitempty"`
	MaxResults               int64  `json:"maxResults,omitempty"`
	Versions                 bool   `json:"versions,omitempty"`
	StartOffset              string `json:"startOffset,omitempty"`
	EndOffset                string `json:"endOffset,omitempty"`
	IncludeTrailingDelimiter bool   `json:"includeTrailingDelimiter,omitempty"`
	MatchGlob                string `json:"matchGlob,omitempty"`
	IncludeFoldersAsPrefixes bool   `json:"includeFoldersAsPrefixes,omitempty"`
	SoftDeleted              bool   `json:"softDeleted,omitempty"`
	Projection               string `json:"projection,omitempty"`
	Fields                   string `json:"fields,omitempty"`
	Filter                   string `json:"filter,omitempty"`
}

// rustListRequest maps a Query page to the core's request, as httpStorageClient.ListObjects
// maps it to the JSON API (default projection: full; field selection: nextPageToken + attrs).
func rustListRequest(bucket string, q *Query, pageSize int, pageToken string) rustListObjectsRequest {
	projection := q.Projection
	if projection == ProjectionDefault {
		projection = ProjectionFull
	}
	req := rustListObjectsRequest{
		Bucket:                   bucket,
		Prefix:                   q.Prefix,
		Delimiter:                q.Delimiter,
		PageToken:                pageToken,
		MaxResults:               int64(max(pageSize, 0)),
		Versions:                 q.Versions,
		StartOffset:              q.StartOffset,
		EndOffset:                q.EndOffset,
		IncludeTrailingDelimiter: q.IncludeTrailingDelimiter,
		MatchGlob:                q.MatchGlob,
		IncludeFoldersAsPrefixes: q.IncludeFoldersAsPrefixes,
		SoftDeleted:              q.SoftDeleted,
		Projection:               projection.String(),
		Filter:                   q.Filter,
	}
	if selection := q.toFieldSelection(); selection != "" {
		req.Fields = "nextPageToken," + selection
	}
	return req
}

// ListObjects lists over the JSON API (storage.objects.list, served by the core), or over gRPC
// (the v2 ListObjects RPC, through grpcStorageClient) for NewGRPCClient. Paging, retries and
// errors are the core's; Go maps the Query to the request and the JSON resources to
// ObjectAttrs (newObject, shared with the JSON transport).
func (c *rustStorageClient) ListObjects(ctx context.Context, bucket string, q *Query, opts ...storageOption) *ObjectIterator {
	if c.grpc {
		return c.grpcStorageClient.ListObjects(ctx, bucket, q, opts...)
	}
	s := callSettings(c.settings, opts...)
	it := &ObjectIterator{ctx: ctx}
	if q != nil {
		it.query = *q
	}
	fetch := func(pageSize int, pageToken string) (token string, err error) {
		ctx, _ := startSpan(it.ctx, "rustStorageClient.ObjectsListCall", withStorageURI(bucket, ""))
		defer func() { endSpan(ctx, err) }()
		req := rustListRequest(bucket, &it.query, pageSize, pageToken)
		var resp raw.Objects
		err = run(ctx, func(ctx context.Context) error {
			out, err := rustJSONCall(ctx, c.core, "storage.objects.list", req, rustOptions{UserProject: s.userProject})
			if err != nil {
				return err
			}
			return json.Unmarshal(out, &resp)
		}, s.retry, s.idempotent, withOperation("ListObjects"), withObject(it.query.Prefix), withBucket(bucket))
		if err != nil {
			return "", formatBucketError(err)
		}
		for _, item := range resp.Items {
			it.items = append(it.items, newObject(item))
		}
		for _, prefix := range resp.Prefixes {
			it.items = append(it.items, &ObjectAttrs{Prefix: prefix})
		}
		return resp.NextPageToken, nil
	}
	it.pageInfo, it.nextFunc = iterator.NewPageInfo(
		fetch,
		func() int { return len(it.items) },
		func() interface{} { b := it.items; it.items = nil; return b })
	return it
}

// ---- Reads -----------------------------------------------------------------------------------

type rustReadMetadata struct {
	Metadata     json.RawMessage `json:"metadata"`
	ReadOffset   int64           `json:"readOffset"`
	Remaining    int64           `json:"remaining"`
	Decompressed bool            `json:"decompressed"`
}

func (c *rustStorageClient) NewRangeReader(ctx context.Context, params *newRangeReaderParams, opts ...storageOption) (*Reader, error) {
	s := callSettings(c.settings, opts...)
	// Go API -> v2 request (NewRangeReader semantics: length < 0 = to the end, offset < 0 = tail).
	req := &storagepb.ReadObjectRequest{
		Bucket:                    bucketResourceName(globalProjectAlias, params.bucket),
		Object:                    params.object,
		CommonObjectRequestParams: toProtoCommonObjectRequestParams(params.encryptionKey),
	}
	if err := applyCondsProto("rustStorageClient.NewRangeReader", params.gen, params.conds, req); err != nil {
		return nil, err
	}
	o := rustOptions{UserProject: s.userProject, Decompress: !params.readCompressed, GRPC: c.grpc}
	switch {
	case params.length == 0:
		// Object metadata only. Over gRPC the core still opens a read stream at the offset (and
		// closes it after the first response), as the Go SDK's gRPC readers do.
		req.ReadOffset = params.offset
		o.MetadataOnly = true
	case params.length > 0:
		req.ReadOffset, req.ReadLimit = params.offset, params.length
	default:
		req.ReadOffset = params.offset
	}
	if params.disableCRCCheck {
		o.VerifyCRC32C = proto.Bool(false)
	}
	var release func()
	o.Retry, release = toRustRetry(s.retry, &RetryContext{InvocationID: uuid.New().String(), Operation: "NewRangeReader", Bucket: params.bucket, Object: params.object})
	in, err := protojson.Marshal(req)
	if err != nil {
		release()
		return nil, err
	}
	optsJSON, _, err := rustCallOptions(ctx, o)
	if err != nil {
		release()
		return nil, err
	}
	r, err := c.core.OpenReader(ctx, in, optsJSON)
	if err != nil {
		release()
		return nil, formatObjectErr(rustToGoError(err, ctx))
	}
	var meta rustReadMetadata
	var obj storagepb.Object
	if err := json.Unmarshal(r.Metadata, &meta); err == nil {
		err = (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(meta.Metadata, &obj)
	}
	if err != nil {
		r.Close()
		release()
		return nil, fmt.Errorf("storage: decoding read metadata from the rust core: %w", err)
	}
	attrs := ReaderObjectAttrs{
		Size:            obj.GetSize(),
		StartOffset:     meta.ReadOffset,
		ContentType:     obj.GetContentType(),
		ContentEncoding: obj.GetContentEncoding(),
		CacheControl:    obj.GetCacheControl(),
		Generation:      obj.GetGeneration(),
		Metageneration:  obj.GetMetageneration(),
		CRC32C:          obj.GetChecksums().GetCrc32C(),
		Decompressed:    meta.Decompressed,
	}
	if obj.GetUpdateTime() != nil {
		attrs.LastModified = obj.GetUpdateTime().AsTime()
	}
	md := obj.GetMetadata()
	if md == nil && !c.grpc {
		md = map[string]string{} // as the native transports: JSON always returns a map, gRPC nil when empty
	}
	return &Reader{
		Attrs:          attrs,
		objectMetadata: &md,
		size:           obj.GetSize(),
		remain:         meta.Remaining,
		// CRC32C of full reads is validated inside the core (no second pass in Go).
		reader: &rustReader{r: r, ctx: ctx, release: release},
		bucket: params.bucket,
		object: params.object,
	}, nil
}

type rustReader struct {
	r       *rustcore.Reader
	ctx     context.Context
	release func()
	once    sync.Once
}

func (rr *rustReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(rr.ctx, p)
	if err != nil && err != io.EOF {
		err = formatObjectErr(rustToGoError(err, rr.ctx))
	}
	return n, err
}

func (rr *rustReader) Close() error {
	rr.r.Close()
	rr.once.Do(rr.release)
	return nil
}

// rustCopyBufs backs rustReader.WriteTo.
var rustCopyBufs = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// WriteTo serves io.Copy (Reader.WriteTo) with 1 MiB reads: io.Copy's own 32 KiB buffer (8 KiB
// for io.Discard) would cost one FFI crossing per small chunk.
func (rr *rustReader) WriteTo(w io.Writer) (int64, error) {
	bp := rustCopyBufs.Get().(*[]byte)
	defer rustCopyBufs.Put(bp)
	buf := *bp
	var total int64
	for {
		n, err := rr.Read(buf)
		if n > 0 {
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
			if m != n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// ---- Writes ----------------------------------------------------------------------------------

func (c *rustStorageClient) OpenWriter(params *openWriterParams, opts ...storageOption) (internalWriter, error) {
	if params.attrs.Retention != nil {
		// Fail instead of silently writing an object without its retention configuration.
		return nil, status.Error(codes.Unimplemented, "storage: object retention on upload is not yet supported with the rust core")
	}
	s := callSettings(c.settings, opts...)
	chunkSize, idempotent := params.chunkSize, s.idempotent // ChunkSize 0 = unchunked, no retries
	o := rustOptions{UserProject: s.userProject, ChunkSize: &chunkSize, Idempotent: &idempotent}
	// Appendable objects (zonal buckets): BidiWriteObject through the SDK's appendable writer,
	// which sizes its own messages. ObjectHandle.Generation selects a takeover.
	takeover := params.append && params.appendGen >= 0
	if params.append {
		o.ChunkSize, o.FinalizeOnClose = nil, params.finalizeOnClose
		if takeover && params.setTakeoverOffset == nil {
			return nil, errors.New("storage: no way to report offset for appendable takeover")
		}
	}
	var release func()
	o.Retry, release = toRustRetry(s.retry, &RetryContext{InvocationID: uuid.New().String(), Operation: "OpenWriter", Bucket: params.bucket, Object: params.attrs.Name})
	iw := &rustInternalWriter{
		core:       c.core,
		opts:       o,
		release:    release,
		params:     params,
		appendable: params.append,
		// A takeover cannot set attributes: the object exists.
		needSniff: params.attrs.ContentType == "" && !params.forceEmptyContentType && !takeover,
	}
	if takeover {
		// Reopen right away: NewWriterFromAppendableObject waits for the takeover offset.
		w, err := iw.open()
		if err != nil {
			if iw.writer() != nil {
				iw.writer().Free()
			}
			release()
			return nil, err
		}
		go iw.awaitTakeover(w)
	}
	return iw, nil
}

// rustWriteRequest maps the Go Writer configuration to the core's upload request (the shape of
// google.storage.v2.StartResumableWriteRequest), reusing the gRPC transport's mapping.
func rustWriteRequest(params *openWriterParams, contentType string) ([]byte, error) {
	attrs := *params.attrs
	attrs.ContentType = contentType
	spec := &storagepb.WriteObjectSpec{Resource: attrs.toProtoObject(params.bucket), PredefinedAcl: attrs.PredefinedACL}
	if err := applyCondsProto("WriteObject", defaultGen, params.conds, spec); err != nil {
		return nil, err
	}
	req := &storagepb.StartResumableWriteRequest{
		WriteObjectSpec:           spec,
		CommonObjectRequestParams: toProtoCommonObjectRequestParams(params.encryptionKey),
	}
	if params.sendCRC32C || len(attrs.MD5) > 0 {
		req.ObjectChecksums = &storagepb.ObjectChecksums{Md5Hash: attrs.MD5}
		if params.sendCRC32C {
			req.ObjectChecksums.Crc32C = proto.Uint32(attrs.CRC32C)
		}
	}
	return protojson.Marshal(req)
}

// rustAppendRequest maps an appendable Writer to the first BidiWriteObjectRequest, as Go's gRPC
// writer sends it: a new appendable object (writeObjectSpec), or a takeover of an unfinalized
// one (appendObjectSpec) when ObjectHandle.Generation is set.
func rustAppendRequest(params *openWriterParams, contentType string) ([]byte, error) {
	attrs := *params.attrs
	attrs.ContentType = contentType
	spec := &storagepb.WriteObjectSpec{Resource: attrs.toProtoObject(params.bucket), PredefinedAcl: attrs.PredefinedACL, Appendable: proto.Bool(true)}
	if err := applyCondsProto("WriteObject", defaultGen, params.conds, spec); err != nil {
		return nil, err
	}
	req := &storagepb.BidiWriteObjectRequest{CommonObjectRequestParams: toProtoCommonObjectRequestParams(params.encryptionKey)}
	if params.appendGen >= 0 {
		req.FirstMessage = &storagepb.BidiWriteObjectRequest_AppendObjectSpec{AppendObjectSpec: writeObjectSpecAsAppendObjectSpec(spec, params.appendGen)}
	} else {
		req.FirstMessage = &storagepb.BidiWriteObjectRequest_WriteObjectSpec{WriteObjectSpec: spec}
	}
	return protojson.Marshal(req)
}

// rustInternalWriter adapts rustcore.Writer to storage.Writer's internalWriter contract:
// Write/Close drive the upload, CloseWithError (ctx cancellation) aborts it without
// finalizing, and donec is closed exactly once when the upload is over.
type rustInternalWriter struct {
	core    *rustcore.Client
	opts    rustOptions
	release func()
	params  *openWriterParams

	// Go API semantics: an empty ContentType is detected from the first 512 bytes.
	needSniff bool
	sniff     []byte
	written   int64

	// Appendable uploads (zonal buckets): Flush is supported, progress reports persisted
	// sizes, and the object is reported (setObj) once its generation is known.
	appendable   bool
	objReported  bool
	sendFinalCRC bool
	finalCRC32C  uint32

	mu       sync.Mutex
	w        *rustcore.Writer
	aborted  bool
	busy     bool // the owning goroutine is inside Write, Flush or Close
	doneOnce sync.Once
}

const sniffLen = 512

// begin marks the owner (Write/Close) as active; it fails once the upload was aborted.
// CloseWithError runs on another goroutine and must never free the upload under an active
// owner: when it finds the owner busy, the owner finishes the writer in end().
func (iw *rustInternalWriter) begin() error {
	iw.mu.Lock()
	defer iw.mu.Unlock()
	if iw.aborted {
		if err := iw.params.ctx.Err(); err != nil {
			return err
		}
		return errors.New("storage: writer was aborted")
	}
	iw.busy = true
	return nil
}

func (iw *rustInternalWriter) end() {
	iw.mu.Lock()
	iw.busy = false
	aborted := iw.aborted
	iw.mu.Unlock()
	if aborted {
		iw.finish()
	}
}

func (iw *rustInternalWriter) finish() {
	iw.doneOnce.Do(func() {
		iw.mu.Lock()
		if iw.w != nil {
			iw.w.Free()
		}
		iw.mu.Unlock()
		iw.release()
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
	err = rustToGoError(err, iw.params.ctx)
	if !iw.isAborted() {
		iw.params.setError(err)
	}
	iw.finish()
	return err
}

// open starts the upload once the content type is known.
func (iw *rustInternalWriter) open() (*rustcore.Writer, error) {
	ct := iw.params.attrs.ContentType
	if iw.needSniff {
		ct = http.DetectContentType(iw.sniff)
	}
	build := rustWriteRequest
	if iw.appendable {
		build = rustAppendRequest
	}
	req, err := build(iw.params, ct)
	if err != nil {
		return nil, err
	}
	optsJSON, _, err := rustCallOptions(iw.params.ctx, iw.opts)
	if err != nil {
		return nil, err
	}
	iw.mu.Lock()
	if iw.aborted {
		iw.mu.Unlock()
		return nil, context.Canceled
	}
	w, err := iw.core.OpenWriter(req, optsJSON)
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
	if err := iw.begin(); err != nil {
		return 0, err
	}
	defer iw.end()
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
	if !iw.appendable {
		// Progress reports bytes accepted by the core (the JSON path reports bytes sent).
		// Appendable uploads report persisted sizes instead (Flush, Close), as Go's gRPC writer.
		iw.written += int64(len(p))
		iw.params.progress(iw.written)
	}
	return len(p), nil
}

func (iw *rustInternalWriter) Close() error {
	if err := iw.begin(); err != nil {
		iw.finish() // aborted earlier; the abort reason is already recorded
		return nil
	}
	defer iw.end()
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
		iw.fail(err) // surfaces through Writer.Close (w.err), as with the other transports
		return nil
	}
	var po storagepb.Object
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(js, &po); err != nil {
		iw.fail(err)
		return nil
	}
	if iw.appendable {
		// The core finalizes with the SDK's running CRC32C of every byte written; a caller's
		// AppendFinalCRC32C is checked against the result (Go's gRPC writer has the service
		// check it before finalizing).
		if iw.sendFinalCRC && po.GetFinalizeTime() != nil && po.GetChecksums().GetCrc32C() != iw.finalCRC32C {
			iw.fail(status.Errorf(codes.DataLoss, "storage: the finalized object's CRC32C is %d, AppendFinalCRC32C is %d", po.GetChecksums().GetCrc32C(), iw.finalCRC32C))
			return nil
		}
		iw.params.progress(po.GetSize())
	}
	iw.params.setObj(newObjectFromProto(&po))
	iw.finish()
	return nil
}

// CloseWithError aborts the upload (never finalizing it). Called by storage.Writer when its
// context is done; must not block. The upload's resources are released here only if no
// Write/Close is in flight; otherwise that call releases them when it returns.
func (iw *rustInternalWriter) CloseWithError(err error) error {
	iw.mu.Lock()
	iw.aborted = true
	w := iw.w
	idle := !iw.busy
	iw.mu.Unlock()
	if w != nil {
		w.Abort() // a pending Write/Close returns promptly with an error
	}
	iw.params.setError(err)
	if idle {
		iw.finish()
	}
	return nil
}

// Flush (appendable uploads) persists everything written so far and returns the persisted
// size. Before the first write it creates the object with no data, as Go's gRPC writer does.
func (iw *rustInternalWriter) Flush() (int64, error) {
	if !iw.appendable {
		return 0, errors.New("storage: Flush not supported unless client uses gRPC and Append is set to true")
	}
	if err := iw.begin(); err != nil {
		return 0, err
	}
	defer iw.end()
	w := iw.writer()
	if w == nil {
		var err error
		if w, err = iw.open(); err != nil { // with the content type sniffed so far
			return 0, iw.fail(err)
		}
	}
	n, err := w.Flush(iw.params.ctx)
	if err != nil {
		return 0, iw.fail(err)
	}
	iw.persisted(w, n)
	return n, nil
}

// persisted records a size the service confirmed for an appendable object. Go's gRPC writer
// reports the object from its stream's first response; the core reports the generation.
func (iw *rustInternalWriter) persisted(w *rustcore.Writer, n int64) {
	if !iw.objReported {
		iw.params.setObj(&ObjectAttrs{Bucket: iw.params.bucket, Name: iw.params.attrs.Name, Generation: w.Generation(), Size: n})
		iw.objReported = true
	}
	iw.params.setSize(n)
	iw.params.progress(n)
}

// awaitTakeover reports the takeover offset (the persisted size) once the core has reopened
// the unfinalized object, or ends the upload with the error that prevented it.
func (iw *rustInternalWriter) awaitTakeover(w *rustcore.Writer) {
	if err := iw.begin(); err != nil {
		iw.finish()
		return
	}
	// While this goroutine owns the writer, CloseWithError cannot end the upload (close donec),
	// so NewWriterFromAppendableObject is still waiting for the offset when it is sent.
	defer iw.end()
	n, err := w.OpenState(iw.params.ctx)
	if err != nil {
		iw.fail(err)
		return
	}
	iw.persisted(w, n)
	iw.params.setTakeoverOffset(n)
}

func (iw *rustInternalWriter) setAppendFinalCRC32C(send bool, c uint32) {
	iw.sendFinalCRC, iw.finalCRC32C = send, c
}

// ---- Introspection ---------------------------------------------------------------------------

func rustEngineStats() map[string]int64 {
	return map[string]int64{"ffi_calls": rustcore.FFICalls.Load(), "async_ops": rustcore.AsyncOps.Load()}
}
