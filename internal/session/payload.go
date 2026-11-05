package session

import (
	"encoding/json"
	"fmt"
)

// JSONPayload marshals a typed event payload and keeps JSON construction out of callers.
func JSONPayload(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal session payload: %w", err)
	}
	return data, nil
}

// MustJSONPayload is intended for statically shaped internal event payloads.
func MustJSONPayload(value any) json.RawMessage {
	data, err := JSONPayload(value)
	if err != nil {
		panic(err)
	}
	return data
}
