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

// addRustEmulatorClient registers the "rust" transport for emulator tests: a client served
// entirely by the Rust core (control plane over the testbench's gRPC port, data path over its
// JSON port). No-op unless the core is compiled in.
func addRustEmulatorClient(ctx context.Context, clients map[string]storageClient) error {
	if !rustCoreCompiled {
		return nil
	}
	rc, err := newRustCoreClient(ctx, "", nil, nil)
	if err != nil {
		return err
	}
	clients["rust"] = rc
	return nil
}

// newRustRetryTestClient returns a Client served by the Rust core for retry conformance tests.
// Metadata RPCs forward the per-call x-retry-test-id header (callctx); the core's data-path
// builders only accept client-level headers, so the id is also set client-wide.
func newRustRetryTestClient(ctx context.Context, retryTestID string) (*Client, error) {
	c, err := NewClient(ctx)
	if err != nil {
		return nil, err
	}
	rc, err := newRustCoreClient(ctx, "", nil, http.Header{"x-retry-test-id": {retryTestID}})
	if err != nil {
		return nil, err
	}
	c.tc.Close() // replaced: do not leak the JSON client
	c.tc = rc
	return c, nil
}
