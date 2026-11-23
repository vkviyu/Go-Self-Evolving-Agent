package codingtools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacecoord"
	"github.com/vkviyu/go-self-evolving-agent/internal/workspacedomain"
)

func TestWorkspaceReadAndLineRange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.read(context.Background(), json.RawMessage(`{"path":"main.go","start_line":2,"end_line":3}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Content != "two\nthree\n" {
		t.Fatalf("content = %q", decoded.Content)
	}
	digest := sha256.Sum256([]byte("one\ntwo\nthree\n"))
	if decoded.SHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("sha256 = %q, want %x", decoded.SHA256, digest)
	}
}

func TestWorkspaceGlobSupportsRecursivePatterns(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a.go", filepath.Join("internal", "nested", "b.go"), "note.txt"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.glob(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Paths) != 2 || decoded.Paths[0] != "a.go" || decoded.Paths[1] != "internal/nested/b.go" {
		t.Fatalf("paths = %#v", decoded.Paths)
	}
}

func TestWorkspaceRejectsEscapeAndSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "link/secret", filepath.Join(outside, "secret")} {
		if _, err := workspace.read(context.Background(), json.RawMessage(`{"path":"`+strings.ReplaceAll(path, `\`, `\\`)+`"}`)); err == nil {
			t.Fatalf("path %q unexpectedly succeeded", path)
		}
	}
	if _, err := workspace.glob(context.Background(), json.RawMessage(`{"pattern":"a/../../*"}`)); err == nil {
		t.Fatal("escaping glob unexpectedly succeeded")
	}
}

func TestWorkspaceEditRequiresUniqueMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("x\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = workspace.edit(context.Background(), json.RawMessage(`{"path":"file.txt","old_text":"x","new_text":"y"}`))
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("error = %v", err)
	}
	if _, err := workspace.edit(context.Background(), json.RawMessage(`{"path":"file.txt","old_text":"x","new_text":"y","replace_all":true}`)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "y\ny\n" {
		t.Fatalf("file = %q", data)
	}
}

func TestWorkspaceWriteReturnsUnifiedDiff(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc old() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.write(context.Background(), json.RawMessage(`{"path":"main.go","content":"package main\n\nfunc new() {}\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Diff struct {
			Status  string `json:"status"`
			Content string `json:"content"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Diff.Status != "ready" {
		t.Fatalf("diff status = %q, want ready: %s", decoded.Diff.Status, result)
	}
	for _, expected := range []string{"--- a/main.go", "+++ b/main.go", "-func old() {}", "+func new() {}"} {
		if !strings.Contains(decoded.Diff.Content, expected) {
			t.Fatalf("diff missing %q: %q", expected, decoded.Diff.Content)
		}
	}
}

func TestWorkspaceWriteReportsBinaryDiffStatus(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.write(context.Background(), json.RawMessage(`{"path":"data.bin","content":"changed"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Diff struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"diff"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Diff.Status != "binary" || decoded.Diff.Message == "" {
		t.Fatalf("binary diff = %s", result)
	}
}

func TestWorkspaceWriteAndEditRejectStaleFingerprint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fingerprint.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("before"))
	valid := fmt.Sprintf("%x", digest)
	if _, err := workspace.write(context.Background(), json.RawMessage(`{"path":"fingerprint.txt","content":"after","expected_sha256":"`+valid+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.edit(context.Background(), json.RawMessage(`{"path":"fingerprint.txt","old_text":"after","new_text":"final","expected_sha256":"`+valid+`"}`)); err == nil {
		t.Fatal("stale edit fingerprint was accepted")
	}
}

func TestWorkspaceWriteReturnsPassiveLanguageDiagnostics(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &fakeDiagnostics{report: lsp.Report{
		Status: lsp.StatusReady, Server: "fake", Path: "main.go", Diagnostics: []lsp.Diagnostic{{
			Path: "main.go", Start: lsp.Position{Line: 1, Column: 1}, End: lsp.Position{Line: 1, Column: 2}, Severity: "error", Message: "broken",
		}},
	}}
	workspace.AttachLanguageDiagnostics(reporter)
	result, err := workspace.write(context.Background(), json.RawMessage(`{"path":"main.go","content":"package main"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Path        string     `json:"path"`
		Diagnostics lsp.Report `json:"diagnostics"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Path != "main.go" || decoded.Diagnostics.Status != lsp.StatusReady || len(decoded.Diagnostics.Diagnostics) != 1 {
		t.Fatalf("result = %s", result)
	}
	if reporter.root != workspace.Root() || reporter.path != filepath.Join(workspace.Root(), "main.go") || string(reporter.content) != "package main" {
		t.Fatalf("diagnostic call = root %q path %q content %q", reporter.root, reporter.path, reporter.content)
	}

	foundBinding := false
	for _, binding := range workspace.Bindings() {
		if binding.Descriptor.Name == "workspace_diagnostics" {
			foundBinding = true
		}
	}
	if !foundBinding {
		t.Fatal("workspace_diagnostics binding was not installed")
	}
}

func TestWorkspaceWriteHonorsCancellationBeforeSideEffect(t *testing.T) {
	workspace, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = workspace.write(ctx, json.RawMessage(`{"path":"cancelled.txt","content":"must not be written"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Root(), "cancelled.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled write created a file: %v", err)
	}
}

func TestWorkspaceRoutesAgentViewAndRejectsReadonlyWrites(t *testing.T) {
	workspaceRoot := t.TempDir()
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	rootThread, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "workspace-view-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: rootThread.TeamID, ParentAgentID: rootThread.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	viewRoot := filepath.Join(workspaceRoot, ".gsea", "test-worktree")
	if err := os.MkdirAll(viewRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(viewRoot, "view.txt"), []byte("view content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: rootThread.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewGitWorktree,
		WorkspaceRoot: workspaceRoot, RootPath: viewRoot, BaseRevision: "base-revision",
		IdempotencyKey: "view-routing",
	}); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspace.AttachWorkspaceViewRepository(store)
	ctx := orchestration.WithActorScope(context.Background(), child.ActorScope())
	result, err := workspace.read(ctx, json.RawMessage(`{"path":"view.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Content != "view content" {
		t.Fatalf("view content = %q", decoded.Content)
	}
	// The trusted view root must reach the OS sandbox, not merely shell cwd.
	recorder := &scopeRecordingRunner{}
	workspace.shellRunner = recorder
	if _, err := workspace.shell(ctx, json.RawMessage(`{"command":"true"}`)); err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(viewRoot)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.request.WorkspaceRoot != wantRoot || recorder.request.WorkingDirectory != wantRoot {
		t.Fatalf("shell did not inherit child view boundary: %#v", recorder.request)
	}

	if err := os.WriteFile(filepath.Join(workspaceRoot, "read-only.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateView(context.Background(), workspacecoord.ViewSpec{
		TeamID: rootThread.TeamID, AgentID: child.ID, Mode: workspacecoord.ViewSharedReadonly,
		WorkspaceRoot: workspaceRoot, RootPath: workspaceRoot,
		IdempotencyKey: "readonly-routing",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.write(ctx, json.RawMessage(`{"path":"read-only.txt","content":"after"}`)); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("readonly write error = %v", err)
	}
}

type scopeRecordingRunner struct {
	request     sandbox.Request
	deadline    time.Time
	hasDeadline bool
}

func (*scopeRecordingRunner) Name() string { return "scope-recorder" }
func (runner *scopeRecordingRunner) Run(ctx context.Context, request sandbox.Request, _, _ io.Writer) error {
	runner.request = request
	runner.deadline, runner.hasDeadline = ctx.Deadline()
	return nil
}

// livenessRecorder issues descriptors the way a coordination domain would.
type livenessRecorder struct {
	directory string
	issued    []*os.File
}

func (recorder *livenessRecorder) DescendantLock() (*os.File, error) {
	file, err := os.CreateTemp(recorder.directory, "liveness-")
	if err != nil {
		return nil, err
	}
	recorder.issued = append(recorder.issued, file)
	return file, nil
}

func TestShellPassesDescendantLivenessToCommand(t *testing.T) {
	workspace, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recorder := &scopeRecordingRunner{}
	workspace.shellRunner = recorder
	liveness := &livenessRecorder{directory: t.TempDir()}
	workspace.AttachDescendantLiveness(liveness)
	if _, err := workspace.shell(context.Background(), json.RawMessage(`{"command":"true"}`)); err != nil {
		t.Fatal(err)
	}
	if len(liveness.issued) != 1 || len(recorder.request.InheritedFiles) != 1 || recorder.request.InheritedFiles[0] != liveness.issued[0] {
		t.Fatalf("command did not inherit the liveness descriptor: issued=%v inherited=%v", liveness.issued, recorder.request.InheritedFiles)
	}
	if _, err := liveness.issued[0].Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("liveness descriptor stayed open after the command: %v", err)
	}
}

func TestWorkspaceLeaseWithoutToolCallIDIsReacquiredForEachMutation(t *testing.T) {
	workspaceRoot := t.TempDir()
	database, err := testfixture.Open(t, context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "lease-library-root"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1"})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := New(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	domain, err := workspacedomain.Open(context.Background(), workspacedomain.Options{HomeDirectory: t.TempDir(), WorkspaceRoot: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = domain.Close() })
	workspace.AttachLeaseRepository(domain.Leases())
	ctx := orchestration.WithActorScope(context.Background(), child.ActorScope())
	var firstLease orchestration.LeaseID
	assertActive := func() (json.RawMessage, error) {
		leases, listErr := domain.Leases().ListLeases(ctx, orchestration.LeaseQuery{TeamID: child.TeamID, AgentID: child.ID, States: []orchestration.LeaseState{orchestration.LeaseActive}})
		if listErr != nil {
			return nil, listErr
		}
		if len(leases) != 1 {
			return nil, fmt.Errorf("active leases during mutation = %d, want 1", len(leases))
		}
		if firstLease == "" {
			firstLease = leases[0].ID
		} else if leases[0].ID == firstLease {
			return nil, errors.New("second mutation reused a released lease")
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	if _, err := workspace.withWriteLease(ctx, "file.txt", assertActive); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.withWriteLease(ctx, "file.txt", assertActive); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceShellReturnsExitCodeAndTimeout(t *testing.T) {
	workspace, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace.shellRunner = directTestRunner{}
	workspace.shellSandboxErr = nil
	result, err := workspace.shell(context.Background(), json.RawMessage(`{"command":"printf ok; exit 7"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Stdout != "ok" || decoded.ExitCode != 7 {
		t.Fatalf("result = %#v", decoded)
	}
	result, err = workspace.shell(context.Background(), json.RawMessage(`{"command":"sleep 1","timeout_ms":20}`))
	if err != nil {
		t.Fatal(err)
	}
	var timeout struct {
		TimedOut bool `json:"timed_out"`
	}
	if err := json.Unmarshal(result, &timeout); err != nil {
		t.Fatal(err)
	}
	if !timeout.TimedOut {
		t.Fatal("expected timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspace.shell(ctx, json.RawMessage(`{"command":"true"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestWorkspaceShellOmittedTimeoutUsesCallerContext(t *testing.T) {
	workspace, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &scopeRecordingRunner{}
	workspace.shellRunner, workspace.shellSandboxErr = runner, nil
	args := json.RawMessage(`{"command":"true"}`)
	if _, err := workspace.shell(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if runner.hasDeadline {
		t.Fatalf("omitted timeout installed a deadline: %v", runner.deadline)
	}
	callerDeadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()
	if _, err := workspace.shell(ctx, args); err != nil {
		t.Fatal(err)
	}
	if !runner.hasDeadline || !runner.deadline.Equal(callerDeadline) {
		t.Fatalf("command deadline = %v, want caller deadline %v", runner.deadline, callerDeadline)
	}
}

func TestWorkspaceShellCapturesChangesAfterParentCancellation(t *testing.T) {
	root := t.TempDir()
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := &mutationThenBlockRunner{started: make(chan struct{})}
	workspace.shellRunner = runner
	workspace.shellSandboxErr = nil
	diagnostics := &fakeChangeSetDiagnostics{report: lsp.Report{Status: lsp.StatusReady, Diagnostics: []lsp.Diagnostic{}}}
	workspace.AttachLanguageDiagnostics(diagnostics)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-runner.started
		cancel()
	}()
	_, err = workspace.shell(ctx, json.RawMessage(`{"command":"mutate"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shell cancellation error = %v, want context canceled", err)
	}
	if len(diagnostics.set.Changes) != 1 || diagnostics.set.Changes[0].Relative != "changed.go" {
		t.Fatalf("captured changes = %#v, want changed.go", diagnostics.set.Changes)
	}
}

func TestWorkspaceShellReportsCreatedModifiedDeletedAndDiagnosesSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.go"), []byte("package old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deleted.go"), []byte("package deleted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace.shellRunner = directTestRunner{}
	workspace.shellSandboxErr = nil
	diagnostics := &fakeChangeSetDiagnostics{report: lsp.Report{Status: lsp.StatusReady, Diagnostics: []lsp.Diagnostic{}}}
	workspace.AttachLanguageDiagnostics(diagnostics)
	result, err := workspace.shell(context.Background(), json.RawMessage(`{"command":"printf 'package changed\\n' > existing.go; printf 'package created\\n' > created.go; rm deleted.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Changes []lsp.ChangeSummary `json:"changes"`
		Diffs   []struct {
			Path    string `json:"path"`
			Status  string `json:"status"`
			Content string `json:"content"`
		} `json:"diffs"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]lsp.ChangeKind{"created.go": lsp.ChangeCreated, "deleted.go": lsp.ChangeDeleted, "existing.go": lsp.ChangeModified}
	if len(decoded.Changes) != len(want) {
		t.Fatalf("changes = %#v", decoded.Changes)
	}
	for _, change := range decoded.Changes {
		if want[change.Path] != change.Kind {
			t.Fatalf("change = %#v, want %s", change, want[change.Path])
		}
	}
	if len(decoded.Diffs) != len(want) {
		t.Fatalf("diffs = %#v", decoded.Diffs)
	}
	for _, diff := range decoded.Diffs {
		if diff.Status != "ready" || diff.Content == "" {
			t.Fatalf("diff = %#v", diff)
		}
	}
	if len(diagnostics.set.Changes) != 3 {
		t.Fatalf("diagnostic change set = %#v", diagnostics.set.Changes)
	}
}

type directTestRunner struct{}

func (directTestRunner) Name() string { return "test-direct" }

func (directTestRunner) Run(ctx context.Context, request sandbox.Request, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, "sh", "-c", request.Command)
	command.Dir = request.WorkingDirectory
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

type mutationThenBlockRunner struct {
	started chan struct{}
}

func (runner *mutationThenBlockRunner) Name() string { return "test-mutation-then-block" }

func (runner *mutationThenBlockRunner) Run(ctx context.Context, request sandbox.Request, _, _ io.Writer) error {
	if err := os.WriteFile(filepath.Join(request.WorkingDirectory, "changed.go"), []byte("package main\n"), 0o600); err != nil {
		return err
	}
	close(runner.started)
	<-ctx.Done()
	return ctx.Err()
}

type fakeDiagnostics struct {
	report  lsp.Report
	root    string
	path    string
	content []byte
}

type fakeChangeSetDiagnostics struct {
	report lsp.Report
	set    lsp.ChangeSet
}

func (f *fakeChangeSetDiagnostics) DiagnoseChange(context.Context, string, string, []byte) lsp.Report {
	return f.report
}

func (f *fakeChangeSetDiagnostics) DiagnoseChanges(_ context.Context, set lsp.ChangeSet) lsp.Report {
	f.set = set
	return f.report
}

func (f *fakeDiagnostics) DiagnoseChange(_ context.Context, root, path string, content []byte) lsp.Report {
	f.root = root
	f.path = path
	f.content = append([]byte(nil), content...)
	return f.report
}
