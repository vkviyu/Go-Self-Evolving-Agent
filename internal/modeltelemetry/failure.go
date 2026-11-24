package modeltelemetry

import "errors"

// ErrToolCallFormat means the provider explicitly rejected its generated tool
// call before the model response completed.
var ErrToolCallFormat = errors.New("provider rejected the model's tool call format")
