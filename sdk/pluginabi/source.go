//go:build !wasip1

package pluginabi

import "embed"

// SourceFiles ships the exact host SDK with the binary; candidate builds never
// fetch a mutable SDK from a network or depend on the checkout layout.
//
//go:embed abi.go bridge_wasip1.go
var SourceFiles embed.FS
