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

// Exported callbacks live in their own file: a cgo preamble in a file with //export may only
// contain declarations.

/*
#include <stdlib.h>
#include "gcs_core.h"
*/
import "C"

import (
	"context"
	"fmt"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"time"
)

// result is what an async completion delivers to the waiting goroutine.
type result struct {
	err *Error // nil on success
	n   uint64
}

//export rustcoreDone
func rustcoreDone(user C.uintptr_t, st C.GcsStatus, n C.uint64_t) {
	// Runs on a Rust runtime thread. The channel is buffered (cap 1) and receives exactly one
	// value per pending operation, so this never blocks the Rust runtime. Deliberately no
	// recover: the handle is owned by a goroutine that is blocked on it, so a failure here is
	// a binding bug, and swallowing it would hang that goroutine forever.
	ch := cgo.Handle(user).Value().(chan result)
	ch <- result{err: takeStatus(st), n: uint64(n)}
}

// Values the core may call back into (token sources, retry predicates) are kept in a registry
// under IDs that are never reused, instead of cgo.Handles: a call for an ID that was already
// released fails cleanly (cgo.Handle.Value panics), and release is idempotent. The core
// releases each ID through rustcoreRelease after its last use.
var (
	registry   sync.Map // uint64 -> any
	registryID atomic.Uint64
)

func register(v any) uint64 {
	id := registryID.Add(1)
	registry.Store(id, v)
	return id
}

func lookup(id uint64) (any, bool) { return registry.Load(id) }

func unregister(id uint64) { registry.Delete(id) }

// registered reports the number of live registry entries (for leak tests).
func registered() int {
	n := 0
	registry.Range(func(any, any) bool { n++; return true })
	return n
}

//export rustcoreRelease
func rustcoreRelease(id C.uintptr_t) { unregister(uint64(id)) }

// TokenSource supplies OAuth2 access tokens to the core (GCS_AUTH_TOKEN_CALLBACK). It lets the
// core use exactly the credentials the Go client was configured with.
type TokenSource interface {
	Token(ctx context.Context) (token string, expiry time.Time, err error)
}

const tokenTimeout = 60 * time.Second

//export rustcoreToken
func rustcoreToken(user C.uintptr_t, outToken **C.char, outExpiry *C.int64_t, outErr **C.char) (rc C.int32_t) {
	// Runs on a Tokio blocking-pool thread (never on an async worker). A panic must not unwind
	// into Rust: report it as a token error instead.
	defer func() {
		if r := recover(); r != nil {
			*outErr = C.CString(fmt.Sprintf("rustcore: token source panicked: %v", r))
			rc = 1
		}
	}()
	v, ok := lookup(uint64(user))
	if !ok {
		*outErr = C.CString("rustcore: token source already released")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	tok, exp, err := v.(TokenSource).Token(ctx)
	if err != nil {
		*outErr = C.CString(err.Error())
		return 1
	}
	*outToken = C.CString(tok) // freed by Rust with free(3)
	if !exp.IsZero() {
		*outExpiry = C.int64_t(exp.Unix())
	}
	return 0
}

// RetryPredicate classifies an error reported by the core: true = retry. Idempotency and the
// retry policy are still enforced by the core; the predicate only classifies errors. It runs
// on a core thread and must be fast.
type RetryPredicate func(err *Error, attempt int) bool

// RegisterRetryPredicate makes p callable by the core and returns the ID to put in the
// options JSON (retry.predicate). The core releases the ID after its last use; release is
// for the caller's own error paths (before the options reached the core) and is idempotent.
func RegisterRetryPredicate(p RetryPredicate) (id uint64, release func()) {
	id = register(p)
	return id, func() { unregister(id) }
}

// evalPredicate runs the predicate registered under id; unknown (released) IDs and panics
// mean "do not retry".
func evalPredicate(id uint64, err *Error, attempt int) (retry bool) {
	defer func() {
		if r := recover(); r != nil {
			retry = false
		}
	}()
	v, ok := lookup(id)
	if !ok || err == nil {
		return false
	}
	return v.(RetryPredicate)(err, attempt)
}

//export rustcoreRetry
func rustcoreRetry(predicate C.uintptr_t, st C.GcsStatus, attempt C.uint32_t) C.int32_t {
	if evalPredicate(uint64(predicate), takeStatus(st), int(attempt)) {
		return 1
	}
	return 0
}
