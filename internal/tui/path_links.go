package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Consume SGR sequences separately so their final "m" cannot become the
// first character of a file path in styled Markdown.
var fileLinkTokenPattern = regexp.MustCompile(`\x1b\[[0-9;:]*m|(?:(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+|/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+)(?::[0-9]+|#L[0-9]+)?`)

const fileLinkPrefix = "\x1b]8;;"

// linkFilePaths annotates existing files in rendered text with OSC 8 links.
// Apply it after Markdown rendering, which processes link syntax and would
// otherwise interpret the control sequences as part of the source text.
func linkFilePaths(content, workspace string) string {
	if !strings.Contains(content, "/") {
		return content
	}
	workspace, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil {
		workspace = ""
	}
	var output strings.Builder
	last := 0
	for _, match := range fileLinkTokenPattern.FindAllStringIndex(content, -1) {
		token := content[match[0]:match[1]]
		if token[0] == '\x1b' {
			continue
		}
		pathToken, lineSuffix := splitPathLine(token)
		target, ok := resolveFileToken(pathToken, workspace)
		if !ok {
			continue
		}
		output.WriteString(content[last:match[0]])
		output.WriteString(osc8FileLink(target+lineSuffix, token))
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

func osc8FileLink(path, label string) string {
	link := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	if index := strings.LastIndex(link.Path, "#L"); index >= 0 {
		link.Fragment = link.Path[index+1:]
		link.Path = link.Path[:index]
	}
	return ansi.SetHyperlink(link.String()) + label + ansi.ResetHyperlink()
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
