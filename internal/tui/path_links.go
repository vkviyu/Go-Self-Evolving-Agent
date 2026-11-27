package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Match Glamour's OSC 8 links and SGR styles as whole tokens. A path in a
// hyperlink destination is metadata, even when its visible label is wrapped.
var fileLinkTokenPattern = regexp.MustCompile(`\x1b\]8;[^;\x07\x1b]*;[^\x1b\x07]*(?:\x07|\x1b\\)|\x1b\[[0-9;:]*m|(?:(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+|/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+)(?::[0-9]+|#L[0-9]+)?`)

const fileLinkPrefix = "\x1b]8;;"

// linkFilePaths annotates existing files in rendered text with OSC 8 links.
// Apply it after Markdown rendering, which processes link syntax and would
// otherwise interpret the control sequences as part of the source text.
func linkFilePaths(content, workspace string) string {
	if !strings.Contains(content, "/") && !strings.Contains(content, "\x1b]8;") {
		return content
	}
	workspace = strings.TrimSpace(workspace)
	if workspace != "" {
		workspace, _ = filepath.Abs(workspace)
	}
	var output strings.Builder
	last := 0
	inLink, emittedLink := false, false
	for _, match := range fileLinkTokenPattern.FindAllStringIndex(content, -1) {
		token := content[match[0]:match[1]]
		if body, ok := strings.CutPrefix(token, "\x1b]8;"); ok {
			_, target, _ := strings.Cut(body, ";")
			target = target[:strings.IndexAny(target, "\a\x1b")] // BEL or ST
			output.WriteString(content[last:match[0]])
			last = match[1]
			if emittedLink {
				output.WriteString(ansi.ResetHyperlink())
			}
			inLink, emittedLink = target != "", false
			if inLink {
				if dest := markdownLinkTarget(target, workspace); dest != "" {
					output.WriteString(ansi.SetHyperlink(dest))
					emittedLink = true
				}
			}
			continue
		}
		if inLink || token[0] == '\x1b' {
			continue
		}
		pathToken, lineSuffix := splitPathLine(token)
		target, ok := resolveFileToken(pathToken, workspace)
		if !ok {
			continue
		}
		output.WriteString(content[last:match[0]])
		output.WriteString(ansi.SetHyperlink(fileLinkURL(target, lineSuffix)) + token + ansi.ResetHyperlink())
		last = match[1]
	}
	if last == 0 {
		return content
	}
	output.WriteString(content[last:])
	return output.String()
}

func splitPathLine(token string) (string, string) {
	if path, line, ok := strings.Cut(token, "#L"); ok {
		return path, "#L" + line
	}
	index := strings.LastIndexByte(token, ':')
	if index <= 0 || index == len(token)-1 {
		return token, ""
	}
	for _, char := range token[index+1:] {
		if char < '0' || char > '9' {
			return token, ""
		}
	}
	return token[:index], "#L" + token[index+1:]
}

func resolveFileToken(token, workspace string) (string, bool) {
	if workspace == "" {
		return "", false
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", false
	}
	absolute := token
	if filepath.IsAbs(token) {
		absolute, err = filepath.Abs(token)
		if err != nil {
			return "", false
		}
	} else {
		absolute = filepath.Join(root, filepath.FromSlash(token))
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	// OpenRoot also protects ancestor replacement races; resolving the token
	// with EvalSymlinks/Stat first would probe outside the trusted workspace.
	boundary, err := os.OpenRoot(root)
	if err != nil {
		return "", false
	}
	defer boundary.Close()
	info, err := boundary.Stat(relative)
	if err != nil || info.IsDir() {
		return "", false
	}
	return absolute, true
}

// markdownLinkTarget preserves web links and resolves local citations against
// the same workspace boundary as bare paths in tool output.
func markdownLinkTarget(target, workspace string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.String()
	}
	if (u.Scheme != "" && u.Scheme != "file") || u.Host != "" || u.Path == "" {
		return ""
	}
	path, line := splitPathLine(u.Path)
	if u.Fragment != "" {
		line = "#" + u.Fragment
	}
	resolved, ok := resolveFileToken(path, workspace)
	if !ok {
		return ""
	}
	return fileLinkURL(resolved, line)
}

func fileLinkURL(path, fragment string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), Fragment: strings.TrimPrefix(fragment, "#")}).String()
}

// The viewport and terminal renderer can display any row on its own. Close a
// wrapped link at the end of each row and reopen it on the next with the same
// target so scrolling or a partial redraw cannot leave a link active.
func scopeFileLinksByLine(content string) string {
	if !strings.Contains(content, fileLinkPrefix) {
		return content
	}
	var output strings.Builder
	active := ""
	for line, rest, more := strings.Cut(content, "\n"); ; line, rest, more = strings.Cut(rest, "\n") {
		output.WriteString(active)
		output.WriteString(line)
		if index := strings.LastIndex(line, fileLinkPrefix); index >= 0 {
			target, _, _ := strings.Cut(line[index+len(fileLinkPrefix):], "\a")
			active = ""
			if target != "" {
				active = ansi.SetHyperlink(target)
			}
		}
		if active != "" {
			output.WriteString(ansi.ResetHyperlink())
		}
		if !more {
			break
		}
		output.WriteByte('\n')
	}
	return output.String()
}
