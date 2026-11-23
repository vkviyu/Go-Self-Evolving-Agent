package codingtools

import (
	"bufio"
	"bytes"
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
	"unicode/utf8"
)

type readArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	MaxBytes  int    `json:"max_bytes"`
	Offset    *int64 `json:"offset"`
}

type readResult struct {
	Path                string `json:"path"`
	Content             string `json:"content"`
	SHA256              string `json:"sha256"`
	FingerprintComplete bool   `json:"fingerprint_complete"`
	Truncated           bool   `json:"truncated"`
	StartLine           int    `json:"start_line"`
	EndLine             int    `json:"end_line"`
	NextStartLine       int    `json:"next_start_line,omitempty"`
	Offset              int64  `json:"offset"`
	NextOffset          int64  `json:"next_offset,omitempty"`
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
	if args.StartLine < 0 || args.EndLine < 0 || (args.EndLine > 0 && args.EndLine < args.StartLine) {
		return nil, errors.New("line range is invalid")
	}
	if args.Offset != nil && (*args.Offset < 0 || args.StartLine != 0 || args.EndLine != 0) {
		return nil, errors.New("offset must be non-negative and cannot be combined with a line range")
	}
	limit, err := bounded(args.MaxBytes, defaultReadBytes, maximumReadBytes)
	if err != nil {
		return nil, err
	}
	root, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	path, relative, err := workspace.resolveExistingAt(root, args.Path)
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
	file, info, err := openRootRegular(files, resolved)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", relative, err)
	}
	defer file.Close()
	result := readResult{Path: relative}
	var source io.ReadSeeker = file
	if info.Size() <= maximumReadBytes {
		// Use the same bytes for the full-file fingerprint and the returned page.
		data, err := io.ReadAll(io.LimitReader(file, maximumReadBytes+1))
		if err != nil {
			return nil, fmt.Errorf("fingerprint %q: %w", relative, err)
		}
		if len(data) <= maximumReadBytes {
			sum := sha256.Sum256(data)
			result.SHA256 = hex.EncodeToString(sum[:])
			result.FingerprintComplete = true
			source = bytes.NewReader(data)
		}
	}
	if args.Offset != nil {
		result.Offset = *args.Offset
	}
	if _, err := source.Seek(result.Offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek %q: %w", relative, err)
	}
	if err := result.readPage(ctx, source, args, limit); err != nil {
		return nil, fmt.Errorf("read %q: %w", relative, err)
	}
	return json.Marshal(result)
}

func (result *readResult) readPage(ctx context.Context, source io.Reader, args readArgs, limit int) error {
	if args.Offset == nil {
		result.StartLine = max(args.StartLine, 1)
		if result.StartLine > 1 {
			buffered := bufio.NewReaderSize(source, 32<<10)
			skipped, err := skipLines(ctx, buffered, result.StartLine-1)
			if err != nil {
				return err
			}
			result.Offset = skipped
			source = buffered
		}
	}
	data, err := io.ReadAll(io.LimitReader(source, int64(limit)+utf8.UTFMax))
	if err != nil {
		return err
	}
	if args.EndLine > 0 {
		data = limitLines(data, args.EndLine-result.StartLine+1)
	}
	result.Truncated = len(data) > limit
	if result.Truncated {
		cut := utf8PageEnd(data, limit)
		if cut == 0 {
			return errors.New("max_bytes is too small for the next UTF-8 character")
		}
		if args.Offset == nil {
			// Prefer complete lines. Oversized lines can be continued by byte offset.
			if newline := bytes.LastIndexByte(data[:cut], '\n'); newline >= 0 {
				cut = newline + 1
			}
		}
		data = data[:cut]
		result.NextOffset = result.Offset + int64(len(data))
	}
	if args.Offset == nil {
		result.EndLine = result.StartLine - 1 + bytes.Count(data, []byte{'\n'})
		if len(data) > 0 && data[len(data)-1] != '\n' {
			result.EndLine++
		} else if result.Truncated {
			result.NextStartLine = result.EndLine + 1
		}
	}
	result.Content = string(data)
	return contextError(ctx)
}

func skipLines(ctx context.Context, source *bufio.Reader, count int) (int64, error) {
	var skipped int64
	for count > 0 {
		if err := contextError(ctx); err != nil {
			return 0, err
		}
		fragment, err := source.ReadSlice('\n')
		skipped += int64(len(fragment))
		switch err {
		case nil:
			count--
		case io.EOF:
			return skipped, nil
		case bufio.ErrBufferFull:
		default:
			return 0, err
		}
	}
	return skipped, nil
}

func limitLines(data []byte, count int) []byte {
	position := 0
	for range count {
		newline := bytes.IndexByte(data[position:], '\n')
		if newline < 0 {
			return data
		}
		position += newline + 1
	}
	return data[:position]
}

// utf8PageEnd preserves a complete code point across a page boundary. data
// includes bytes beyond the requested end so a valid rune can be verified.
func utf8PageEnd(data []byte, end int) int {
	for start := end - 1; start >= max(end-utf8.UTFMax+1, 0); start-- {
		if utf8.RuneStart(data[start]) {
			_, size := utf8.DecodeRune(data[start:])
			if start+size > end {
				return start
			}
			break
		}
	}
	return end
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
