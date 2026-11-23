package codingtools

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxReferenceQueryBytes = 256 << 10
	maxReferenceCandidates = 64
	maxReferencedFiles     = 8
	maxReferencePathBytes  = 4096
	maxReferenceFileScan   = 512 << 10
	maxReferenceTotalScan  = 2 << 20
	maxReferenceHeadings   = 16
	maxHeadingTitleRunes   = 96
)

// FileReferenceMetadata describes a workspace file without attaching its
// contents. Counts are populated only after a complete UTF-8 text scan.
type FileReferenceMetadata struct {
	Path              string        `json:"path"`
	SizeBytes         int64         `json:"size_bytes"`
	LineCount         *int          `json:"line_count,omitempty"`
	CharacterCount    *int          `json:"character_count,omitempty"` // Unicode code points.
	Headings          []FileHeading `json:"headings,omitempty"`
	HeadingsTruncated bool          `json:"headings_truncated,omitempty"`
}

type FileHeading struct {
	Line  int    `json:"line"`
	Level int    `json:"level"`
	Title string `json:"title"`
}

// InspectFileReferences resolves file mentions in the same workspace view as
// workspace_read. It prioritizes explicit @ references over inferred paths.
// allow, when supplied, checks each normalized path before it is resolved.
// Missing, unreadable and unsafe candidates do not become metadata.
func (workspace *Workspace) InspectFileReferences(ctx context.Context, query string, allow func(path string) bool) ([]FileReferenceMetadata, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	candidates := referencedPaths(query)
	if len(candidates) == 0 {
		return nil, nil
	}
	root, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	files, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer files.Close()

	remainingScan := maxReferenceTotalScan
	seen := make(map[string]bool)
	var result []FileReferenceMetadata
	for _, candidate := range candidates {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		relative, err := relativeReference(root, candidate)
		if err != nil || relative == "." || relative == "" || (allow != nil && !allow(relative)) {
			continue
		}
		path, relative, err := workspace.resolveExistingAt(root, relative)
		if err != nil || seen[path] {
			continue
		}
		resolved, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		metadata, consumed, ok := inspectFileReference(files, resolved, relative, min(maxReferenceFileScan, remainingScan))
		remainingScan -= consumed
		if !ok {
			continue
		}
		seen[path] = true
		result = append(result, metadata)
		if len(result) == maxReferencedFiles {
			break
		}
	}
	return result, contextError(ctx)
}

func relativeReference(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
	}
	return normalizeRelative(path)
}

func inspectFileReference(root *os.Root, resolved, relative string, scanLimit int) (FileReferenceMetadata, int, bool) {
	// Avoid opening nonregular files such as named pipes while collecting hints.
	info, err := root.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return FileReferenceMetadata{}, 0, false
	}
	file, info, err := openRootRegular(root, resolved)
	if err != nil {
		return FileReferenceMetadata{}, 0, false
	}
	defer file.Close()
	metadata := FileReferenceMetadata{Path: relative, SizeBytes: info.Size()}
	markdown := isMarkdownFile(relative)
	if scanLimit == 0 || (info.Size() > int64(scanLimit) && !markdown) {
		return metadata, 0, true
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(scanLimit)))
	consumed := len(data)
	if err != nil {
		return FileReferenceMetadata{}, consumed, false
	}
	latest, err := file.Stat()
	if err != nil {
		return FileReferenceMetadata{}, consumed, false
	}
	if latest.Size() != info.Size() || !latest.ModTime().Equal(info.ModTime()) {
		metadata.SizeBytes = latest.Size()
		return metadata, consumed, true
	}
	complete := int64(consumed) == info.Size()
	if !complete {
		// The prefix can end in a partial line or UTF-8 character.
		data = data[:bytes.LastIndexByte(data, '\n')+1]
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return metadata, consumed, true
	}
	if complete {
		lines := bytes.Count(data, []byte{'\n'})
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}
		characters := utf8.RuneCount(data)
		metadata.LineCount, metadata.CharacterCount = &lines, &characters
	}
	if markdown {
		metadata.Headings, metadata.HeadingsTruncated = markdownHeadings(data)
		metadata.HeadingsTruncated = metadata.HeadingsTruncated || !complete
	}
	return metadata, consumed, true
}

func isMarkdownFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	default:
		return false
	}
}

var fileReferenceLocator = regexp.MustCompile(`(?:(?::[0-9]+){1,2}|#L[0-9]+(?:-L[0-9]+)?)$`)

func referencedPaths(query string) []string {
	if len(query) > maxReferenceQueryBytes {
		query = query[:maxReferenceQueryBytes]
		// Avoid treating a path that crosses the scan boundary as a file.
		if boundary := strings.LastIndexFunc(query, referenceDelimiter); boundary >= 0 {
			query = query[:boundary]
		} else {
			return nil
		}
	}
	var explicit, inferred []string
	seen := make(map[string]bool)
	for query != "" && len(explicit) < maxReferenceCandidates {
		query = strings.TrimLeftFunc(query, referenceSeparator)
		if query == "" {
			break
		}
		marked := strings.HasPrefix(query, "@")
		if marked {
			query = query[1:]
		}
		value, tail, quoted := referenceToken(query)
		query = tail
		if strings.HasPrefix(value, "@") {
			value, marked = value[1:], true
		}
		if !quoted {
			value = strings.TrimRight(value, ".:")
		}
		value = fileReferenceLocator.ReplaceAllString(value, "")
		if value == "" || len(value) > maxReferencePathBytes || strings.Contains(value, "://") || strings.HasPrefix(value, "~/") || (!marked && strings.Contains(value, "@")) {
			continue
		}
		if !marked && !quoted && !strings.Contains(value, "/") && filepath.Ext(value) == "" {
			continue
		}
		path := filepath.Clean(value)
		if !filepath.IsAbs(path) {
			var err error
			path, err = normalizeRelative(path)
			if err != nil || path == "." {
				continue
			}
		}
		key := path
		if marked {
			key = "@" + path
		}
		if seen[key] {
			continue
		}
		if marked {
			explicit = append(explicit, path)
		} else if len(inferred) < maxReferenceCandidates {
			inferred = append(inferred, path)
		} else {
			continue
		}
		seen[key] = true
	}
	return append(explicit, inferred[:min(len(inferred), maxReferenceCandidates-len(explicit))]...)
}

func referenceToken(query string) (value, tail string, quoted bool) {
	if query == "" {
		return "", "", false
	}
	if quote := query[0]; quote == '"' || quote == '\'' || quote == '`' {
		if end := strings.IndexByte(query[1:], quote); end >= 0 {
			return query[1 : end+1], query[end+2:], true
		}
		return "", query[1:], false
	}
	var previous rune
	for i, current := range query {
		if referenceDelimiter(current) || (current == '@' && unicode.Is(unicode.Han, previous)) {
			return query[:i], query[i:], false
		}
		previous = current
	}
	return query, "", false
}

func referenceSeparator(r rune) bool {
	return referenceDelimiter(r) && r != '"' && r != '\'' && r != '`'
}

func referenceDelimiter(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("[](){}<>,;?!\"'`", r) || (r > unicode.MaxASCII && unicode.IsPunct(r))
}

func markdownHeadings(data []byte) ([]FileHeading, bool) {
	var headings []FileHeading
	var fence byte
	fenceLength := 0
	lineNumber := 0
	for raw := range bytes.SplitSeq(data, []byte{'\n'}) {
		lineNumber++
		line := bytes.TrimLeft(raw, " ")
		if len(raw)-len(line) > 3 || len(line) == 0 {
			continue
		}
		marker := line[0]
		if marker == '`' || marker == '~' {
			length := markerCount(line, marker)
			if fence == 0 && length >= 3 {
				fence, fenceLength = marker, length
			} else if marker == fence && length >= fenceLength && len(bytes.TrimSpace(line[length:])) == 0 {
				fence, fenceLength = 0, 0
			}
			continue
		}
		if fence != 0 || marker != '#' {
			continue
		}
		level := markerCount(line, '#')
		if level > 6 || level == len(line) || (line[level] != ' ' && line[level] != '\t') {
			continue
		}
		body := bytes.TrimRight(line[level:], " \t\r")
		if suffix := bytes.TrimRight(body, "#"); len(suffix) < len(body) && (len(suffix) == 0 || suffix[len(suffix)-1] == ' ' || suffix[len(suffix)-1] == '\t') {
			body = suffix
		}
		title := strings.Join(strings.Fields(string(body)), " ")
		if title == "" {
			continue
		}
		if len(headings) == maxReferenceHeadings {
			return headings, true
		}
		if runes := []rune(title); len(runes) > maxHeadingTitleRunes {
			title = string(runes[:maxHeadingTitleRunes]) + "…"
		}
		headings = append(headings, FileHeading{Line: lineNumber, Level: level, Title: title})
	}
	return headings, false
}

func markerCount(line []byte, marker byte) int {
	count := 0
	for count < len(line) && line[count] == marker {
		count++
	}
	return count
}
