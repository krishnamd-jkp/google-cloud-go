// Copyright 2024 Google LLC
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
	"testing"
	"time"

	"cloud.google.com/go/storage/internal/apiv2/storagepb"
	"github.com/google/go-cmp/cmp"
	raw "google.golang.org/api/storage/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestBucketEncryption_ToRaw(t *testing.T) {
	e := &BucketEncryption{
		DefaultKMSKeyName: "key",
		GoogleManagedEncryptionEnforcementConfig: &GoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
		},
		CustomerManagedEncryptionEnforcementConfig: &CustomerManagedEncryptionEnforcementConfig{
			RestrictionMode: "NotRestricted",
		},
		CustomerSuppliedEncryptionEnforcementConfig: &CustomerSuppliedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
		},
	}

	got := e.toRawBucketEncryption()
	want := &raw.BucketEncryption{
		DefaultKmsKeyName: "key",
		GoogleManagedEncryptionEnforcementConfig: &raw.BucketEncryptionGoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
		},
		CustomerManagedEncryptionEnforcementConfig: &raw.BucketEncryptionCustomerManagedEncryptionEnforcementConfig{
			RestrictionMode: "NotRestricted",
		},
		CustomerSuppliedEncryptionEnforcementConfig: &raw.BucketEncryptionCustomerSuppliedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
		},
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("toRawBucketEncryption mismatch (-got +want):\n%s", diff)
	}
}

func TestBucketEncryption_FromRaw(t *testing.T) {
	rawE := &raw.BucketEncryption{
		DefaultKmsKeyName: "key",
		GoogleManagedEncryptionEnforcementConfig: &raw.BucketEncryptionGoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
			EffectiveTime:   "2024-01-01T00:00:00Z",
		},
	}

	got := toBucketEncryption(rawE)
	want := &BucketEncryption{
		DefaultKMSKeyName: "key",
		GoogleManagedEncryptionEnforcementConfig: &GoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
			EffectiveTime:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("toBucketEncryption mismatch (-got +want):\n%s", diff)
	}
}

func TestBucketEncryption_ToProto(t *testing.T) {
	e := &BucketEncryption{
		DefaultKMSKeyName: "key",
		GoogleManagedEncryptionEnforcementConfig: &GoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
		},
	}

	got := e.toProtoBucketEncryption()
	// Helper to create string pointer
	s := func(s string) *string { return &s }
	want := &storagepb.Bucket_Encryption{
		DefaultKmsKey: "key",
		GoogleManagedEncryptionEnforcementConfig: &storagepb.Bucket_Encryption_GoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: s("FullyRestricted"),
		},
	}

	if diff := cmp.Diff(got, want, cmp.Comparer(proto.Equal)); diff != "" {
		t.Errorf("toProtoBucketEncryption mismatch (-got +want):\n%s", diff)
	}
}

func TestBucketEncryption_FromProto(t *testing.T) {
	s := func(s string) *string { return &s }
	protoE := &storagepb.Bucket_Encryption{
		DefaultKmsKey: "key",
		GoogleManagedEncryptionEnforcementConfig: &storagepb.Bucket_Encryption_GoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: s("FullyRestricted"),
			EffectiveTime:   timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		},
	}

	got := toBucketEncryptionFromProto(protoE)
	want := &BucketEncryption{
		DefaultKMSKeyName: "key",
		GoogleManagedEncryptionEnforcementConfig: &GoogleManagedEncryptionEnforcementConfig{
			RestrictionMode: "FullyRestricted",
			EffectiveTime:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	if diff := cmp.Diff(got, want); diff != "" {
		t.Errorf("toBucketEncryptionFromProto mismatch (-got +want):\n%s", diff)
	}
}
