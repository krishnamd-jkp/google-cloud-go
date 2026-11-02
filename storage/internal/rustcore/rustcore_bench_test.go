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

import "testing"

// BenchmarkNoop: synchronous Go->Rust call (what EVALUATION.md reports as "the FFI hop").
func BenchmarkNoop(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Noop(uint64(i))
	}
}

// BenchmarkAsyncNoop: one asynchronous completion — the cost every async FFI operation pays
// (the POC paid it on every Read/Write call; v2 only when it actually has to wait).
func BenchmarkAsyncNoop(b *testing.B) {
	a := NewAsyncNoopper()
	defer a.Close()
	for i := 0; i < b.N; i++ {
		a.Do()
	}
}

func BenchmarkAsyncNoopParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		a := NewAsyncNoopper()
		defer a.Close()
		for pb.Next() {
			a.Do()
		}
	})
}
