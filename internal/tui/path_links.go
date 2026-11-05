package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var filePathTokenPattern = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+(?::[0-9]+)?|/[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+(?:\:[0-9]+)?`)

// linkFilePaths annotates existing files with OSC 8 links. The terminal may
// wrap the visible label, but the hyperlink target remains the complete path.
// Nonexistent or ambiguous tokens are left untouched so ordinary prose and
// tool output do not become misleading links.
func linkFilePaths(content, workspace string) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	workspace, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil {
		workspace = ""
	}
	var output strings.Builder
	last := 0
	for _, match := range filePathTokenPattern.FindAllStringIndex(content, -1) {
		token := content[match[0]:match[1]]
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
	target := link.String()
	return "\x1b]8;;" + target + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}
