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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const rustCoreCompiled = true

type rustStorageClient struct {
	*grpcStorageClient // Go API <-> v2 proto mapping; RPCs served by rustRPC
	core               *rustcore.Client
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

// newRustCoreClient returns a storageClient served entirely by the Rust core. endpoint and
// creds are the JSON endpoint and credentials NewClient resolved from the user's options.
func newRustCoreClient(ctx context.Context, endpoint string, creds *auth.Credentials, extraHeaders http.Header, opts ...storageOption) (storageClient, error) {
	s := initSettings(opts...)
	s.engineRetries = true
	config := newStorageConfig(s.clientOption...)

	cfg := rustcore.ClientConfig{UserAgent: userAgent + " gccl-rust-core", Headers: extraHeaders}
	if host := os.Getenv("STORAGE_EMULATOR_HOST"); host != "" {
		cfg.Endpoint = emulatorURL(host)
		cfg.GRPCEndpoint = cfg.Endpoint
		if g := os.Getenv("STORAGE_EMULATOR_HOST_GRPC"); g != "" {
			cfg.GRPCEndpoint = emulatorURL(g)
		}
		cfg.Auth = rustcore.AuthAnonymous
	} else {
		if creds == nil {
			return nil, errors.New("storage: the rust core needs credentials it can use; custom HTTP clients and transports are not supported")
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
	o := rustOptions{UserProject: s.userProject, Decompress: !params.readCompressed}
	switch {
	case params.length == 0:
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
	o.Retry, release = toRustRetry(s.retry, &RetryContext{Operation: "NewRangeReader", Bucket: params.bucket, Object: params.object})
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
	if md == nil {
		md = map[string]string{}
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

// ---- Writes ----------------------------------------------------------------------------------

func (c *rustStorageClient) OpenWriter(params *openWriterParams, opts ...storageOption) (internalWriter, error) {
	if params.append {
		return nil, status.Error(codes.Unimplemented, "storage: appendable uploads are not yet supported with the rust core")
	}
	s := callSettings(c.settings, opts...)
	o := rustOptions{UserProject: s.userProject, ChunkSize: params.chunkSize}
	var release func()
	o.Retry, release = toRustRetry(s.retry, &RetryContext{Operation: "OpenWriter", Bucket: params.bucket, Object: params.attrs.Name})
	return &rustInternalWriter{
		core:      c.core,
		opts:      o,
		release:   release,
		params:    params,
		needSniff: params.attrs.ContentType == "" && !params.forceEmptyContentType,
	}, nil
}

// rustWriteRequest maps the Go Writer configuration to the core's upload request (the shape of
// google.storage.v2.StartResumableWriteRequest), reusing the gRPC transport's mapping.
func rustWriteRequest(params *openWriterParams, contentType string) ([]byte, error) {
	attrs := *params.attrs
	attrs.ContentType = contentType
	spec := &storagepb.WriteObjectSpec{Resource: attrs.toProtoObject(params.bucket)}
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
	req, err := rustWriteRequest(iw.params, ct)
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
	// Progress reports bytes accepted by the core (the JSON path reports bytes sent).
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
		iw.fail(err) // surfaces through Writer.Close (w.err), as with the other transports
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
	return 0, errors.New("storage: Writer.Flush is not supported with the rust core")
}

func (iw *rustInternalWriter) setAppendFinalCRC32C(bool, uint32) {}

// ---- Introspection ---------------------------------------------------------------------------

func rustEngineStats() map[string]int64 {
	return map[string]int64{"ffi_calls": rustcore.FFICalls.Load(), "async_ops": rustcore.AsyncOps.Load()}
}
