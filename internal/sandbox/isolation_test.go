package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInvocationOnlyNarrowsWorkspaceAndRejectsSymlinkEscape(t *testing.T) {
	root, outside := canonicalTempDir(t), canonicalTempDir(t)
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(child, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		{WorkspaceRoot: outside},
		{WorkspaceRoot: child, WorkingDirectory: root},
		{WorkspaceRoot: child, WorkingDirectory: filepath.Join(child, "escape")},
	} {
		if _, cleanup, err := prepareInvocation(root, request); err == nil {
			cleanup()
			t.Fatalf("escape accepted: %#v", request)
		}
	}
	request, cleanup, err := prepareInvocation(root, Request{WorkspaceRoot: child, Environment: []string{"PATH=" + root + ":" + outside, "HOME=" + outside, "API_KEY=secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if request.WorkspaceRoot != child || request.WorkingDirectory != child {
		t.Fatalf("workspace not narrowed: %#v", request)
	}
	for _, path := range request.readPaths {
		if insideDirectory(path, root) || insideDirectory(root, path) {
			t.Fatalf("read grant reopens parent: %s", path)
		}
	}
	joined := strings.Join(request.Environment, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "HOME="+outside) || !strings.Contains(joined, "HOME="+request.temporary) {
		t.Fatalf("environment not isolated: %s", joined)
	}
	if _, err := os.Stat(request.temporary); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(request.temporary); !os.IsNotExist(err) {
		t.Fatalf("private scratch retained: %v", err)
	}
}

func TestReadPathsRejectBroadSymlinkTargets(t *testing.T) {
	root := canonicalTempDir(t)
	alias := filepath.Join(root, "bin")
	if err := os.Symlink("/", alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range shellReadPaths([]string{"PATH=" + alias}) {
		if path == "/" || path == alias {
			t.Fatalf("broad path accepted: %q", path)
		}
	}
}

func TestSandboxRestrictsChildReadsWritesAndAllowsPrivateScratch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no OS sandbox backend")
	}
	root, outside := canonicalTempDir(t), canonicalTempDir(t)
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	parentSecret := filepath.Join(root, "parent-secret")
	outsideSecret := filepath.Join(outside, "outside-secret")
	for _, path := range []string{parentSecret, outsideSecret} {
		if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(child, "outside")); err != nil {
		t.Fatal(err)
	}
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string) (string, string, error) {
		var stdout, stderr strings.Builder
		err := runner.Run(context.Background(), Request{Command: command, WorkingDirectory: child, WorkspaceRoot: child}, &stdout, &stderr)
		return stdout.String(), stderr.String(), err
	}
	stdout, stderr, err := run(`printf ok > inside; printf scratch > "$TMPDIR/data"; cat "$HOME/data"`)
	if err != nil || stdout != "scratch" {
		t.Fatalf("in-bound shell failed: stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	for _, path := range []string{parentSecret, outsideSecret, filepath.Join(child, "outside", "outside-secret")} {
		stdout, _, err := run("cat " + shellQuote(path))
		if err == nil || strings.Contains(stdout, "secret") {
			t.Fatalf("outside read succeeded: %s stdout=%s err=%v", path, stdout, err)
		}
	}
	for _, path := range []string{filepath.Join(root, "unauthorized"), filepath.Join(outside, "unauthorized")} {
		if _, _, err := run("printf bad > " + shellQuote(path)); err == nil {
			t.Fatalf("outside write accepted: %s", path)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("outside file created: %v", err)
		}
	}
	stdout, stderr, err = run(shellQuote(filepath.Join(runtime.GOROOT(), "bin", "go")) + " version")
	if err != nil || !strings.Contains(stdout, "go version") {
		t.Fatalf("Go toolchain unavailable: %s %s err=%v", stdout, stderr, err)
	}
}

func TestBubblewrapDoesNotExposeHostRootOrProcesses(t *testing.T) {
	root := canonicalTempDir(t)
	request, cleanup, err := prepareInvocation(root, Request{Command: "true"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	arguments := bubblewrapArguments(root, request)
	joined := strings.Join(arguments, " ")
	for _, flag := range []string{"--tmpfs /", "--unshare-net", "--unshare-pid", "--unshare-ipc", "--bind " + root + " " + root} {
		if !strings.Contains(joined, flag) {
			t.Errorf("missing %q: %s", flag, joined)
		}
	}
	for index, argument := range arguments {
		if (argument == "--ro-bind" || argument == "--bind") && arguments[index+1] == "/" {
			t.Fatal("host root exposed")
		}
	}
}

func TestSandboxBuildsGoWithPrivateCache(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no OS sandbox backend")
	}
	root := canonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() { println(\"sandbox-build-ok\") }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var stdout, stderr strings.Builder
	err = runner.Run(ctx, Request{Command: "CGO_ENABLED=0 GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off " + shellQuote(filepath.Join(runtime.GOROOT(), "bin", "go")) + " run main.go", WorkingDirectory: root}, &stdout, &stderr)
	if err != nil || !strings.Contains(stderr.String(), "sandbox-build-ok") {
		t.Fatalf("Go build/run failed: stdout=%s stderr=%s err=%v", stdout.String(), stderr.String(), err)
	}
}

func TestOuterContainerRefusesNarrowerView(t *testing.T) {
	root := canonicalTempDir(t)
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	runner := &containerRunner{workspace: root}
	var stdout, stderr strings.Builder
	if err := runner.Run(context.Background(), Request{Command: "true", WorkspaceRoot: child, WorkingDirectory: child}, &stdout, &stderr); err == nil {
		t.Fatal("outer container silently widened child authority")
	}
}
