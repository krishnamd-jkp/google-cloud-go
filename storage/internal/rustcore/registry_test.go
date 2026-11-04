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

// A callback for an ID the host already released must fail cleanly (cgo.Handle would panic and
// abort the process), panics must not escape, and release must be idempotent.
func TestRegistrySemantics(t *testing.T) {
	e := &Error{Code: CodeErr, RPCCode: 14, HTTPCode: 503}
	id, release := RegisterRetryPredicate(func(*Error, int) bool { return true })
	if !evalPredicate(id, e, 1) {
		t.Errorf("registered predicate not evaluated")
	}
	release()
	release() // idempotent
	unregister(id)
	if evalPredicate(id, e, 2) {
		t.Errorf("released predicate still evaluated")
	}
	pid, prelease := RegisterRetryPredicate(func(*Error, int) bool { panic("boom") })
	defer prelease()
	if evalPredicate(pid, e, 1) {
		t.Errorf("panicking predicate must mean 'do not retry'")
	}
	if a, b := register(1), register(2); a == b {
		t.Errorf("registry IDs reused")
	}
}
