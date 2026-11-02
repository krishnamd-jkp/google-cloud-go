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

//go:build !(gcsrust && cgo && linux && amd64)

package storage

import (
	"context"
	"net/http"

	"cloud.google.com/go/auth"
)

// The Rust engine is not compiled into this binary (no `-tags gcsrust`, CGO_ENABLED=0, or an
// unsupported GOOS/GOARCH). experimental.WithRustDataPath is then a no-op: the pure-Go
// transports are used, so the same code builds and runs everywhere.
const rustDataPathCompiled = false

func newRustStorageClient(_ context.Context, base storageClient, _ string, _ *auth.Credentials, _ http.Header) (storageClient, error) {
	return base, nil
}

func rustDataPathStats() (reads, writes, readFallbacks, writeFallbacks int64) { return 0, 0, 0, 0 }

func rustEngineStats() map[string]int64 { return map[string]int64{} }
