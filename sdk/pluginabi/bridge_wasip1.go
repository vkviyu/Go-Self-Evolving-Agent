//go:build wasip1

package pluginabi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unsafe"
)

type responseEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

//go:wasmimport local_agent_host_v1 host_blob_len
func hostBlobLen(handle uint64) int64

//go:wasmimport local_agent_host_v1 host_blob_read
//go:noescape
func hostBlobRead(handle uint64, destination *byte, capacity uint32) int32

//go:wasmimport local_agent_host_v1 host_blob_put
//go:noescape
func hostBlobPut(source *byte, length uint32) uint64

//go:wasmexport plugin_abi_version
func pluginABIVersion() uint32 {
	return Version
}

//go:wasmexport plugin_invoke
func pluginInvoke(requestHandle uint64) uint64 {
	request, err := getBlob(requestHandle)
	if err != nil {
		return errorBlob(fmt.Errorf("read request blob: %w", err))
	}
	if registered == nil {
		return errorBlob(errors.New("plugin handler is not registered"))
	}
	result, err := registered(json.RawMessage(request))
	if err != nil {
		return errorBlob(err)
	}
	if len(result) == 0 || !json.Valid(result) {
		return errorBlob(errors.New("plugin handler returned invalid JSON"))
	}
	payload, err := json.Marshal(responseEnvelope{OK: true, Result: result})
	if err != nil {
		return errorBlob(fmt.Errorf("encode plugin response: %w", err))
	}
	return putBlob(payload)
}

// errorBlob returns a failed response envelope as a host blob.
func errorBlob(cause error) uint64 {
	message := cause.Error()
	if len(message) > 4096 {
		message = strings.ToValidUTF8(message[:4096], "") + " [truncated]"
	}
	payload, _ := json.Marshal(responseEnvelope{OK: false, Error: message})
	return putBlob(payload)
}

// getBlob copies one host blob into guest memory. The request is host-owned:
// the guest must not call host_blob_drop on it. The host reclaims it when the
// call ends, including traps/cancellation. The returned response handle must
// remain live until the host copies it; there are no intermediate host blobs
// allocated by this SDK to drop earlier.
func getBlob(handle uint64) ([]byte, error) {
	length := hostBlobLen(handle)
	if length < 0 {
		return nil, errors.New("host rejected blob handle")
	}
	if uint64(length) > uint64(^uint32(0)) {
		return nil, errors.New("blob exceeds guest address space")
	}
	data := make([]byte, uint32(length))
	if len(data) > 0 && hostBlobRead(handle, unsafe.SliceData(data), uint32(len(data))) != 0 {
		return nil, errors.New("host failed to read blob")
	}
	return data, nil
}

// putBlob copies guest bytes into a new host blob and returns its handle.
func putBlob(data []byte) uint64 {
	if len(data) == 0 {
		return hostBlobPut(nil, 0)
	}
	return hostBlobPut(unsafe.SliceData(data), uint32(len(data)))
}
