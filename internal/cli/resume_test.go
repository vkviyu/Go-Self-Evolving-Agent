package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

func TestResumeDiscoversAndAdoptsNativeHistory(t *testing.T) {
	ctx := context.Background()
	home, workspace := t.TempDir(), t.TempDir()
	t.Chdir(workspace)
	cfg := cliCommandConfig(t, home, workspace)
	var stdout, stderr bytes.Buffer
	fake := &chatTestModel{}
	deps := dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
		return fake, nil
	}}
	run := func(input string, dependencies dependencies, args ...string) string {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if exit := runWithDependencies(ctx, args, strings.NewReader(input), &stdout, &stderr, dependencies); exit != 0 {
			t.Fatalf("%v exited %d: %s", args, exit, stderr.String())
		}
		return stdout.String()
	}
	if got := run("first question\n/exit\n", deps, "chat", "--model", "test-model"); got != "reply: first question\n" {
		t.Fatalf("first chat output = %q", got)
	}
	entries, _, err := app.ReadChatCatalog(ctx, cfg, "", true)
	if err != nil || len(entries) != 1 || entries[0].State != sessionregistry.StateStopped || entries[0].DataDirectory != canonicalDirectory(home) {
		t.Fatalf("automatic session registration = %+v, %v", entries, err)
	}
	item := entries[0]
	if got := run("", dependencies{}, "resume", "--list"); !strings.Contains(got, "first question") || !strings.Contains(got, item.ID) {
		t.Fatalf("catalog listing = %q", got)
	}
	func() {
		registry, err := app.OpenSessionRegistry(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer registry.Close()
		if err := registry.Remove(ctx, item.ID); err != nil {
			t.Fatal(err)
		}
	}()
	seedNonConversationEvents(t, ctx, cfg)
	if got := run("", dependencies{}, "resume", "--list"); strings.Contains(got, item.ID) {
		t.Fatalf("uncataloged native history without workspace metadata leaked into workspace scope: %q", got)
	}
	if got := run("", dependencies{}, "resume", "--list", "--all"); !strings.Contains(got, item.ID) || !strings.Contains(got, "first question") || strings.Contains(got, "agent-thread_") || strings.Contains(got, "doctor-") {
		t.Fatalf("native history discovery = %q", got)
	}
	var resumeProgressMu sync.Mutex
	resumeBatches := make(map[uint64]struct {
		ownership bool
		start     bool
	})
	resumeCtx := ateprocess.WithObserver(ctx, func(_ context.Context, event ateprocess.Progress) {
		resumeProgressMu.Lock()
		progress := resumeBatches[event.BatchID]
		switch event.Stage {
		case ateprocess.StageOwnership:
			progress.ownership = true
		case ateprocess.StageStart:
			progress.start = true
		}
		resumeBatches[event.BatchID] = progress
		resumeProgressMu.Unlock()
	})
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDependencies(resumeCtx, []string{"resume", item.ID, "--model", "test-model"}, strings.NewReader("follow up\n/exit\n"), &stdout, &stderr, deps); exit != 0 {
		t.Fatalf("resume exited %d: %s", exit, stderr.String())
	}
	if got := stdout.String(); got != "reply: follow up\n" || fake.lastInputCount != 4 {
		t.Fatalf("native history was not loaded: output=%q model input=%d", got, fake.lastInputCount)
	}
	resumeProgressMu.Lock()
	ownedStarts := 0
	for _, progress := range resumeBatches {
		if progress.ownership && progress.start {
			ownedStarts++
		}
	}
	resumeProgressMu.Unlock()
	if ownedStarts != 1 {
		t.Fatalf("explicit resume opened %d owned ATE connections, want 1", ownedStarts)
	}
	registered, _, err := app.ReadChatCatalog(ctx, cfg, session.ID(item.ID), false)
	if err != nil || len(registered) != 1 || registered[0].Workspace != canonicalDirectory(workspace) || registered[0].DataDirectory != canonicalDirectory(home) {
		t.Fatalf("native history lost its workspace or runtime directory: %+v %v", registered, err)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDependencies(ctx, []string{"resume", "missing"}, strings.NewReader(""), &stdout, &stderr, dependencies{}); exit == 0 || !strings.Contains(stderr.String(), "was not found") {
		t.Fatalf("missing resume opened a new chat: exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestResumeCatalogIgnoresForeignAuthorityRuntime(t *testing.T) {
	ctx := t.Context()
	home, workspace := t.TempDir(), t.TempDir()
	cfg := cliCommandConfig(t, home, workspace)
	t.Chdir(workspace)
	var stdout, stderr bytes.Buffer
	run := func(deps dependencies, input string, args ...string) string {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if code := runWithDependencies(ctx, args, strings.NewReader(input), &stdout, &stderr, deps); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr.String())
		}
		return stdout.String()
	}
	if out := run(dependencies{}, "", "resume"); !strings.Contains(out, "No sessions") {
		t.Fatalf("empty catalog: %q", out)
	}
	if _, err := os.Stat(cfg.ATE.Directory); !os.IsNotExist(err) {
		t.Fatalf("listing created an ATE store: %v", err)
	}
	modelFixture := &chatTestModel{}
	deps := dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
		return modelFixture, nil
	}}
	const id = "saved-work"
	const prompt = "List package names"
	run(deps, prompt+"\n/exit\n", "chat", "--session", id, "--model", "test-model")
	path := filepath.Join(home, "authority-runtime-v3.db")
	oldRuntime, err := runtimestore.OpenRuntime(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldRuntime.ATERequests().BindAuthority(ctx, "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0"); err != nil {
		t.Fatal(err)
	}
	if err := oldRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"resume"}, {"resume", "--list"}} {
		if out := run(dependencies{}, "", args...); !strings.Contains(out, id) || !strings.Contains(out, prompt) {
			t.Fatalf("%v omitted %q: %q", args, id, out)
		}
	}
	if out := run(deps, "Summarize the names\n/exit\n", "resume", "--last"); !strings.Contains(out, "reply: Summarize the names") || modelFixture.lastInputCount != 4 {
		t.Fatalf("selected session did not reopen its history: %q (inputs=%d)", out, modelFixture.lastInputCount)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("resume modified the old authority database: %v", err)
	}
}

func TestResumeListIdentifiesActiveStoreOwner(t *testing.T) {
	cfg := cliCommandConfig(t, t.TempDir(), t.TempDir())
	active, err := app.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	list := func(ctx context.Context) error {
		return runResume(ctx, []string{"--list"}, strings.NewReader(""), io.Discard, io.Discard, dependencies{})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := list(ctx); !errors.Is(err, runtimeowner.ErrOwned) || !strings.Contains(err.Error(), "close the other instance") {
		t.Fatalf("active owner diagnosis = %v", err)
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	if err := list(t.Context()); err != nil {
		t.Fatalf("list after the owner exited: %v", err)
	}
}

func seedNonConversationEvents(t *testing.T, ctx context.Context, cfg config.Config) {
	t.Helper()
	db := openCLIApp(t, cfg)
	defer db.Close()
	if _, err := db.Events().Append(ctx, "doctor-fixture", 0, session.PendingEvent{Type: session.EventSessionCreated, Payload: session.MustJSONPayload(map[string]string{})}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Events().Append(ctx, "agent-thread_fixture", 0, session.PendingEvent{Type: session.EventUserMessage, Payload: session.MustJSONPayload(map[string]string{"content": "child fixture"})}); err != nil {
		t.Fatal(err)
	}
}
