package codingtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type listArgs struct {
	Path       string `json:"path"`
	MaxEntries int    `json:"max_entries"`
}

func (workspace *Workspace) list(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var args listArgs
	if err := decode(raw, &args); err != nil {
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
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%q is not a directory", relative)
	}
	limit, err := bounded(args.MaxEntries, defaultListEntries, maximumListEntries)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, min(len(entries), limit))
	for index, entry := range entries {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		if index >= limit {
			break
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if entry.Type()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		items = append(items, map[string]any{"name": entry.Name(), "type": kind})
	}
	return json.Marshal(map[string]any{"path": relative, "entries": items, "truncated": len(entries) > limit})
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	MaxResults int    `json:"max_results"`
}

func (workspace *Workspace) glob(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var args globArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	pattern, err := normalizePattern(args.Pattern)
	if err != nil {
		return nil, err
	}
	limit, err := bounded(args.MaxResults, defaultResults, maximumResults)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, limit)
	truncated := false
	root, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(candidate string, entry os.DirEntry, walkErr error) error {
		if contextErr := contextError(ctx); contextErr != nil {
			return contextErr
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, relErr := filepath.Rel(root, candidate)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		matched, matchErr := matchesGlob(pattern, relative)
		if matchErr != nil {
			return matchErr
		}
		if !matched {
			return nil
		}
		if len(paths) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, err
	}
	sort.Strings(paths)
	return json.Marshal(map[string]any{"pattern": pattern, "paths": paths, "truncated": truncated})
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	MaxResults int    `json:"max_results"`
	MaxBytes   int    `json:"max_bytes"`
}

func (workspace *Workspace) grep(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var args grepArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	pattern := args.Pattern
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("pattern is required")
	}
	if args.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid grep pattern: %w; provide one complete RE2 expression, or search each symbol separately", err)
	}
	limit, err := bounded(args.MaxResults, defaultResults, maximumResults)
	if err != nil {
		return nil, err
	}
	maxBytes, err := bounded(args.MaxBytes, defaultSearchBytes, maximumSearchBytes)
	if err != nil {
		return nil, err
	}
	if args.Glob != "" {
		args.Glob, err = normalizePattern(args.Glob)
		if err != nil {
			return nil, err
		}
	}
	viewRoot, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	root := viewRoot
	if strings.TrimSpace(args.Path) != "" {
		root, _, err = workspace.resolveExistingAt(viewRoot, args.Path)
		if err != nil {
			return nil, err
		}
	}
	results := make([]map[string]any, 0, limit)
	usedBytes := 0
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if contextErr := contextError(ctx); contextErr != nil {
			return contextErr
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if len(results) >= limit || usedBytes >= maxBytes {
			truncated = true
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if args.Glob != "" {
			relative, _ := filepath.Rel(viewRoot, path)
			matched, matchErr := matchesGlob(args.Glob, filepath.ToSlash(relative))
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				return nil
			}
		}
		data, wasTruncated, readErr := readLimited(path, maxBytes-usedBytes)
		if readErr != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		relative, _ := filepath.Rel(viewRoot, path)
		for lineNumber, line := range strings.Split(string(data), "\n") {
			locations := re.FindAllStringIndex(line, -1)
			for _, location := range locations {
				if len(results) >= limit {
					truncated = true
					return filepath.SkipAll
				}
				match := map[string]any{"path": filepath.ToSlash(relative), "line": lineNumber + 1, "column": location[0] + 1, "text": line}
				usedBytes += len(line)
				results = append(results, match)
				if usedBytes >= maxBytes {
					truncated = true
					return filepath.SkipAll
				}
			}
		}
		if wasTruncated {
			truncated = true
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, err
	}
	return json.Marshal(map[string]any{"pattern": args.Pattern, "matches": results, "truncated": truncated})
}

func matchesGlob(pattern, candidate string) (bool, error) {
	if !strings.Contains(pattern, "/") {
		return pathpkg.Match(pattern, pathpkg.Base(candidate))
	}
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	candidateParts := strings.Split(strings.Trim(candidate, "/"), "/")
	var match func(int, int) (bool, error)
	match = func(patternIndex, candidateIndex int) (bool, error) {
		if patternIndex == len(patternParts) {
			return candidateIndex == len(candidateParts), nil
		}
		if patternParts[patternIndex] == "**" {
			for index := candidateIndex; index <= len(candidateParts); index++ {
				matched, err := match(patternIndex+1, index)
				if err != nil || matched {
					return matched, err
				}
			}
			return false, nil
		}
		if candidateIndex >= len(candidateParts) {
			return false, nil
		}
		matched, err := pathpkg.Match(patternParts[patternIndex], candidateParts[candidateIndex])
		if err != nil || !matched {
			return matched, err
		}
		return match(patternIndex+1, candidateIndex+1)
	}
	return match(0, 0)
}
