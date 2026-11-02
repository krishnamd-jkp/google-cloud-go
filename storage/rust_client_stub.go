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
	"errors"
	"net/http"

	"cloud.google.com/go/auth"
)

// The Rust core is not compiled into this binary (no `-tags gcsrust`, CGO_ENABLED=0, or an
// unsupported GOOS/GOARCH).
const rustCoreCompiled = false

var errRustCoreNotCompiled = errors.New("storage: experimental.WithRustCore requires a binary built with -tags gcsrust and cgo on linux/amd64")

func newRustCoreClient(context.Context, string, *auth.Credentials, http.Header, ...storageOption) (storageClient, error) {
	return nil, errRustCoreNotCompiled
}

func rustEngineStats() map[string]int64 { return map[string]int64{} }
