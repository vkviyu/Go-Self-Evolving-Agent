package tool

import (
	"context"
	"encoding/json"
	"errors"
)

// InvokeFunc adapts a function to Executor.
type InvokeFunc func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error)

// Invoke executes the adapted function.
func (f InvokeFunc) Invoke(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("nil tool invoke function")
	}
	return f(ctx, arguments)
}
