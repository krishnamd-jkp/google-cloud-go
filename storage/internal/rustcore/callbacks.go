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
	"runtime/cgo"
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
	// value per pending operation, so this never blocks the Rust runtime.
	ch := cgo.Handle(user).Value().(chan result)
	ch <- result{err: takeStatus(st), n: uint64(n)}
}

// TokenSource supplies OAuth2 access tokens to the engine (GCS_AUTH_TOKEN_CALLBACK). It lets
// the Rust data path use exactly the credentials the Go client was configured with.
type TokenSource interface {
	Token(ctx context.Context) (token string, expiry time.Time, err error)
}

const tokenTimeout = 60 * time.Second

//export rustcoreToken
func rustcoreToken(user C.uintptr_t, outToken **C.char, outExpiry *C.int64_t, outErr **C.char) C.int32_t {
	// Runs on a Tokio blocking-pool thread (never on an async worker).
	ts := cgo.Handle(user).Value().(TokenSource)
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	tok, exp, err := ts.Token(ctx)
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

// RegisterRetryPredicate makes p callable by the core. The returned handle goes into the
// options JSON (retry.predicate); call release once the operation using it has completed.
func RegisterRetryPredicate(p RetryPredicate) (handle uint64, release func()) {
	h := cgo.NewHandle(p)
	return uint64(h), h.Delete
}

//export rustcoreRetry
func rustcoreRetry(predicate C.uintptr_t, st C.GcsStatus, attempt C.uint32_t) C.int32_t {
	err := takeStatus(st)
	p := cgo.Handle(predicate).Value().(RetryPredicate)
	if err != nil && p(err, int(attempt)) {
		return 1
	}
	return 0
}
