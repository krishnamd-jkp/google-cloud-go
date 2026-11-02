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

// rustRPC implements storageRPC by handing every google.storage.v2 request to the shared Rust
// core (internal/rustcore): the request is encoded as proto3 JSON, sent with gcs_call together
// with the caller's policy (retry configuration, user project, per-call headers), and the
// response decoded back. It contains no storage logic: transport, auth, retries, backoff,
// idempotency and error classification all run in the core, identically for every language.
//
// Each method is one line; adding an RPC is one line here plus one line in the core.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	gapic "cloud.google.com/go/storage/internal/apiv2"
	"cloud.google.com/go/storage/internal/apiv2/storagepb"
	"cloud.google.com/go/storage/internal/rustcore"
	"github.com/googleapis/gax-go/v2"
	"github.com/googleapis/gax-go/v2/apierror"
	"github.com/googleapis/gax-go/v2/callctx"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type rustRPC struct{ core *rustcore.Client }

var _ storageRPC = (*rustRPC)(nil)

func (r *rustRPC) Close() error { r.core.Close(); return nil }

func (r *rustRPC) CreateBucket(ctx context.Context, req *storagepb.CreateBucketRequest, _ ...gax.CallOption) (*storagepb.Bucket, error) {
	return rustUnary(ctx, r.core, "CreateBucket", req, &storagepb.Bucket{})
}

func (r *rustRPC) DeleteBucket(ctx context.Context, req *storagepb.DeleteBucketRequest, _ ...gax.CallOption) error {
	_, err := rustUnary(ctx, r.core, "DeleteBucket", req, &emptypb.Empty{})
	return err
}

func (r *rustRPC) GetBucket(ctx context.Context, req *storagepb.GetBucketRequest, _ ...gax.CallOption) (*storagepb.Bucket, error) {
	return rustUnary(ctx, r.core, "GetBucket", req, &storagepb.Bucket{})
}

func (r *rustRPC) LockBucketRetentionPolicy(ctx context.Context, req *storagepb.LockBucketRetentionPolicyRequest, _ ...gax.CallOption) (*storagepb.Bucket, error) {
	return rustUnary(ctx, r.core, "LockBucketRetentionPolicy", req, &storagepb.Bucket{})
}

func (r *rustRPC) UpdateBucket(ctx context.Context, req *storagepb.UpdateBucketRequest, _ ...gax.CallOption) (*storagepb.Bucket, error) {
	return rustUnary(ctx, r.core, "UpdateBucket", req, &storagepb.Bucket{})
}

func (r *rustRPC) ComposeObject(ctx context.Context, req *storagepb.ComposeObjectRequest, _ ...gax.CallOption) (*storagepb.Object, error) {
	return rustUnary(ctx, r.core, "ComposeObject", req, &storagepb.Object{})
}

func (r *rustRPC) DeleteObject(ctx context.Context, req *storagepb.DeleteObjectRequest, _ ...gax.CallOption) error {
	_, err := rustUnary(ctx, r.core, "DeleteObject", req, &emptypb.Empty{})
	return err
}

func (r *rustRPC) GetObject(ctx context.Context, req *storagepb.GetObjectRequest, _ ...gax.CallOption) (*storagepb.Object, error) {
	return rustUnary(ctx, r.core, "GetObject", req, &storagepb.Object{})
}

func (r *rustRPC) MoveObject(ctx context.Context, req *storagepb.MoveObjectRequest, _ ...gax.CallOption) (*storagepb.Object, error) {
	return rustUnary(ctx, r.core, "MoveObject", req, &storagepb.Object{})
}

func (r *rustRPC) RestoreObject(ctx context.Context, req *storagepb.RestoreObjectRequest, _ ...gax.CallOption) (*storagepb.Object, error) {
	return rustUnary(ctx, r.core, "RestoreObject", req, &storagepb.Object{})
}

func (r *rustRPC) RewriteObject(ctx context.Context, req *storagepb.RewriteObjectRequest, _ ...gax.CallOption) (*storagepb.RewriteResponse, error) {
	return rustUnary(ctx, r.core, "RewriteObject", req, &storagepb.RewriteResponse{})
}

func (r *rustRPC) UpdateObject(ctx context.Context, req *storagepb.UpdateObjectRequest, _ ...gax.CallOption) (*storagepb.Object, error) {
	return rustUnary(ctx, r.core, "UpdateObject", req, &storagepb.Object{})
}

func (r *rustRPC) GetIamPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest, _ ...gax.CallOption) (*iampb.Policy, error) {
	return rustUnary(ctx, r.core, "GetIamPolicy", req, &iampb.Policy{})
}

func (r *rustRPC) SetIamPolicy(ctx context.Context, req *iampb.SetIamPolicyRequest, _ ...gax.CallOption) (*iampb.Policy, error) {
	return rustUnary(ctx, r.core, "SetIamPolicy", req, &iampb.Policy{})
}

func (r *rustRPC) TestIamPermissions(ctx context.Context, req *iampb.TestIamPermissionsRequest, _ ...gax.CallOption) (*iampb.TestIamPermissionsResponse, error) {
	return rustUnary(ctx, r.core, "TestIamPermissions", req, &iampb.TestIamPermissionsResponse{})
}

// Paginated RPCs: the core serves one page per call; the gapic iterator drives paging.

func (r *rustRPC) ListBuckets(ctx context.Context, req *storagepb.ListBucketsRequest, _ ...gax.CallOption) *gapic.BucketIterator {
	it := &gapic.BucketIterator{}
	it.InternalFetch = func(pageSize int, pageToken string) ([]*storagepb.Bucket, string, error) {
		page := proto.Clone(req).(*storagepb.ListBucketsRequest)
		page.PageSize, page.PageToken = int32(min(pageSize, math.MaxInt32)), pageToken
		resp, err := rustUnary(ctx, r.core, "ListBuckets", page, &storagepb.ListBucketsResponse{})
		if err != nil {
			return nil, "", err
		}
		it.Response = resp
		return resp.GetBuckets(), resp.GetNextPageToken(), nil
	}
	return it
}

func (r *rustRPC) ListObjects(ctx context.Context, req *storagepb.ListObjectsRequest, _ ...gax.CallOption) *gapic.ObjectIterator {
	it := &gapic.ObjectIterator{}
	it.InternalFetch = func(pageSize int, pageToken string) ([]*storagepb.Object, string, error) {
		page := proto.Clone(req).(*storagepb.ListObjectsRequest)
		page.PageSize, page.PageToken = int32(min(pageSize, math.MaxInt32)), pageToken
		resp, err := rustUnary(ctx, r.core, "ListObjects", page, &storagepb.ListObjectsResponse{})
		if err != nil {
			return nil, "", err
		}
		it.Response = resp
		return resp.GetObjects(), resp.GetNextPageToken(), nil
	}
	return it
}

// Streaming RPCs are never issued: rustStorageClient serves object reads and writes through
// the core's streaming API (gcs_read_* / gcs_write*) instead of the Go gRPC data path.

var errRustStream = status.Error(codes.Unimplemented, "storage: streaming RPCs are served by the rust core's data path")

func (r *rustRPC) ReadObject(context.Context, *storagepb.ReadObjectRequest, ...gax.CallOption) (storagepb.Storage_ReadObjectClient, error) {
	return nil, errRustStream
}

func (r *rustRPC) BidiReadObject(context.Context, ...gax.CallOption) (storagepb.Storage_BidiReadObjectClient, error) {
	return nil, errRustStream
}

func (r *rustRPC) BidiWriteObject(context.Context, ...gax.CallOption) (storagepb.Storage_BidiWriteObjectClient, error) {
	return nil, errRustStream
}

func (r *rustRPC) StartResumableWrite(context.Context, *storagepb.StartResumableWriteRequest, ...gax.CallOption) (*storagepb.StartResumableWriteResponse, error) {
	return nil, errRustStream
}

func (r *rustRPC) QueryWriteStatus(context.Context, *storagepb.QueryWriteStatusRequest, ...gax.CallOption) (*storagepb.QueryWriteStatusResponse, error) {
	return nil, errRustStream
}

// ---- generic plumbing ------------------------------------------------------------------------

// rustUnary is the whole per-call bridge: encode, call the core, decode, map the error.
func rustUnary[T proto.Message](ctx context.Context, core *rustcore.Client, method string, req proto.Message, resp T) (T, error) {
	var zero T
	in, err := encodeRustRequest(req)
	if err != nil {
		return zero, err
	}
	opts, release, err := rustCallOptions(ctx, rustOptions{})
	if err != nil {
		return zero, err
	}
	defer release()
	out, err := core.Call(ctx, method, in, opts)
	if err != nil {
		return zero, rustToGoError(err, ctx)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(out, resp); err != nil {
		return zero, fmt.Errorf("storage: decoding %s response from the rust core: %w", method, err)
	}
	return resp, nil
}

// encodeRustRequest marshals req as proto3 JSON, except that FieldMask fields (read_mask,
// update_mask) are sent as JSON arrays of raw paths: proto3 JSON camelCases mask paths, which
// rejects the "*" wildcard and cannot represent map keys such as metadata.<key> or
// labels.<key>. The core accepts both forms. Generic over every request message.
func encodeRustRequest(req proto.Message) ([]byte, error) {
	rm := req.ProtoReflect()
	var clone protoreflect.Message
	masks := map[string][]string{}
	fields := rm.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind || fd.Message().FullName() != "google.protobuf.FieldMask" || !rm.Has(fd) {
			continue
		}
		fm, ok := rm.Get(fd).Message().Interface().(*fieldmaskpb.FieldMask)
		if !ok {
			continue
		}
		if clone == nil {
			clone = proto.Clone(req).ProtoReflect()
		}
		masks[fd.JSONName()] = append([]string{}, fm.GetPaths()...)
		clone.Clear(fd)
	}
	if clone == nil {
		return protojson.Marshal(req)
	}
	b, err := protojson.Marshal(clone.Interface())
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	for name, paths := range masks {
		if obj[name], err = json.Marshal(paths); err != nil {
			return nil, err
		}
	}
	return json.Marshal(obj)
}

// rustOptions is the options JSON of the core ABI (see gcs_core/src/options.rs).
type rustOptions struct {
	Retry        *rustRetryOptions `json:"retry,omitempty"`
	UserProject  string            `json:"userProject,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Decompress   bool              `json:"decompress,omitempty"`
	VerifyCRC32C *bool             `json:"verifyCrc32c,omitempty"`
	MetadataOnly bool              `json:"metadataOnly,omitempty"`
	ChunkSize    int               `json:"chunkSize,omitempty"`
}

type rustRetryOptions struct {
	Policy             string              `json:"policy,omitempty"`
	MaxAttempts        int                 `json:"maxAttempts,omitempty"`
	MaxRetryDurationMs int64               `json:"maxRetryDurationMs,omitempty"`
	Backoff            *rustBackoffOptions `json:"backoff,omitempty"`
	Predicate          uint64              `json:"predicate,omitempty"`
}

type rustBackoffOptions struct {
	InitialMs  int64   `json:"initialMs"`
	MaxMs      int64   `json:"maxMs"`
	Multiplier float64 `json:"multiplier"`
}

// toRustRetry translates the caller's retry configuration (storage.RetryPolicy, WithMaxAttempts,
// WithMaxRetryDuration, WithBackoff, WithErrorFunc). It only *describes* the policy; the core
// executes it. release must be called once the operation has completed.
func toRustRetry(r *retryConfig, rctx *RetryContext) (*rustRetryOptions, func()) {
	release := func() {}
	if r == nil {
		return nil, release
	}
	o := &rustRetryOptions{}
	switch r.policy {
	case RetryAlways:
		o.Policy = "always"
	case RetryNever:
		o.Policy = "never"
	}
	if r.maxAttempts != nil {
		o.MaxAttempts = *r.maxAttempts
	}
	o.MaxRetryDurationMs = r.maxRetryDuration.Milliseconds()
	if b := r.backoff; b != nil && b.Initial > 0 {
		o.Backoff = &rustBackoffOptions{InitialMs: b.Initial.Milliseconds(), MaxMs: b.Max.Milliseconds(), Multiplier: b.Multiplier}
	}
	if r.shouldRetry != nil {
		h, rel := rustcore.RegisterRetryPredicate(func(e *rustcore.Error, attempt int) bool {
			var rc RetryContext
			if rctx != nil {
				rc = *rctx
			}
			rc.Attempt = attempt
			return r.shouldRetry(rustToGoError(e, context.Background()), &rc)
		})
		o.Predicate, release = h, rel
	}
	return o, release
}

// rustCallOptions builds the options JSON for one call from the context: the retry hand-off
// (set by run(), see invoke.go), the user project and per-call headers.
func rustCallOptions(ctx context.Context, o rustOptions) ([]byte, func(), error) {
	release := func() {}
	if ec := engineCallFrom(ctx); ec != nil && o.Retry == nil {
		o.Retry, release = toRustRetry(ec.retry, ec.rctx)
	}
	addHeader := func(k string, vs []string) {
		switch k = strings.ToLower(k); k {
		case "x-goog-user-project":
			o.UserProject = vs[0]
		case "x-goog-api-client", "x-goog-request-params", "x-goog-gcs-idempotency-token":
			// Owned by the core.
		default:
			if o.Headers == nil {
				o.Headers = map[string]string{}
			}
			o.Headers[k] = strings.Join(vs, ",")
		}
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for k, vs := range md {
			if len(vs) > 0 {
				addHeader(k, vs)
			}
		}
	}
	for k, vs := range callctx.HeadersFromContext(ctx) {
		if len(vs) > 0 {
			addHeader(k, vs)
		}
	}
	b, err := json.Marshal(o)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return b, release, nil
}

// rustCoreError is what callers see for a failure reported by the core. It satisfies both
// error conventions of this package regardless of the transport the core used:
// status.FromError / apierror (gRPC code) and errors.As(*googleapi.Error) (HTTP code).
type rustCoreError struct {
	ae   *apierror.APIError
	gerr *googleapi.Error
}

func (e *rustCoreError) Error() string              { return e.ae.Error() }
func (e *rustCoreError) GRPCStatus() *status.Status { return e.ae.GRPCStatus() }
func (e *rustCoreError) Unwrap() []error            { return []error{e.ae, e.gerr} }

// rustToGoError converts core failures into the error types this package returns.
func rustToGoError(err error, ctx context.Context) error {
	var e *rustcore.Error
	if !errors.As(err, &e) {
		if errors.Is(err, rustcore.ErrClosed) && ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if e.Code == rustcore.CodeCancelled {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return context.Canceled
	}
	if e.RPCCode < 0 && e.HTTPCode == 0 {
		return err // local failure (invalid argument, panic, ...): keep the core's message
	}
	code := codes.Unknown
	if e.RPCCode >= 0 {
		code = codes.Code(e.RPCCode)
	}
	httpCode := e.HTTPCode
	if httpCode == 0 {
		httpCode = http.StatusInternalServerError
	}
	ae, ok := apierror.FromError(status.Error(code, e.Message))
	if !ok {
		return err
	}
	return &rustCoreError{ae: ae, gerr: &googleapi.Error{Code: httpCode, Message: e.Message, Header: e.Header, Body: e.Message}}
}
