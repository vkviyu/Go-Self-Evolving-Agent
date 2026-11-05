package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// prepareInvocation resolves authority from the trusted workspace view, not
// from shell text. Host user files and other Agent worktrees are not implicitly
// readable. Each invocation gets its own writable home/temp/build cache.
func prepareInvocation(configured string, request Request) (Request, func(), error) {
	root := request.WorkspaceRoot
	if root == "" {
		root = configured
	}
	root, err := canonicalDirectory(root)
	if err != nil {
		return request, nil, err
	}
	if !insideDirectory(configured, root) {
		return request, nil, fmt.Errorf("shell workspace %q escapes configured root", root)
	}
	directory := request.WorkingDirectory
	if directory == "" {
		directory = root
	}
	directory, err = canonicalDirectory(directory)
	if err != nil {
		return request, nil, err
	}
	if !insideDirectory(root, directory) {
		return request, nil, fmt.Errorf("shell working directory %q escapes Agent workspace", directory)
	}
	temporary, err := os.MkdirTemp("", "gsea-shell-")
	if err != nil {
		return request, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	canonical, err := filepath.EvalSymlinks(temporary)
	if err != nil {
		cleanup()
		return request, nil, err
	}
	request.WorkspaceRoot, request.WorkingDirectory, request.temporary = root, directory, canonical
	for _, path := range request.ReadOnlyPaths {
		path, err := canonicalDirectory(path)
		if err != nil {
			cleanup()
			return request, nil, fmt.Errorf("resolve trusted read-only path: %w", err)
		}
		home, _ := os.UserHomeDir()
		if broadReadPath(path, home) {
			cleanup()
			return request, nil, fmt.Errorf("trusted read-only path is too broad")
		}
		request.readPaths = append(request.readPaths, path)
	}
	for _, path := range shellReadPaths(request.Environment) {
		// A toolchain/PATH entry must not reopen the canonical workspace or
		// another Agent's view. The selected workspace is granted separately.
		if !insideDirectory(path, configured) && !insideDirectory(configured, path) {
			request.readPaths = append(request.readPaths, path)
		}
	}
	var environment []string
	for _, entry := range sanitizedEnvironment(request.Environment) {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "TMPDIR", "TMP", "TEMP", "GOCACHE", "XDG_CACHE_HOME", "GSEA_SANDBOX":
			continue
		case "PATH":
			_, value, _ := strings.Cut(entry, "=")
			var paths []string
			for _, path := range filepath.SplitList(value) {
				if !filepath.IsAbs(path) {
					continue
				}
				if canonical, err := filepath.EvalSymlinks(path); err == nil {
					paths = append(paths, canonical)
				}
			}
			entry = "PATH=" + strings.Join(paths, string(os.PathListSeparator))
		}
		environment = append(environment, entry)
	}
	for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "GOCACHE", "XDG_CACHE_HOME"} {
		environment = append(environment, key+"="+canonical)
	}
	request.Environment = environment
	return request, cleanup, nil
}

func insideDirectory(root, value string) bool {
	relative, err := filepath.Rel(root, value)
	return err == nil && filepath.IsLocal(relative)
}

func shellReadPaths(environment []string) []string {
	if len(environment) == 0 {
		environment = os.Environ()
	}
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	candidates := []string{"/bin", "/sbin", "/usr", "/lib", "/lib64", "/etc", "/private/etc", "/System/Library", "/System/Cryptexes", "/System/Volumes/Preboot/Cryptexes", "/Library/Developer", "/opt/homebrew/Cellar"}
	// Explicit toolchain installations are data, not the user's entire home.
	candidates = append(candidates, runtime.GOROOT(), values["GOROOT"], values["GOMODCACHE"])
	for _, path := range filepath.SplitList(values["GOPATH"]) {
		if path != "" {
			candidates = append(candidates, filepath.Join(path, "pkg", "mod"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "go", "pkg", "mod"))
	}
	// PATH permits executables in those directories, not their parent trees.
	candidates = append(candidates, filepath.SplitList(values["PATH"])...)
	seen := map[string]bool{}
	var paths []string
	home, _ := os.UserHomeDir()
	for _, path := range candidates {
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		if broadReadPath(path, home) {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			continue
		}
		// macOS paths often contain symlinks; Seatbelt checks canonical paths.
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || broadReadPath(canonical, home) {
			continue
		}
		// Use only canonical paths: binding a symlink source at its original
		// path could otherwise reopen a forbidden ancestor of the workspace.
		if !seen[canonical] {
			paths = append(paths, canonical)
			seen[canonical] = true
		}
	}
	return paths
}

func broadReadPath(path, home string) bool {
	return path == "/" || path == home || path == "/Users" || path == "/home" || path == "/private" || path == "/var" || path == "/private/var" || path == "/tmp" || path == "/private/tmp" || path == "/System/Volumes/Data"
}

func invocationSeatbeltProfile(request Request) string {
	var profile strings.Builder
	// dyld opens the root directory before loading its shared cache on recent
	// macOS. A literal grant permits that without reading the tree beneath it.
	profile.WriteString("(version 1)\n(deny default)\n(allow file-read-metadata)\n(allow file-read-data (literal \"/\"))\n(allow process-exec)\n(allow process-fork)\n(allow signal (target self))\n(allow sysctl-read)\n(allow file-read* (subpath \"/dev\"))\n(allow file-write* (literal \"/dev/null\"))\n")
	for _, path := range append(append([]string(nil), request.readPaths...), request.WorkspaceRoot, request.temporary) {
		if path != "" {
			fmt.Fprintf(&profile, "(allow file-read* (subpath \"%s\"))\n", escapeSBPL(path))
		}
	}
	writable := []string{request.temporary}
	if !request.ReadOnly {
		writable = append(writable, request.WorkspaceRoot)
	}
	for _, path := range writable {
		if path != "" {
			fmt.Fprintf(&profile, "(allow file-write* (subpath \"%s\"))\n", escapeSBPL(path))
		}
	}
	profile.WriteString("(deny network*)\n")
	return profile.String()
}
