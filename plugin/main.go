// Command plugin is the CLIProxyAPI native plugin entry point. Build it with
// go build -buildmode=c-shared; the host loads the resulting .so, .dylib or
// .dll through the C ABI declared below (ABI version 1).
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int has_host_api(void) {
	return stored_host != NULL && stored_host->call != NULL;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (!has_host_api()) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/OlivierDijkstra/cliproxyapi-quota-router/internal/router"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

var (
	initOnce sync.Once
	handler  *router.Handler

	// hostGate protects the host API table, which the host frees as soon as
	// cliproxyPluginShutdown returns. Every host callback holds the read lock
	// for its whole duration. Shutdown takes the write lock, which waits for
	// in-flight callbacks to return and keeps new ones out, then clears the
	// pointer. The wait has no timeout: returning early would let a callback
	// read the table after the host freed it.
	hostGate   sync.RWMutex
	hostClosed bool
)

func main() {}

func setup(withHost bool) {
	initOnce.Do(func() {
		opts := []router.Option{}
		if withHost {
			h := router.HostClient{Call: callHost}
			opts = append(opts, router.WithAuthLister(h), router.WithLogger(h.Log))
		}
		r := router.New(opts...)
		handler = &router.Handler{Router: r, OnConfigured: func() {
			if withHost {
				r.StartRefresher()
			}
		}}
	})
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	hostGate.Lock()
	C.store_host_api(host)
	withHost := C.has_host_api() != 0
	hostClosed = !withHost
	hostGate.Unlock()
	setup(withHost)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	setup(false)
	if method == nil {
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, handler.Handle(C.GoString(method), req))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// Close the gate first. A refresh blocked in host.auth.list returns once the
	// host answers; host callbacks attempted afterwards fail without touching
	// the table, so stopping the refresher below cannot wait on the gate.
	hostGate.Lock()
	hostClosed = true
	C.store_host_api(nil)
	hostGate.Unlock()
	if handler != nil {
		handler.Router.StopRefresher()
		// The host does not send plugin.shutdown as an RPC before unloading
		// (v8.0.13 calls only this entry point), so the last flush of session
		// bindings happens here. It needs no host callback, only the filesystem.
		_ = handler.Router.SaveState()
	}
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHost invokes one host callback and returns the raw response envelope.
// It holds hostGate's read lock until the host's response buffer is freed.
func callHost(method string, payload []byte) ([]byte, int) {
	hostGate.RLock()
	defer hostGate.RUnlock()
	if hostClosed {
		return nil, 1
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(payload) > 0 {
		p := C.CBytes(payload)
		defer C.free(p)
		req = (*C.uint8_t)(p)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, req, C.size_t(len(payload)), &response)
	var raw []byte
	if response.ptr != nil {
		if response.len > 0 {
			raw = C.GoBytes(response.ptr, C.int(response.len))
		}
		C.free_host_buffer(response.ptr, response.len)
	}
	return raw, int(code)
}
