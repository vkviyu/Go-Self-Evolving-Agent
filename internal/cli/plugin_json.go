package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

func decodePluginJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("plugin input must contain exactly one JSON value")
	}
	return nil
}

func readBoundedPluginFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("plugin input must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("plugin input exceeds size limit")
	}
	return raw, nil
}

func readStrictPluginJSON(path string, limit int64, target any) error {
	raw, err := readBoundedPluginFile(path, limit)
	if err != nil {
		return err
	}
	return decodePluginJSON(raw, target)
}
