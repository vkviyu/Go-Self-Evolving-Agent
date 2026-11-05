// Package pluginabi is the guest-side SDK for local-agent WASM tools.
//
// The ABI passes opaque blob handles across the trust boundary. Go pointers
// never leave guest memory and host resources are always re-authorized by the
// host before use. The host bridge in bridge_wasip1.go only exists in a wasip1
// guest build.
package pluginabi

import "encoding/json"

// Version is the only ABI version supported by this SDK revision.
const Version uint32 = 1

// Handler processes one JSON request and returns one JSON result.
type Handler func(request json.RawMessage) (json.RawMessage, error)

var registered Handler

// Register installs the process-wide plugin handler during guest init.
func Register(handler Handler) {
	if handler == nil {
		panic("pluginabi: nil handler")
	}
	if registered != nil {
		panic("pluginabi: handler already registered")
	}
	registered = handler
}
