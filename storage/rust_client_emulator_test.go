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
	"net/http"
)

// addRustEmulatorClient registers the "rust" transport for emulator tests. The client wraps a
// fresh JSON client: object reads/writes go to the Rust engine, everything else (bucket setup,
// metadata, ACLs, ...) to the JSON client. No-op unless the engine is compiled in.
func addRustEmulatorClient(ctx context.Context, clients map[string]storageClient) error {
	if !rustDataPathCompiled {
		return nil
	}
	base, err := newHTTPStorageClient(ctx)
	if err != nil {
		return err
	}
	rc, err := newRustStorageClient(ctx, base, "", nil, nil)
	if err != nil {
		return err
	}
	clients["rust"] = rc
	return nil
}

// newRustRetryTestClient returns a Client whose object reads/writes go through the Rust engine
// with the testbench retry-test id attached to every request. The engine cannot forward
// per-call context headers (callctx.SetHeaders), so the id is set client-wide instead.
func newRustRetryTestClient(ctx context.Context, retryTestID string) (*Client, error) {
	c, err := NewClient(ctx)
	if err != nil {
		return nil, err
	}
	rc, err := newRustStorageClient(ctx, c.tc, "", nil, http.Header{"x-retry-test-id": {retryTestID}})
	if err != nil {
		return nil, err
	}
	c.tc = rc
	return c, nil
}
