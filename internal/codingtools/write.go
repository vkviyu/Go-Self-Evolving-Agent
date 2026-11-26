package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type writeArgs struct {
	Path           string `json:"path"`
	Content        string `json:"content"`
	Mode           int    `json:"mode"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

func (workspace *Workspace) write(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var args writeArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return nil, errors.New("path is required")
	}
	if args.Mode < 0 || args.Mode > 0o777 {
		return nil, errors.New("mode must be between 0 and 511")
	}
	if len(args.Content) > maxDiffInputBytes {
		return nil, errors.New("content exceeds the structured write limit")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	root, _, err := workspace.ensureWritable(ctx)
	if err != nil {
		return nil, err
	}
	_, relative, err := workspace.resolveParentAt(root, args.Path)
	if err != nil {
		return nil, err
	}
	return workspace.withWriteLease(ctx, relative, func() (json.RawMessage, error) {
		files, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer files.Close()
		if err := files.MkdirAll(filepath.Dir(relative), 0o755); err != nil {
			return nil, fmt.Errorf("create parent directory: %w", err)
		}
		mode := os.FileMode(0o644)
		kind := changeCreated
		var before []byte
		if info, statErr := files.Lstat(relative); statErr == nil {
			if !info.Mode().IsRegular() {
				return nil, errors.New("write target must be a regular file, not a link or device")
			}
			mode = info.Mode().Perm()
			kind = changeModified
			var truncated bool
			before, truncated, err = readRootBounded(ctx, files, relative, maxDiffInputBytes)
			if err != nil {
				return nil, fmt.Errorf("read existing workspace file %q: %w", relative, err)
			}
			if truncated {
				return nil, errors.New("existing file exceeds the structured write limit; use an approved bounded transformation")
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		} else if args.Mode != 0 {
			mode = os.FileMode(args.Mode)
		}
		if kind == changeCreated && strings.TrimSpace(args.ExpectedSHA256) != "" {
			return nil, fmt.Errorf("workspace file %q does not exist", relative)
		}
		if err := verifyExpectedBytes(relative, before, args.ExpectedSHA256); err != nil {
			return nil, err
		}
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if err := atomicWriteRoot(files, relative, []byte(args.Content), mode); err != nil {
			return nil, fmt.Errorf("write %q: %w", relative, err)
		}
		result := map[string]any{"path": relative, "bytes_written": len(args.Content)}
		appendFileChange(ctx, workspace.artifacts, result, relative, kind, before, []byte(args.Content))
		return json.Marshal(result)
	})
}

type editArgs struct {
	Path           string `json:"path"`
	OldText        string `json:"old_text"`
	NewText        string `json:"new_text"`
	ReplaceAll     bool   `json:"replace_all"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

func (workspace *Workspace) edit(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var args editArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if args.OldText == "" {
		return nil, errors.New("old_text must not be empty")
	}
	if strings.TrimSpace(args.Path) == "" {
		return nil, errors.New("path is required")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	root, _, err := workspace.ensureWritable(ctx)
	if err != nil {
		return nil, err
	}
	path, relative, err := workspace.resolveExistingAt(root, args.Path)
	if err != nil {
		return nil, err
	}
	return workspace.withWriteLease(ctx, relative, func() (json.RawMessage, error) {
		files, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer files.Close()
		resolved, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		data, truncated, err := readRootBounded(ctx, files, resolved, maxDiffInputBytes)
		if err != nil {
			return nil, err
		}
		if truncated {
			return nil, errors.New("file exceeds the structured edit limit; use an approved bounded transformation")
		}
		if err := verifyExpectedBytes(relative, data, args.ExpectedSHA256); err != nil {
			return nil, err
		}
		count := strings.Count(string(data), args.OldText)
		if count == 0 {
			return nil, errors.New("old_text was not found")
		}
		if count > 1 && !args.ReplaceAll {
			return nil, fmt.Errorf("old_text occurs %d times; set replace_all to replace every occurrence", count)
		}
		replacements := 1
		if args.ReplaceAll {
			replacements = count
		}
		// Check growth before strings.Replace allocates its result. A small
		// source and repeated large replacement must not trigger unbounded RAM.
		retained := len(data) - replacements*len(args.OldText)
		if len(args.NewText) > (maxDiffInputBytes-retained)/replacements {
			return nil, errors.New("edited content exceeds the structured edit limit")
		}
		content := string(data)
		if args.ReplaceAll {
			content = strings.ReplaceAll(content, args.OldText, args.NewText)
		} else {
			content = strings.Replace(content, args.OldText, args.NewText, 1)
		}
		if len(content) > maxDiffInputBytes {
			return nil, errors.New("edited content exceeds the structured edit limit")
		}
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		info, err := files.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if err := atomicWriteRoot(files, resolved, []byte(content), info.Mode().Perm()); err != nil {
			return nil, fmt.Errorf("edit %q: %w", relative, err)
		}
		result := map[string]any{"path": relative, "replacements": replacements, "bytes_written": len(content)}
		appendFileChange(ctx, workspace.artifacts, result, relative, changeModified, data, []byte(content))
		return json.Marshal(result)
	})
}

func verifyExpectedBytes(relative string, data []byte, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return nil
	}
	if len(expected) != sha256.Size*2 {
		return errors.New("expected_sha256 must be a 64-character hexadecimal digest")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return errors.New("expected_sha256 must be a hexadecimal digest")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expected {
		return fmt.Errorf("workspace file %q changed since the expected fingerprint was captured", relative)
	}
	return nil
}
