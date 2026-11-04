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

package storage

// Hermetic unit tests of the Rust-core integration: no bucket, emulator or network needed.
//
//	go test -short -tags gcsrust -run TestRustUnit .

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"cloud.google.com/go/storage/experimental"
	"cloud.google.com/go/storage/internal/apiv2/storagepb"
	"cloud.google.com/go/storage/internal/rustcore"
	gax "github.com/googleapis/gax-go/v2"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestRustUnitEndpoints(t *testing.T) {
	for _, tc := range []struct{ in, json, grpc string }{
		{"https://storage.googleapis.com/storage/v1/", "https://storage.googleapis.com", "https://storage.googleapis.com"},
		{"https://storage.us-central1.rep.googleapis.com/storage/v1/", "https://storage.us-central1.rep.googleapis.com", "https://storage.us-central1.rep.googleapis.com"},
		{"http://localhost:8080/proxy/storage/v1/", "http://localhost:8080/proxy", "http://localhost:8080"},
	} {
		j, g, err := rustEndpoints(tc.in)
		if err != nil || j != tc.json || g != tc.grpc {
			t.Errorf("rustEndpoints(%q) = %q, %q, %v; want %q, %q", tc.in, j, g, err, tc.json, tc.grpc)
		}
	}
	if _, _, err := rustEndpoints("storage.googleapis.com"); err == nil {
		t.Errorf("relative endpoint accepted")
	}
}

// FieldMasks travel as raw path arrays: "*" and map keys must survive verbatim.
func TestRustUnitEncodeMasks(t *testing.T) {
	check := func(req any, field string, want []string) {
		t.Helper()
		var b []byte
		var err error
		switch r := req.(type) {
		case *storagepb.GetObjectRequest:
			b, err = encodeRustRequest(r)
		case *storagepb.UpdateObjectRequest:
			b, err = encodeRustRequest(r)
		}
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := json.Unmarshal(m[field], &got); err != nil || len(got) != len(want) {
			t.Fatalf("%s = %s, want %v", field, m[field], want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s[%d] = %q, want %q", field, i, got[i], want[i])
			}
		}
	}
	check(&storagepb.GetObjectRequest{Bucket: "projects/_/buckets/b", Object: "o", ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}}}, "readMask", []string{"*"})
	paths := []string{"metadata.X-Custom-Key", "metadata.with_underscore", "content_type"}
	check(&storagepb.UpdateObjectRequest{Object: &storagepb.Object{Name: "o"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}, "updateMask", paths)
}

func TestRustUnitRetryTranslation(t *testing.T) {
	zero := 0
	o, release := toRustRetry(&retryConfig{
		policy:           RetryAlways,
		maxAttempts:      &zero,
		maxRetryDuration: 500 * time.Microsecond,
		backoff:          &gax.Backoff{Max: 5 * time.Second},
	}, nil)
	defer release()
	if o.Policy != "always" || o.MaxAttempts == nil || *o.MaxAttempts != 0 {
		t.Errorf("policy/attempts: %+v", o)
	}
	if o.MaxRetryDurationMs != 1 {
		t.Errorf("sub-millisecond deadline became %d ms, want 1", o.MaxRetryDurationMs)
	}
	if o.Backoff == nil || o.Backoff.MaxMs != 5000 || o.Backoff.InitialMs != 0 {
		t.Errorf("backoff with zero Initial must be kept (core applies the 1s default): %+v", o.Backoff)
	}
	if o, _ := toRustRetry(&retryConfig{}, nil); o.MaxAttempts != nil || o.Backoff != nil || o.MaxRetryDurationMs != 0 {
		t.Errorf("unset limits must stay unset: %+v", o)
	}
}

func TestRustUnitWithoutAuthentication(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	c, err := NewClient(context.Background(), option.WithoutAuthentication(), experimental.WithRustCore())
	if err != nil {
		t.Fatalf("NewClient(WithoutAuthentication, WithRustCore): %v", err)
	}
	defer c.Close()
	if _, ok := c.tc.(*rustStorageClient); !ok {
		t.Fatalf("rust core not active: %T", c.tc)
	}
}

// Closing clients releases the token sources the core holds (no per-client leak).
func TestRustUnitClientCloseReleasesTokenSource(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "unused", Expiry: time.Now().Add(time.Hour)})
	before := rustcore.RegisteredCallbacks()
	for i := 0; i < 20; i++ {
		c, err := NewClient(context.Background(), option.WithTokenSource(ts), experimental.WithRustCore())
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for rustcore.RegisteredCallbacks() > before && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
	if n := rustcore.RegisteredCallbacks(); n > before {
		t.Errorf("%d token sources still held after closing 20 clients", n-before)
	}
}

// NewClient + WithRustCore reads and lists over JSON; NewGRPCClient + WithRustCore over gRPC.
func TestRustUnitTransportSelection(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		newFn   func(context.Context, ...option.ClientOption) (*Client, error)
		grpc    bool
		rpcName string
	}{
		{"NewClient", NewClient, false, "http"},
		{"NewGRPCClient", NewGRPCClient, true, "grpc"},
	} {
		c, err := tc.newFn(ctx, option.WithoutAuthentication(), experimental.WithRustCore())
		if err != nil {
			t.Fatalf("%s(WithRustCore): %v", tc.name, err)
		}
		rc, ok := c.tc.(*rustStorageClient)
		if !ok {
			c.Close()
			t.Fatalf("%s: rust core not active: %T", tc.name, c.tc)
		}
		if rc.grpc != tc.grpc || c.rpcSystem.Value.AsString() != tc.rpcName {
			t.Errorf("%s: grpc=%v rpcSystem=%q, want grpc=%v rpcSystem=%q", tc.name, rc.grpc, c.rpcSystem.Value.AsString(), tc.grpc, tc.rpcName)
		}
		c.Close()
	}
}

// Query -> storage.objects.list request: the same parameters httpStorageClient sends.
func TestRustUnitListRequest(t *testing.T) {
	q := &Query{Prefix: "p/", Delimiter: "/", StartOffset: "a", EndOffset: "z", Versions: true, MatchGlob: "**.txt",
		IncludeTrailingDelimiter: true, IncludeFoldersAsPrefixes: true, SoftDeleted: true, Filter: "contexts.\"k\"=\"v\""}
	got := rustListRequest("b", q, 500, "tok")
	want := rustListObjectsRequest{Bucket: "b", Prefix: "p/", Delimiter: "/", PageToken: "tok", MaxResults: 500, Versions: true,
		StartOffset: "a", EndOffset: "z", IncludeTrailingDelimiter: true, MatchGlob: "**.txt", IncludeFoldersAsPrefixes: true,
		SoftDeleted: true, Projection: "full", Filter: "contexts.\"k\"=\"v\""}
	if got != want {
		t.Errorf("rustListRequest:\n got  %+v\n want %+v", got, want)
	}

	q = &Query{Projection: ProjectionNoACL}
	if err := q.SetAttrSelection([]string{"Name", "Size"}); err != nil {
		t.Fatal(err)
	}
	got = rustListRequest("b", q, 0, "")
	// toFieldSelection's field order is unspecified (map iteration).
	fieldsOK := strings.HasPrefix(got.Fields, "nextPageToken,prefixes,items(") && strings.HasSuffix(got.Fields, ")") &&
		strings.Contains(got.Fields, "name") && strings.Contains(got.Fields, "size")
	if got.Projection != "noAcl" || got.MaxResults != 0 || !fieldsOK {
		t.Errorf("projection/fields: got %+v", got)
	}
	b, err := json.Marshal(rustListRequest("b", &Query{}, -1, ""))
	if err != nil || string(b) != `{"bucket":"b","projection":"full"}` {
		t.Errorf("minimal request JSON = %s, %v", b, err)
	}
}

// coreThreadIDs returns the ids of the Rust core's threads (named "gcs-core").
func coreThreadIDs(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatal(err)
	}
	var tids []int
	for _, e := range entries {
		comm, err := os.ReadFile("/proc/self/task/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "gcs-core" {
			continue
		}
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids
}

// A TLS or HTTP/1 write (writev) to a connection the peer reset raises SIGPIPE on the writing
// core thread. Rust binaries ignore SIGPIPE in main(); a library inside a Go process does not
// get that, and the Go runtime kills the process for a SIGPIPE on a non-Go thread. The core
// blocks SIGPIPE on its threads, so the write fails with EPIPE instead. Without that, this test
// binary dies with "signal: broken pipe".
func TestRustUnitSIGPIPEOnCoreThreads(t *testing.T) {
	t.Setenv("STORAGE_EMULATOR_HOST", "")
	c, err := NewClient(context.Background(), option.WithoutAuthentication(), experimental.WithRustCore())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tids := coreThreadIDs(t)
	if len(tids) == 0 {
		t.Fatal("no gcs-core threads found")
	}
	for _, tid := range tids {
		if err := syscall.Tgkill(os.Getpid(), tid, syscall.SIGPIPE); err != nil {
			t.Fatalf("tgkill(%d): %v", tid, err)
		}
	}
	time.Sleep(200 * time.Millisecond) // a delivered SIGPIPE terminates the process immediately
	t.Logf("sent SIGPIPE to %d core threads; process alive", len(tids))
}

// The core holds as many data-path gRPC connections as the Go gRPC client it stands in for
// (the Rust SDK's own default is one per CPU).
func TestRustGRPCConnPoolSize(t *testing.T) {
	if got := rustGRPCConnPoolSize(initSettings()); got != defaultConnPoolSize {
		t.Errorf("default: %d connections, want the Go SDK's default %d", got, defaultConnPoolSize)
	}
	if got := rustGRPCConnPoolSize(initSettings(withClientOptions(option.WithGRPCConnectionPool(8)))); got != 8 {
		t.Errorf("WithGRPCConnectionPool(8): %d connections, want 8", got)
	}
}
