package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type readArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	MaxBytes  int    `json:"max_bytes"`
}

func (workspace *Workspace) read(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var args readArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return nil, errors.New("path is required")
	}
	root, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	path, relative, err := workspace.resolveExistingAt(root, args.Path)
	if err != nil {
		return nil, err
	}
	limit, err := bounded(args.MaxBytes, defaultReadBytes, maximumReadBytes)
	if err != nil {
		return nil, err
	}
	files, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	resolved, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	data, incomplete, err := readRootBounded(ctx, files, resolved, maximumReadBytes)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", relative, err)
	}
	digest := ""
	if !incomplete {
		sum := sha256.Sum256(data)
		digest = hex.EncodeToString(sum[:])
	}
	truncated := incomplete || len(data) > limit
	data = data[:min(limit, len(data))]
	content := string(data)
	start, end := args.StartLine, args.EndLine
	if start < 0 || end < 0 || (end > 0 && start > 0 && end < start) {
		return nil, errors.New("line range is invalid")
	}
	if start > 0 || end > 0 {
		content, start, end = selectLines(content, start, end)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"path":                 relative,
		"content":              content,
		"sha256":               digest,
		"fingerprint_complete": !incomplete,
		"truncated":            truncated,
		"start_line":           start,
		"end_line":             end,
	})
}

func readLimited(path string, limit int) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	truncated := len(data) > limit
	if truncated {
		data = data[:limit]
	}
	return data, truncated, nil
}

func selectLines(content string, start, end int) (string, int, int) {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if start <= 0 {
		start = 1
	}
	if start > len(lines) {
		return "", start, start - 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], ""), start, end
}
