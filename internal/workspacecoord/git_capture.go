package workspacecoord

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

const (
	protectedGitTimeout = 2 * time.Minute
	gitMetadataMaxBytes = 1 << 20
)

func (runner commandGit) executable(directory string) (string, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("locate Git executable: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root := runner.workspace
	if root == "" {
		root = directory
	}
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		if relative, err := filepath.Rel(canonical, path); err == nil && filepath.IsLocal(relative) {
			return "", errors.New("refusing to execute a Git binary from the Agent-writable workspace")
		}
	}
	return path, nil
}

// captureProtectedPatch uses only host-created repository configuration and
// index/objects. The edited .git pointer, includes, clean/process filters,
// hooks and credential settings never enter the capture repository. Its source
// object database and worktree are read-only to the OS-sandboxed Git process.
func (runner commandGit) captureProtectedPatch(ctx context.Context, repository string, view View) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, protectedGitTimeout)
	defer cancel()
	base := view.BaseRevision
	if len(base) != 40 && len(base) != 64 {
		return nil, errors.New("patch capture requires a full commit object ID")
	}
	if _, err := hex.DecodeString(base); err != nil {
		return nil, errors.New("patch capture requires a hexadecimal commit object ID")
	}
	executable, err := runner.executable(repository)
	if err != nil {
		return nil, err
	}
	common, err := runner.Run(ctx, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve source Git object metadata: %w", err)
	}
	metadata, err := filepath.EvalSymlinks(strings.TrimSpace(string(common)))
	if err != nil {
		return nil, fmt.Errorf("resolve source Git metadata: %w", err)
	}
	objectFormat := "sha1"
	if len(base) == 64 {
		objectFormat = "sha256"
	}
	scratch, err := os.MkdirTemp("", "gsea-protected-git-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	scratch, err = filepath.EvalSymlinks(scratch)
	if err != nil {
		return nil, err
	}
	gitDirectory := filepath.Join(scratch, "repository.git")
	run, err := sandbox.New(scratch)
	if err != nil {
		return nil, fmt.Errorf("protected Git capture requires an OS sandbox: %w", err)
	}
	// Plain metadata remains data: preserve repository-local exclusions and
	// attributes, but never copy its config, hooks, indexes or executable files.
	for _, name := range []string{"info/exclude", "info/attributes"} {
		if err := copyGitDataFile(metadata, filepath.Join(scratch, "metadata"), name); err != nil {
			return nil, err
		}
	}
	quote := quoteGitShell
	global := []string{executable, "--no-pager", "--literal-pathspecs", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.splitIndex=false", "-c", "core.attributesFile=" + os.DevNull, "-c", "safe.directory=" + view.RootPath}
	conversion, err := runner.captureConversionOptions(ctx, repository)
	if err != nil {
		return nil, err
	}
	global = append(global, conversion...)
	command := func(arguments ...string) string {
		words := append(append([]string{}, global...), arguments...)
		for index := range words {
			words[index] = quote(words[index])
		}
		return strings.Join(words, " ")
	}
	var script strings.Builder
	script.WriteString("set -eu\n")
	for _, entry := range []struct{ key, value string }{
		{"GIT_CONFIG_NOSYSTEM", "1"}, {"GIT_CONFIG_GLOBAL", os.DevNull}, {"GIT_TERMINAL_PROMPT", "0"}, {"GIT_NO_LAZY_FETCH", "1"},
		{"GIT_ALTERNATE_OBJECT_DIRECTORIES", strconv.Quote(filepath.Join(metadata, "objects"))},
	} {
		fmt.Fprintf(&script, "export %s=%s\n", entry.key, quote(entry.value))
	}
	script.WriteString(command("init", "--bare", "--quiet", "--object-format="+objectFormat, gitDirectory) + "\n")
	for _, name := range []string{"exclude", "attributes"} {
		path := filepath.Join(scratch, "metadata", "info", name)
		fmt.Fprintf(&script, "if test -f %s; then cp %s %s; fi\n", quote(path), quote(path), quote(filepath.Join(gitDirectory, "info", name)))
	}
	prefix := []string{"--git-dir=" + gitDirectory, "--work-tree=" + view.RootPath, "-C", view.RootPath}
	script.WriteString(command(append(append([]string{}, prefix...), "read-tree", base)...) + "\n")
	script.WriteString(command(append(append([]string{}, prefix...), "add", "--all", "--", ".")...) + "\n")
	script.WriteString("exec " + command(append(append([]string{}, prefix...), "diff", "--cached", "--binary", "--full-index", "--no-renames", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", base, "--")...) + "\n")
	stdout, stderr := &boundedGitOutput{limit: maxGitOutputBytes}, &boundedGitOutput{limit: 64 << 10}
	err = run.Run(ctx, sandbox.Request{
		Command: script.String(), WorkingDirectory: scratch, WorkspaceRoot: scratch,
		ReadOnlyPaths: []string{view.RootPath, metadata},
		Environment:   []string{"PATH=" + filepath.Dir(executable) + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C"},
	}, stdout, stderr)
	if err != nil {
		return nil, fmt.Errorf("protected Git patch capture: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// These options are finite data values, not executable Git configuration.
// Preserve ordinary checkout normalization without importing clean/smudge,
// process filters, hooks, config includes or credential helpers.
func (runner commandGit) captureConversionOptions(ctx context.Context, repository string) ([]string, error) {
	var options []string
	for _, setting := range []struct {
		key     string
		allowed string
	}{
		{"core.autocrlf", "true false input"}, {"core.eol", "lf crlf native"},
		{"core.filemode", "true false"}, {"core.ignorecase", "true false"},
		{"core.symlinks", "true false"}, {"core.precomposeunicode", "true false"},
	} {
		value, err := runner.Run(ctx, repository, "config", "--local", "--no-includes", "--get", setting.key)
		if err != nil {
			if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
				continue
			}
			return nil, fmt.Errorf("read bounded Git conversion setting: %w", err)
		}
		text := strings.ToLower(strings.TrimSpace(string(value)))
		if !strings.Contains(" "+setting.allowed+" ", " "+text+" ") || strings.ContainsAny(text, " \t\r\n") {
			return nil, fmt.Errorf("unsupported Git conversion setting %s", setting.key)
		}
		options = append(options, "-c", setting.key+"="+text)
	}
	return options, nil
}

func quoteGitShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func copyGitDataFile(source, target, relative string) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := openGitDataFile(root, relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read bounded Git metadata %q: %w", relative, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > gitMetadataMaxBytes {
		return fmt.Errorf("unsupported or oversized Git metadata %q", relative)
	}
	data, err := io.ReadAll(io.LimitReader(file, gitMetadataMaxBytes+1))
	if err != nil || len(data) > gitMetadataMaxBytes {
		return fmt.Errorf("read Git metadata %q: %w", relative, errors.Join(err, errors.New("metadata limit exceeded")))
	}
	path := filepath.Join(target, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
