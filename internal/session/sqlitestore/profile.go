package sqlitestore

import "encoding/json"

func nonNilJSON(value json.RawMessage) json.RawMessage {
	if value == nil {
		return json.RawMessage{}
	}
	return value
}
