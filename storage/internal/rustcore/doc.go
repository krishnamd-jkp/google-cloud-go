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

// Package rustcore contains the cgo bindings to the shared Rust core (gcs_core v3) that serves
// every operation of cloud.google.com/go/storage when experimental.WithRustCore is used.
//
// The bindings are generic: they move proto3-JSON requests/responses, options JSON and object
// bytes across the C ABI (gcs_core.h) and contain no Cloud Storage logic, so they do not change
// when the storage API grows.
//
// The bindings are only compiled with `-tags gcsrust` and cgo enabled, on platforms for which
// a prebuilt core is shipped. Everywhere else this package is empty and the storage package
// uses its pure-Go transports, so `CGO_ENABLED=0`, cross-compiled and static builds keep
// working unchanged.
//
// EXPERIMENTAL proof of concept; not a supported API.
package rustcore
