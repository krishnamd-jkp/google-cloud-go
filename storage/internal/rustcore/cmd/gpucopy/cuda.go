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

//go:build linux && cgo

package main

// The CUDA driver API, loaded with dlopen so that building needs no CUDA toolkit and the binary
// runs (with --device=sim) on machines without a GPU.

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stddef.h>
#include <stdint.h>

typedef int CUresult;
typedef int CUdevice;
typedef void *CUcontext;
typedef void *CUstream;
typedef unsigned long long CUdeviceptr;

static CUcontext g_ctx;
static CUresult (*p_cuInit)(unsigned int);
static CUresult (*p_cuDeviceGet)(CUdevice *, int);
static CUresult (*p_cuDeviceGetName)(char *, int, CUdevice);
static CUresult (*p_cuDevicePrimaryCtxSetFlags)(CUdevice, unsigned int);
static CUresult (*p_cuDevicePrimaryCtxRetain)(CUcontext *, CUdevice);
static CUresult (*p_cuCtxSetCurrent)(CUcontext);
static CUresult (*p_cuMemAlloc)(CUdeviceptr *, size_t);
static CUresult (*p_cuMemFree)(CUdeviceptr);
static CUresult (*p_cuMemHostAlloc)(void **, size_t, unsigned int);
static CUresult (*p_cuMemFreeHost)(void *);
static CUresult (*p_cuMemcpyHtoD)(CUdeviceptr, const void *, size_t);
static CUresult (*p_cuMemcpyHtoDAsync)(CUdeviceptr, const void *, size_t, CUstream);
static CUresult (*p_cuMemcpyDtoH)(void *, CUdeviceptr, size_t);
static CUresult (*p_cuStreamCreate)(CUstream *, unsigned int);
static CUresult (*p_cuStreamSynchronize)(CUstream);
static CUresult (*p_cuStreamDestroy)(CUstream);
static CUresult (*p_cuGetErrorString)(CUresult, const char **);

#define CU_CTX_SCHED_BLOCKING_SYNC 0x04
#define CU_STREAM_NON_BLOCKING 0x01

static void *sym(void *lib, const char *name, const char *alt) {
	void *p = dlsym(lib, name);
	if (!p && alt) p = dlsym(lib, alt);
	return p;
}

#define LOAD(var, name, alt) do { \
	*(void **)(&var) = sym(lib, name, alt); \
	if (!var) { *missing = name; return -2; } \
} while (0)

// cu_open loads libcuda.so.1, initializes the driver and makes the device's primary context
// current. It returns 0; -1 if libcuda can't be loaded (*missing = dlerror()); -2 if a symbol is
// missing (*missing = its name); or a CUDA error code.
static int cu_open(int ordinal, char *name, int name_len, const char **missing) {
	void *lib = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
	if (!lib) { *missing = dlerror(); return -1; }
	LOAD(p_cuInit, "cuInit", 0);
	LOAD(p_cuDeviceGet, "cuDeviceGet", 0);
	LOAD(p_cuDeviceGetName, "cuDeviceGetName", 0);
	LOAD(p_cuDevicePrimaryCtxSetFlags, "cuDevicePrimaryCtxSetFlags_v2", "cuDevicePrimaryCtxSetFlags");
	LOAD(p_cuDevicePrimaryCtxRetain, "cuDevicePrimaryCtxRetain", 0);
	LOAD(p_cuCtxSetCurrent, "cuCtxSetCurrent", 0);
	LOAD(p_cuMemAlloc, "cuMemAlloc_v2", 0);
	LOAD(p_cuMemFree, "cuMemFree_v2", 0);
	LOAD(p_cuMemHostAlloc, "cuMemHostAlloc", 0);
	LOAD(p_cuMemFreeHost, "cuMemFreeHost", 0);
	LOAD(p_cuMemcpyHtoD, "cuMemcpyHtoD_v2", 0);
	LOAD(p_cuMemcpyHtoDAsync, "cuMemcpyHtoDAsync_v2", 0);
	LOAD(p_cuMemcpyDtoH, "cuMemcpyDtoH_v2", 0);
	LOAD(p_cuStreamCreate, "cuStreamCreate", 0);
	LOAD(p_cuStreamSynchronize, "cuStreamSynchronize", 0);
	LOAD(p_cuStreamDestroy, "cuStreamDestroy_v2", "cuStreamDestroy");
	LOAD(p_cuGetErrorString, "cuGetErrorString", 0);
	CUdevice dev;
	CUresult r;
	if ((r = p_cuInit(0)) != 0) return r;
	if ((r = p_cuDeviceGet(&dev, ordinal)) != 0) return r;
	if ((r = p_cuDeviceGetName(name, name_len, dev)) != 0) return r;
	// Waiting threads sleep instead of spinning, so that the CPU time measured is the copies' own.
	if ((r = p_cuDevicePrimaryCtxSetFlags(dev, CU_CTX_SCHED_BLOCKING_SYNC)) != 0) return r;
	if ((r = p_cuDevicePrimaryCtxRetain(&g_ctx, dev)) != 0) return r;
	return p_cuCtxSetCurrent(g_ctx);
}

// Each call makes the context current first: a CUDA context is current per OS thread, and a
// goroutine's cgo calls can run on any thread.
static void *cu_host_alloc(size_t n, int *rc) {
	void *p = 0;
	CUresult r = p_cuCtxSetCurrent(g_ctx);
	if (!r) r = p_cuMemHostAlloc(&p, n, 0);
	*rc = r;
	return r ? 0 : p;
}
static int cu_host_free(void *p) { CUresult r = p_cuCtxSetCurrent(g_ctx); return r ? r : p_cuMemFreeHost(p); }
static CUdeviceptr cu_alloc(size_t n, int *rc) {
	CUdeviceptr d = 0;
	CUresult r = p_cuCtxSetCurrent(g_ctx);
	if (!r) r = p_cuMemAlloc(&d, n);
	*rc = r;
	return r ? 0 : d;
}
static int cu_free(CUdeviceptr d) { CUresult r = p_cuCtxSetCurrent(g_ctx); return r ? r : p_cuMemFree(d); }
static CUstream cu_stream_create(int *rc) {
	CUstream s = 0;
	CUresult r = p_cuCtxSetCurrent(g_ctx);
	if (!r) r = p_cuStreamCreate(&s, CU_STREAM_NON_BLOCKING);
	*rc = r;
	return r ? 0 : s;
}
static int cu_stream_destroy(CUstream s) { CUresult r = p_cuCtxSetCurrent(g_ctx); return r ? r : p_cuStreamDestroy(s); }
static int cu_htod(CUdeviceptr d, const void *src, size_t n) {
	CUresult r = p_cuCtxSetCurrent(g_ctx);
	return r ? r : p_cuMemcpyHtoD(d, src, n);
}
static int cu_htod_async(CUdeviceptr d, const void *src, size_t n, CUstream s) {
	CUresult r = p_cuCtxSetCurrent(g_ctx);
	return r ? r : p_cuMemcpyHtoDAsync(d, src, n, s);
}
static int cu_sync(CUstream s) { CUresult r = p_cuCtxSetCurrent(g_ctx); return r ? r : p_cuStreamSynchronize(s); }
static int cu_dtoh(void *dst, CUdeviceptr d, size_t n) {
	CUresult r = p_cuCtxSetCurrent(g_ctx);
	return r ? r : p_cuMemcpyDtoH(dst, d, n);
}
static const char *cu_error(int r) {
	const char *s = 0;
	if (p_cuGetErrorString) p_cuGetErrorString(r, &s);
	return s ? s : "unknown CUDA error";
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// cudaDevice is the first GPU, through the CUDA driver API.
type cudaDevice struct{ devName string }

func openCUDA(ordinal int) (device, error) {
	var name [256]C.char
	var missing *C.char
	switch rc := C.cu_open(C.int(ordinal), &name[0], C.int(len(name)), &missing); {
	case rc == -1:
		return nil, fmt.Errorf("loading libcuda.so.1 (is the NVIDIA driver installed?): %s", C.GoString(missing))
	case rc == -2:
		return nil, fmt.Errorf("libcuda.so.1 has no %s", C.GoString(missing))
	case rc != 0:
		return nil, cuErr("initializing CUDA", rc)
	}
	return &cudaDevice{devName: "cuda: " + C.GoString(&name[0])}, nil
}

func cuErr(what string, rc C.int) error {
	if rc == 0 {
		return nil
	}
	return fmt.Errorf("%s: CUDA error %d (%s)", what, int(rc), C.GoString(C.cu_error(rc)))
}

func (d *cudaDevice) name() string { return d.devName }

// allocPinned returns page-locked memory from cuMemHostAlloc, which the GPU DMAs from directly.
func (d *cudaDevice) allocPinned(n int) ([]byte, bool, func(), error) {
	var rc C.int
	p := C.cu_host_alloc(C.size_t(n), &rc)
	if rc != 0 {
		return nil, false, nil, cuErr("cuMemHostAlloc", rc)
	}
	return unsafe.Slice((*byte)(p), n), true, func() { C.cu_host_free(p) }, nil
}

func (d *cudaDevice) newQueue(slots, n int) (queue, error) {
	q := &cudaQueue{}
	for range slots {
		var rc C.int
		s := C.cu_stream_create(&rc)
		if rc != 0 {
			q.close()
			return nil, cuErr("cuStreamCreate", rc)
		}
		q.streams = append(q.streams, s)
		b := C.cu_alloc(C.size_t(n), &rc)
		if rc != 0 {
			q.close()
			return nil, cuErr("cuMemAlloc", rc)
		}
		q.bufs = append(q.bufs, b)
	}
	return q, nil
}

// cudaQueue is one worker's device buffers, each with its own non-blocking stream.
type cudaQueue struct {
	streams []C.CUstream
	bufs    []C.CUdeviceptr
}

// copyPageable is a blocking cuMemcpyHtoD: from pageable memory, the driver copies src into a
// pinned staging buffer on this thread, then DMAs it.
func (q *cudaQueue) copyPageable(slot int, src []byte) error {
	return cuErr("cuMemcpyHtoD", C.cu_htod(q.bufs[slot], unsafe.Pointer(&src[0]), C.size_t(len(src))))
}

// copyPinned queues a DMA from pinned memory on slot's stream and returns at once. src is memory
// from allocPinned (outside the Go heap), so the driver may keep using it after the call.
func (q *cudaQueue) copyPinned(slot int, src []byte) error {
	return cuErr("cuMemcpyHtoDAsync", C.cu_htod_async(q.bufs[slot], unsafe.Pointer(&src[0]), C.size_t(len(src)), q.streams[slot]))
}

func (q *cudaQueue) wait(slot int) error {
	return cuErr("cuStreamSynchronize", C.cu_sync(q.streams[slot]))
}

func (q *cudaQueue) readBack(slot int, dst []byte) ([]byte, error) {
	if err := q.wait(slot); err != nil {
		return nil, err
	}
	return dst, cuErr("cuMemcpyDtoH", C.cu_dtoh(unsafe.Pointer(&dst[0]), q.bufs[slot], C.size_t(len(dst))))
}

func (q *cudaQueue) close() {
	for _, s := range q.streams {
		C.cu_sync(s)
		C.cu_stream_destroy(s)
	}
	for _, b := range q.bufs {
		C.cu_free(b)
	}
}
