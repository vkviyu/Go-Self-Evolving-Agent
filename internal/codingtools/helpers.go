package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func relativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." {
		return "."
	}
	return filepath.ToSlash(relative)
}

func normalizePattern(pattern string) (string, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || filepath.IsAbs(pattern) || strings.IndexByte(pattern, 0) >= 0 {
		return "", errors.New("glob pattern must stay within workspace")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(pattern)))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("glob pattern must stay within workspace")
	}
	return clean, nil
}

func decode(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func bounded(value, defaultValue, maximum int) (int, error) {
	if value == 0 {
		return defaultValue, nil
	}
	if value < 1 || value > maximum {
		return 0, fmt.Errorf("limit must be between 1 and %d", maximum)
	}
	return value, nil
}
