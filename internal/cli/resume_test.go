package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/sessionregistry"
)

func TestResumeDiscoversAndAdoptsNativeHistory(t *testing.T) {
	ctx := context.Background()
	cliATEEnvironment(t)
	home := os.Getenv(config.HomeEnvironment)
	workspace := t.TempDir()
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
	registry, err := app.OpenSessionRegistry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	entries, err := registry.List(ctx)
	if err != nil || len(entries) != 1 || entries[0].State != sessionregistry.StateStopped || entries[0].DataDirectory != canonicalDirectory(home) {
		t.Fatalf("automatic session registration = %+v, %v", entries, err)
	}
	item := entries[0]
	if got := run("", dependencies{}, "resume", "--list"); !strings.Contains(got, "first question") || !strings.Contains(got, item.ID) {
		t.Fatalf("catalog listing = %q", got)
	}
	if err := registry.Remove(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	seedNonConversationEvents(t, ctx, cfg)
	if got := run("", dependencies{}, "resume", "--list"); strings.Contains(got, item.ID) {
		t.Fatalf("uncataloged native history without workspace metadata leaked into workspace scope: %q", got)
	}
	if got := run("", dependencies{}, "resume", "--list", "--all"); !strings.Contains(got, item.ID) || !strings.Contains(got, "first question") || strings.Contains(got, "agent-thread_") || strings.Contains(got, "doctor-") {
		t.Fatalf("native history discovery = %q", got)
	}
	if got := run("follow up\n/exit\n", deps, "resume", item.ID, "--model", "test-model"); got != "reply: follow up\n" || fake.lastInputCount != 4 {
		t.Fatalf("native history was not loaded: output=%q model input=%d", got, fake.lastInputCount)
	}
	registered, err := registry.Get(ctx, item.ID)
	if err != nil || registered.Workspace != canonicalDirectory(workspace) || registered.DataDirectory != canonicalDirectory(home) {
		t.Fatalf("native history lost its workspace or runtime directory: %+v %v", registered, err)
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runWithDependencies(ctx, []string{"resume", "missing"}, strings.NewReader(""), &stdout, &stderr, dependencies{}); exit == 0 || !strings.Contains(stderr.String(), "was not found") {
		t.Fatalf("missing resume opened a new chat: exit=%d stderr=%q", exit, stderr.String())
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
