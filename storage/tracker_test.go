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
	"testing"
)

func TestAddFeatureAttributes(t *testing.T) {
	ctx := context.Background()

	// Initial features should be 0.
	if got := getFeatureAttributes(ctx); got != 0 {
		t.Errorf("getFeatureAttributes(empty) = %d; want 0", got)
	}

	// Add a single feature.
	ctx = addFeatureAttributes(ctx, featureMultiStream)
	if got := getFeatureAttributes(ctx); got != uint8(featureMultiStream) {
		t.Errorf("getFeatureAttributes(MultiStream) = %d; want %d", got, featureMultiStream)
	}

	// Add another feature (merge).
	ctx = addFeatureAttributes(ctx, featurePCU)
	want := uint8(featureMultiStream) | uint8(featurePCU)
	if got := getFeatureAttributes(ctx); got != want {
		t.Errorf("getFeatureAttributes(MultiStream | PCU) = %d; want %d", got, want)
	}

	// Adding same feature should be idempotent.
	ctx = addFeatureAttributes(ctx, featurePCU)
	if got := getFeatureAttributes(ctx); got != want {
		t.Errorf("getFeatureAttributes(MultiStream | PCU | PCU) = %d; want %d", got, want)
	}
}

func TestEncodeDecodeUint8(t *testing.T) {
	tests := []uint8{0, 1, 2, 127, 128, 255}

	for _, want := range tests {
		encoded := encodeUint8(want)
		got, err := decodeUint8(encoded)
		if err != nil {
			t.Errorf("decodeUint8(%q) error: %v", encoded, err)
			continue
		}
		if got != want {
			t.Errorf("decodeUint8(encodeUint8(%d)) = %d; want %d", want, got, want)
		}
	}
}
