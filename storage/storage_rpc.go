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

package storage

import (
	"context"

	"cloud.google.com/go/iam/apiv1/iampb"
	gapic "cloud.google.com/go/storage/internal/apiv2"
	"cloud.google.com/go/storage/internal/apiv2/storagepb"
	"github.com/googleapis/gax-go/v2"
)

// storageRPC is the set of google.storage.v2 RPCs that grpcStorageClient issues. It is the
// seam between the Go API mapping (grpc_client.go: Go types <-> v2 protos) and the transport:
//
//   - *gapic.Client implements it over gRPC (the regular gRPC client), and
//   - rustRPC (rust_transport.go, EXPERIMENTAL) implements it by handing each request to the
//     shared Rust core, which owns transport, auth, retries and all other storage logic.
type storageRPC interface {
	Close() error

	CreateBucket(context.Context, *storagepb.CreateBucketRequest, ...gax.CallOption) (*storagepb.Bucket, error)
	DeleteBucket(context.Context, *storagepb.DeleteBucketRequest, ...gax.CallOption) error
	GetBucket(context.Context, *storagepb.GetBucketRequest, ...gax.CallOption) (*storagepb.Bucket, error)
	ListBuckets(context.Context, *storagepb.ListBucketsRequest, ...gax.CallOption) *gapic.BucketIterator
	LockBucketRetentionPolicy(context.Context, *storagepb.LockBucketRetentionPolicyRequest, ...gax.CallOption) (*storagepb.Bucket, error)
	UpdateBucket(context.Context, *storagepb.UpdateBucketRequest, ...gax.CallOption) (*storagepb.Bucket, error)

	ComposeObject(context.Context, *storagepb.ComposeObjectRequest, ...gax.CallOption) (*storagepb.Object, error)
	DeleteObject(context.Context, *storagepb.DeleteObjectRequest, ...gax.CallOption) error
	GetObject(context.Context, *storagepb.GetObjectRequest, ...gax.CallOption) (*storagepb.Object, error)
	ListObjects(context.Context, *storagepb.ListObjectsRequest, ...gax.CallOption) *gapic.ObjectIterator
	MoveObject(context.Context, *storagepb.MoveObjectRequest, ...gax.CallOption) (*storagepb.Object, error)
	RestoreObject(context.Context, *storagepb.RestoreObjectRequest, ...gax.CallOption) (*storagepb.Object, error)
	RewriteObject(context.Context, *storagepb.RewriteObjectRequest, ...gax.CallOption) (*storagepb.RewriteResponse, error)
	UpdateObject(context.Context, *storagepb.UpdateObjectRequest, ...gax.CallOption) (*storagepb.Object, error)

	GetIamPolicy(context.Context, *iampb.GetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	SetIamPolicy(context.Context, *iampb.SetIamPolicyRequest, ...gax.CallOption) (*iampb.Policy, error)
	TestIamPermissions(context.Context, *iampb.TestIamPermissionsRequest, ...gax.CallOption) (*iampb.TestIamPermissionsResponse, error)

	// Streaming data-path RPCs, used by the Go gRPC reader/writer implementations.
	ReadObject(context.Context, *storagepb.ReadObjectRequest, ...gax.CallOption) (storagepb.Storage_ReadObjectClient, error)
	BidiReadObject(context.Context, ...gax.CallOption) (storagepb.Storage_BidiReadObjectClient, error)
	BidiWriteObject(context.Context, ...gax.CallOption) (storagepb.Storage_BidiWriteObjectClient, error)
	StartResumableWrite(context.Context, *storagepb.StartResumableWriteRequest, ...gax.CallOption) (*storagepb.StartResumableWriteResponse, error)
	QueryWriteStatus(context.Context, *storagepb.QueryWriteStatusRequest, ...gax.CallOption) (*storagepb.QueryWriteStatusResponse, error)
}

var _ storageRPC = (*gapic.Client)(nil)

type engineCallKey struct{}

// engineCall carries the caller's retry configuration of a call served by the Rust core: run()
// makes a single attempt and the core executes the retry policy (see invoke.go). idempotent is
// the veneer's own classification, which the core uses to narrow (never widen) its per-RPC
// rule.
type engineCall struct {
	retry      *retryConfig
	rctx       *RetryContext
	idempotent bool
}

func withEngineCall(ctx context.Context, r *retryConfig, rctx *RetryContext, idempotent bool) context.Context {
	return context.WithValue(ctx, engineCallKey{}, &engineCall{retry: r, rctx: rctx, idempotent: idempotent})
}

func engineCallFrom(ctx context.Context) *engineCall {
	ec, _ := ctx.Value(engineCallKey{}).(*engineCall)
	return ec
}
