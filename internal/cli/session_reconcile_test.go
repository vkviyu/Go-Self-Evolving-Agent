package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestSessionReconcileCLIUnregisteredWithoutExecution(t *testing.T) {
	ctx := t.Context()
	cfg := cliCommandConfig(t, t.TempDir(), t.TempDir())
	store := openCLIApp(t, cfg)
	root, _, err := store.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "interactive-chat"})
	if err != nil {
		t.Fatal(err)
	}
	child := createRecoveryChild(t, store, root)
	_, err = store.Events().Append(ctx, root.SessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventUserMessage, Payload: session.MustJSONPayload(map[string]any{"text": "SECRET-PROMPT"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GSEA_MODEL_PROVIDER", "invalid-provider")
	var stdout, stderr bytes.Buffer
	exit := runWithDependencies(ctx, []string{"session", "reconcile", string(root.SessionID),
		"--data-dir", cfg.DataDirectory, "--workspace", cfg.WorkspaceDirectory},
		unreadAbandonmentInput{t}, &stdout, &stderr, dependencies{})
	if exit != 0 || stderr.Len() != 0 {
		t.Fatalf("reconcile: exit=%d stderr=%s", exit, stderr.String())
	}
	var metadata map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &metadata); err != nil ||
		len(metadata) != 2 || metadata["session_id"] != string(root.SessionID) || metadata["outcome"] != "reconciled" {
		t.Fatalf("unexpected safe metadata: %s (%v)", stdout.String(), err)
	}
	reopened := openCLIApp(t, cfg)
	unchanged, err := reopened.Orchestration().GetAgent(ctx, child.ID)
	if err != nil || unchanged.State != orchestration.AgentRunning || unchanged.Version != child.Version {
		t.Fatalf("reconcile started child recovery: %+v %v", unchanged, err)
	}
	if _, err := os.Stat(cfg.SessionRegistryPath()); !os.IsNotExist(err) {
		t.Fatalf("reconcile required or created registration: %v", err)
	}
	entries, err := os.ReadDir(cfg.WorkspaceDirectory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("reconcile activated workspace services: %v", err)
	}
}

func TestSessionReconcileCLIProfileAndOverrides(t *testing.T) {
	home, data, workspace := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv(config.HomeEnvironment, home)
	cliATEEnvironment(t)
	t.Setenv("GSEA_DATA_DIRECTORY", "")
	t.Setenv("GSEA_WORKSPACE_DIRECTORY", "")
	profile := fmt.Sprintf("data_directory = %q\nworkspace_directory = %q\n[ate]\nenabled = false\ndirectory = %q\n",
		data, workspace, filepath.Join(home, "profile-authority"))
	if err := os.WriteFile(filepath.Join(home, "repair.config.toml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	// The explicit profile must bypass a broken default environment profile.
	t.Setenv(config.ProfileEnvironment, "missing-profile")
	t.Setenv("GSEA_ATE_ENABLED", "")
	for _, args := range [][]string{
		{"chat-id", "--profile", "repair"},
		{"-p", "repair", "chat-id"},
		{"chat-id", "--profile=repair"},
	} {
		cfg, id, err := parseSessionReconcileOptions(args, io.Discard)
		if err != nil || id != "chat-id" || cfg.DataDirectory != data ||
			cfg.WorkspaceDirectory != workspace || cfg.ATE.Enabled ||
			cfg.ATE.Directory != filepath.Join(home, "profile-authority") {
			t.Fatalf("profile not respected: id=%s err=%v", id, err)
		}
	}
	override := t.TempDir()
	cfg, _, err := parseSessionReconcileOptions([]string{"chat-id", "-p", "repair", "--data-dir", override, "--workspace", override}, io.Discard)
	if err != nil || cfg.DataDirectory != override || cfg.WorkspaceDirectory != override || cfg.ATE.Enabled {
		t.Fatalf("overrides not applied: %v", err)
	}
	// Exercise dispatch, not only parsing. The profile selects the seeded store.
	t.Setenv("GSEA_ATE_ENABLED", "true")
	cfg, _, err = parseSessionReconcileOptions([]string{"chat-id", "-p", "repair"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	store := openCLIApp(t, cfg)
	if _, _, err := store.Orchestration().EnsureRootThread(t.Context(), orchestration.RootThreadSeed{SessionID: "chat-id"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := runSession(t.Context(), []string{"reconcile", "chat-id", "-p", "repair"}, &stdout, &stderr); err != nil {
		t.Fatalf("dispatch loaded default profile before reconcile flags: %v", err)
	}
}

func TestSessionReconcileCLIRejectsInputBeforeAuthority(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnvironment, home)
	cliATEEnvironment(t)
	t.Setenv("GSEA_ATE_BINARY", filepath.Join(home, "must-not-start"))
	t.Setenv("GSEA_DATA_DIRECTORY", "")
	t.Setenv("GSEA_WORKSPACE_DIRECTORY", "")
	for _, args := range [][]string{
		{}, {"../bad"}, {strings.Repeat("x", 129)}, {"one", "two"}, {"one", "--workspace="},
		{"one", "--data-dir="}, {"one", "--profile="}, {"one", "--workspace", "\x00"},
		{"one", "--workspace", filepath.Join(home, "absent")}, {"one", "--data-dir", filepath.Join(home, "absent")},
		{"one", "--force"}, {"--data-dir"}, {"--profile", "../bad", "one"},
		{"one", "--data-dir", home},
	} {
		var stdout, stderr bytes.Buffer
		if err := runSession(t.Context(), append([]string{"reconcile"}, args...), &stdout, &stderr); err == nil {
			t.Fatalf("accepted invalid args: %q", args)
		}
		if stdout.Len() != 0 {
			t.Fatal("printed success for invalid args")
		}
		entries, err := os.ReadDir(home)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid args opened storage: %q (%v)", args, err)
		}
	}
	// Help must also avoid config loading, including a missing selected profile.
	t.Setenv(config.ProfileEnvironment, "missing")
	var stdout, stderr bytes.Buffer
	exit := Run(context.Background(), []string{"session", "reconcile", "--help"}, nil, &stdout, &stderr)
	if exit != 0 || !strings.Contains(stderr.String(), "--data-dir") {
		t.Fatalf("help: exit=%d stderr=%s", exit, stderr.String())
	}
}

func TestSessionReconcileCLIOwnedAndSafeErrors(t *testing.T) {
	cfg := cliCommandConfig(t, t.TempDir(), t.TempDir())
	store := openCLIApp(t, cfg)
	root, _, err := store.Orchestration().EnsureRootThread(t.Context(), orchestration.RootThreadSeed{SessionID: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	holder, err := app.OpenControl(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := holder.AttachExistingTeam(t.Context(), root.TeamID); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runSession(t.Context(), []string{"reconcile", "owned", "--workspace", cfg.WorkspaceDirectory}, &stdout, &stderr)
	if !errors.Is(err, runtimeowner.ErrOwned) || stdout.Len() != 0 || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("ownership result: %v", err)
	}
	private := errors.New("SECRET-PROMPT SECRET-TOKEN SECRET-KEY")
	safe := sessionReconcileError{message: "reconciliation failed", cause: private}
	if strings.Contains(safe.Error(), "SECRET") || !errors.Is(safe, private) {
		t.Fatal("safe error exposed private evidence or lost its cause")
	}
	// Config parse diagnostics may quote a line from the file; suppress it.
	if err := os.WriteFile(filepath.Join(cfg.HomeDirectory, "config.toml"), []byte("SECRET-TOKEN = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	exit := Run(t.Context(), []string{"session", "reconcile", "owned"}, nil, &stdout, &stderr)
	if exit == 0 || strings.Contains(stderr.String(), "SECRET") {
		t.Fatalf("config failure leaked contents: %s", stderr.String())
	}
}
