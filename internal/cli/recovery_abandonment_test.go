package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
)

type unreadAbandonmentInput struct{ t *testing.T }

func (input unreadAbandonmentInput) Read([]byte) (int, error) {
	input.t.Fatal("offline abandonment read a new prompt")
	return 0, nil
}

func TestAbandonmentFlagsRequireExplicitTargetReasonAndConsent(t *testing.T) {
	defaults := config.Config{Model: config.ModelConfig{Timeout: 30}, Agent: config.AgentConfig{MaxIterations: 20, ContextWindowTokens: 32768, CompactionTriggerTokens: 24576}, Agents: config.AgentsConfig{MaxRunning: 4}}
	valid := []string{"--session", "root", "--abandon-turn", "target", "--abandon-reason", "reviewed", "--confirm-abandon"}
	options, err := parseChatOptions(valid, defaults, &bytes.Buffer{})
	if err != nil || options.abandonRequest == nil || options.abandonRequest.TurnID != "target" {
		t.Fatalf("options=%+v err=%v", options, err)
	}
	invalid := [][]string{
		valid[2:], valid[:len(valid)-1], valid[:4],
		{"--session", "root", "--confirm-abandon"},
		{"--session", "root", "--abandon-reason", "reviewed"},
	}
	for _, extra := range [][]string{{"--abandon-goal", "goal"}, {"--resume-turn", "target"}, {"--list-recoveries"}, {"--prompt-file", "-"}, {"--evaluation-mode", "--workspace", "/isolated"}} {
		invalid = append(invalid, append(append([]string{}, valid...), extra...))
	}
	for _, args := range invalid {
		if _, err := parseChatOptions(args, defaults, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid abandonment: %v", args)
		}
	}
}

func TestOfflineAbandonmentNeverStartsProviderSupervisorOrReadsPrompt(t *testing.T) {
	t.Setenv(config.HomeEnvironment, t.TempDir())
	for _, activeChild := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "dependent-child"}[activeChild], func(t *testing.T) {
			ctx := context.Background()
			directory := t.TempDir()
			path := filepath.Join(directory, "sessions.db")
			database, err := sqlitestore.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "root"})
			if err != nil {
				t.Fatal(err)
			}
			var child orchestration.AgentSnapshot
			if activeChild {
				child = createRecoveryChild(t, database, root)
			}
			if _, err := database.SessionEvents().Append(ctx, "root", session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "target", Payload: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			args := []string{"chat", "--data-dir", directory, "--workspace", t.TempDir(), "--session", "root", "--abandon-turn", "target", "--abandon-reason", "reviewed", "--confirm-abandon"}
			var stdout, stderr bytes.Buffer
			exit := runWithDependencies(ctx, args, unreadAbandonmentInput{t}, &stdout, &stderr, dependencies{})
			reopened, err := sqlitestore.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if activeChild {
				if exit == 0 || !strings.Contains(stderr.String(), "dependent work") {
					t.Fatalf("dependency ignored: exit=%d stderr=%s", exit, stderr.String())
				}
				unchanged, err := reopened.Orchestration().GetAgent(ctx, child.ID)
				if err != nil || unchanged.State != child.State || unchanged.Version != child.Version {
					t.Fatalf("offline command started child recovery: %+v %v", unchanged, err)
				}
				if err := session.CheckRecovery(ctx, reopened.SessionEvents(), "root", "target"); err != nil {
					t.Fatal("blocked request abandoned root", err)
				}
				return
			}
			var receipt session.Abandonment
			if exit != 0 || json.Unmarshal(stdout.Bytes(), &receipt) != nil || receipt.SessionID != "root" || len(receipt.TurnIDs) != 1 || receipt.EffectsReverted || !strings.Contains(stderr.String(), "NOT undone or deleted") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
			}
		})
	}
}
