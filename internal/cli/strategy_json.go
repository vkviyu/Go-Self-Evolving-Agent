package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const (
	maxStrategySpecBytes  = 16 << 10
	maxStrategySuiteBytes = 128 << 10
)

func readStrategyJSON(path string, limit int64, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("strategy input must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > limit {
		return errors.New("strategy input exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("strategy input must contain exactly one JSON value")
	}
	return nil
}
