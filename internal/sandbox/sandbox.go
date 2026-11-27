// Package sandbox provides the operating-system boundary for shell tools.
// A missing backend is an explicit error: callers must never silently fall
// back to an unsandboxed subprocess.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrUnavailable = errors.New("operating-system shell sandbox is unavailable")

// Request is the fully resolved command passed to a backend.
type Request struct {
	Command          string
	WorkingDirectory string
	// WorkspaceRoot may narrow, but never widen, the runner's configured root.
	WorkspaceRoot string
	// ReadOnly forbids workspace writes while retaining private scratch.
	ReadOnly bool
	// ReadOnlyPaths is authority supplied by trusted host adapters only,
	// never by model tool arguments.
	ReadOnlyPaths []string
	Environment   []string
	// InheritedFiles become descriptors 3 and up of the command. The caller
	// keeps ownership and closes them after Run returns.
	InheritedFiles []*os.File
	temporary      string
	readPaths      []string
}

// Runner executes a command inside a platform sandbox.
type Runner interface {
	// Run executes the request under the configured sandbox and streams stdout and stderr
	// to their respective writers.
	Run(context.Context, Request, io.Writer, io.Writer) error
	// Name identifies the sandbox implementation used to run the request.
	Name() string
}

// Mode selects where the shell isolation boundary is enforced. ModeOS uses
// the host operating system sandbox. ModeContainer delegates that boundary to
// an already-isolated container and is intentionally opt-in.
type Mode string

const (
	ModeOS        Mode = "os"
	ModeContainer Mode = "container"
)

// New selects the strongest supported backend for the current platform.
func New(workspace string) (Runner, error) {
	return NewWithMode(workspace, ModeOS)
}

// NewWithMode constructs a runner for the selected isolation boundary.
func NewWithMode(workspace string, mode Mode) (Runner, error) {
	workspace, err := canonicalDirectory(workspace)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = ModeOS
	}
	if mode == ModeContainer {
		if !runningInContainer() {
			return nil, fmt.Errorf("%w: container mode requires an existing container boundary", ErrUnavailable)
		}
		return &containerRunner{workspace: workspace}, nil
	}
	if mode != ModeOS {
		return nil, fmt.Errorf("%w: unknown sandbox mode %q", ErrUnavailable, mode)
	}
	switch runtime.GOOS {
	case "darwin":
		path, err := exec.LookPath("sandbox-exec")
		if err != nil {
			return nil, fmt.Errorf("%w: sandbox-exec was not found", ErrUnavailable)
		}
		return &seatbeltRunner{executable: path, workspace: workspace}, nil
	case "linux":
		path, err := exec.LookPath("bwrap")
		if err != nil {
			path, err = exec.LookPath("bubblewrap")
		}
		if err != nil {
			return nil, fmt.Errorf("%w: bubblewrap was not found", ErrUnavailable)
		}
		return &bubblewrapRunner{executable: path, workspace: workspace}, nil
	default:
		return nil, fmt.Errorf("%w: no backend for %s", ErrUnavailable, runtime.GOOS)
	}
}

// canonicalDirectory resolves the workspace to an absolute physical directory for sandbox
// configuration.
func canonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("sandbox workspace is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox workspace: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve sandbox workspace symlinks: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect sandbox workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("sandbox workspace is not a directory")
	}
	return canonical, nil
}

type seatbeltRunner struct {
	executable string
	workspace  string
}

func (runner *seatbeltRunner) Name() string { return "macos-seatbelt" }

func (runner *seatbeltRunner) Run(ctx context.Context, request Request, stdout, stderr io.Writer) error {
	request, cleanup, err := prepareInvocation(runner.workspace, request)
	if err != nil {
		return err
	}
	defer cleanup()
	profile := invocationSeatbeltProfile(request)
	command := exec.CommandContext(ctx, runner.executable, "-p", profile, "/bin/sh", "-c", request.Command)
	command.Dir = request.WorkingDirectory
	command.Env = sanitizedEnvironment(request.Environment)
	command.ExtraFiles = request.InheritedFiles
	command.Stdout = stdout
	command.Stderr = stderr
	return runProcessGroup(command)
}

type bubblewrapRunner struct {
	executable string
	workspace  string
}

func (runner *bubblewrapRunner) Name() string { return "linux-bubblewrap" }

func (runner *bubblewrapRunner) Run(ctx context.Context, request Request, stdout, stderr io.Writer) error {
	request, cleanup, err := prepareInvocation(runner.workspace, request)
	if err != nil {
		return err
	}
	defer cleanup()
	command := exec.CommandContext(ctx, runner.executable, bubblewrapArguments(request.WorkspaceRoot, request)...)
	command.ExtraFiles = request.InheritedFiles
	command.Stdout = stdout
	command.Stderr = stderr
	return runProcessGroup(command)
}

func bubblewrapArguments(workspace string, request Request) []string {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-net",
		"--unshare-pid",
		"--unshare-ipc",
		"--tmpfs", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--clearenv",
	}
	for _, path := range request.readPaths {
		args = append(args, "--ro-bind", path, path)
	}
	// Merged-/usr distributions expose /bin and /lib via symlinks. Recreate
	// these aliases without binding any additional host directory.
	for _, alias := range []string{"/bin", "/sbin", "/lib", "/lib64", "/etc"} {
		info, err := os.Lstat(alias)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := filepath.EvalSymlinks(alias)
		if err != nil {
			continue
		}
		for _, path := range request.readPaths {
			if insideDirectory(path, target) {
				args = append(args, "--symlink", target, alias)
				break
			}
		}
	}
	workspaceBind := "--bind"
	if request.ReadOnly {
		workspaceBind = "--ro-bind"
	}
	args = append(args, workspaceBind, workspace, workspace)
	if request.temporary != "" {
		args = append(args, "--bind", request.temporary, request.temporary)
	}
	args = append(args, "--chdir", request.WorkingDirectory)
	for _, value := range sanitizedEnvironment(request.Environment) {
		key, item, ok := strings.Cut(value, "=")
		if ok {
			args = append(args, "--setenv", key, item)
		}
	}
	args = append(args, "/bin/sh", "-c", request.Command)
	return args
}

// containerRunner executes directly in a pre-existing container. The caller
// owns that container's filesystem, process and network policy; this runner is
// never selected implicitly and still strips credentials from child commands.
type containerRunner struct {
	workspace string
}

func (runner *containerRunner) Name() string { return "outer-container" }

func (runner *containerRunner) Run(ctx context.Context, request Request, stdout, stderr io.Writer) error {
	if request.ReadOnly {
		return errors.New("outer container cannot enforce a read-only invocation boundary")
	}
	if request.WorkspaceRoot != "" {
		root, err := canonicalDirectory(request.WorkspaceRoot)
		if err != nil {
			return err
		}
		if root != runner.workspace {
			return errors.New("outer container cannot enforce a narrower Agent worktree boundary")
		}
	}
	command := exec.CommandContext(ctx, "/bin/sh", "-c", request.Command)
	command.Dir = request.WorkingDirectory
	command.Env = sanitizedEnvironment(request.Environment)
	command.ExtraFiles = request.InheritedFiles
	command.Stdout = stdout
	command.Stderr = stderr
	return runProcessGroup(command)
}

func runningInContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	cgroup := strings.ToLower(string(data))
	for _, marker := range []string{"docker", "containerd", "kubepods", "libpod"} {
		if strings.Contains(cgroup, marker) {
			return true
		}
	}
	return false
}

func escapeSBPL(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

func sanitizedEnvironment(input []string) []string {
	if len(input) == 0 {
		input = os.Environ()
	}
	result := make([]string, 0, len(input)+2)
	seen := make(map[string]struct{}, len(input))
	for _, item := range input {
		key, _, ok := strings.Cut(item, "=")
		if !ok || sensitiveEnvironmentKey(key) || !allowedEnvironmentKey(key) {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, item)
	}
	result = append(result, "GSEA_SANDBOX=1")
	return result
}

// Inherited host variables are capabilities too (DSNs, sockets and loader
// settings can carry secrets or execute code). A denylist alone cannot cover
// them. Keep only the documented locale and offline toolchain inputs.
func allowedEnvironmentKey(key string) bool {
	switch key {
	case "PATH", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TZ", "TERM", "COLORTERM", "NO_COLOR", "CLICOLOR",
		"HOME", "TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "GOCACHE", "GOROOT", "GOPATH", "GOMODCACHE",
		"GOTOOLCHAIN", "GOENV", "GOWORK", "GOPROXY", "GOSUMDB", "CGO_ENABLED", "GOOS", "GOARCH", "GOMAXPROCS", "GOMEMLIMIT":
		return true
	default:
		return false
	}
}

func sensitiveEnvironmentKey(key string) bool {
	key = strings.ToUpper(key)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PRIVATE"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
