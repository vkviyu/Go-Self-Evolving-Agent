package evolutiontools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const maxStrategyArgumentBytes = 256 << 10

func decodeEvolutionArgs(raw []byte, target any) error {
	if len(raw) > maxStrategyArgumentBytes {
		return errors.New("candidate request exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}
