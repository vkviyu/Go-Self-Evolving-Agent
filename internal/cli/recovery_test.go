package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type headlessRecoveryFake struct {
	runs, resumes, inspections int
	resumedID                  string
}

func (*headlessRecoveryFake) ID() session.ID      { return "recovery-session" }
func (*headlessRecoveryFake) CompletedTurns() int { return 0 }
func (fake *headlessRecoveryFake) RunTurn(context.Context, string, agentruntime.EventSink) (agentruntime.Result, error) {
	fake.runs++
	return agentruntime.Result{}, nil
}
func (fake *headlessRecoveryFake) Recoveries(context.Context) ([]conversation.Recovery, error) {
	fake.inspections++
	return []conversation.Recovery{{TurnID: "original", Phase: "assessment"}}, nil
}
func (fake *headlessRecoveryFake) ResumeTurn(_ context.Context, id string, _ agentruntime.EventSink) (agentruntime.Result, error) {
	fake.resumes++
	fake.resumedID = id
	return agentruntime.Result{TurnID: id, Final: schema.AssistantMessage("recovered result", nil)}, nil
}

func TestHeadlessResumeIsAnOperationNotANewPrompt(t *testing.T) {
	for _, format := range []string{headlessFormatStream, headlessFormatJSON} {
		t.Run(format, func(t *testing.T) {
			fake := &headlessRecoveryFake{}
			var stdout, stderr bytes.Buffer
			err := runHeadlessChat(context.Background(), strings.NewReader("/recoveries\n/resume original\n/exit\n"), &stdout, &stderr, fake, chatOptions{headlessFormat: format})
			if err != nil || fake.inspections != 1 || fake.runs != 0 || fake.resumes != 1 || fake.resumedID != "original" {
				t.Fatalf("dispatch=%#v err=%v", fake, err)
			}
			if !strings.Contains(stdout.String(), "assessment") || !strings.Contains(stdout.String(), "recovered result") {
				t.Fatalf("output=%s", stdout.String())
			}
		})
	}
}

func TestRecoveryFlagsRequireSelectionAndRejectNewWork(t *testing.T) {
	defaults := config.Config{Model: config.ModelConfig{Timeout: 30}, Agent: config.AgentConfig{MaxIterations: 20, ContextWindowTokens: 32768, CompactionTriggerTokens: 24576}, Agents: config.AgentsConfig{MaxRunning: 4}}
	for _, arguments := range [][]string{
		{"--resume-turn", "turn"}, {"--list-recoveries"},
		{"--session", "s", "--resume-turn", "turn", "--list-recoveries"},
		{"--session", "s", "--resume-turn", "turn", "--prompt-file", "-"},
		{"--session", "s", "--resume-turn", "turn", "--evaluation-mode", "--workspace", "/isolated"},
	} {
		if _, err := parseChatOptions(arguments, defaults, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted conflicting recovery flags=%v", arguments)
		}
	}
	options, err := parseChatOptions([]string{"--session", "s", "--resume-turn", "turn"}, defaults, &bytes.Buffer{})
	if err != nil || options.resumeTurnID != "turn" || options.sessionID != "s" {
		t.Fatalf("options=%#v %v", options, err)
	}
}

func TestListRecoveriesDoesNotStartSupervisorOrProvider(t *testing.T) {
	cliATEEnvironment(t)
	ctx := context.Background()
	directory := t.TempDir()
	cfg := cliCommandConfig(t, directory, t.TempDir())
	database := openCLIApp(t, cfg)
	root, _, err := database.Orchestration().EnsureRootThread(ctx, orchestration.RootThreadSeed{SessionID: "list-session"})
	if err != nil {
		t.Fatal(err)
	}
	child := createRecoveryChild(t, database, root)
	_, err = database.Events().Append(ctx, root.SessionID, session.AnySequence, session.PendingEvent{Type: session.EventTurnStarted, CorrelationID: "interrupted", Payload: session.MustJSONPayload(map[string]any{})})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exit := runWithDependencies(ctx, []string{"chat", "--data-dir", directory, "--workspace", cfg.WorkspaceDirectory, "--session", "list-session", "--list-recoveries"}, unreadAbandonmentInput{t}, &stdout, &stderr, dependencies{})
	if exit != 0 {
		t.Fatalf("list executed model: exit=%d err=%s", exit, stderr.String())
	}
	var items []conversation.Recovery
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil || len(items) != 1 || items[0].TurnID != "interrupted" || items[0].Phase != "unavailable" {
		t.Fatalf("output=%s err=%v", stdout.String(), err)
	}
	reopened := openCLIApp(t, cfg)
	unchanged, err := reopened.Orchestration().GetAgent(ctx, child.ID)
	if err != nil || unchanged.State != orchestration.AgentRunning || unchanged.Version != child.Version {
		t.Fatalf("listing recovered child: %#v %v", unchanged, err)
	}
}

func TestRootResumeDoesNotConsumeNewMailboxMessages(t *testing.T) {
	fake := &rootMailboxTestModel{}
	chat, store, root, child := newRootMailboxConversation(t, fake)
	enqueueRootResult(t, store, root, child, "not part of saved context")
	if _, err := chat.ResumeTurn(context.Background(), "missing-checkpoint", nil); err == nil {
		t.Fatal("missing checkpoint accepted")
	}
	queued, err := store.ListMessages(context.Background(), orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: root.AgentID, States: []orchestration.DeliveryState{orchestration.DeliveryEnqueued}})
	if err != nil || len(queued) != 1 {
		t.Fatalf("mailbox consumed=%#v %v", queued, err)
	}
	if fake.lastInput != "" {
		t.Fatal("resume injected a new prompt")
	}
}
