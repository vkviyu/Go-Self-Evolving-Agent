package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/conversation"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/sqlitestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestRootConversationDeliversChildResultAndAcknowledgesAfterTurn(t *testing.T) {
	model := &rootMailboxTestModel{}
	rootChat, store, root, child := newRootMailboxConversation(t, model)
	enqueueRootResult(t, store, root, child, "child finished")

	if _, err := rootChat.RunTurn(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if !strings.Contains(model.lastInput, "child finished") {
		t.Fatalf("child result was not included in root input: %q", model.lastInput)
	}
	messages, err := store.ListMessages(context.Background(), orchestration.MessageQuery{
		TeamID: root.TeamID, RecipientAgentID: root.AgentID,
		States: []orchestration.DeliveryState{orchestration.DeliveryAcknowledged},
	})
	if err != nil || len(messages) != 1 {
		t.Fatalf("root result was not acknowledged: messages=%#v err=%v", messages, err)
	}
}

func TestRootConversationKeepsMailboxDeliveredWhenTurnFails(t *testing.T) {
	model := &rootMailboxTestModel{err: errors.New("model unavailable")}
	rootChat, store, root, child := newRootMailboxConversation(t, model)
	enqueueRootResult(t, store, root, child, "must retry")

	if _, err := rootChat.RunTurn(context.Background(), "continue", nil); err == nil {
		t.Fatal("expected root turn failure")
	}
	messages, err := store.ListMessages(context.Background(), orchestration.MessageQuery{
		TeamID: root.TeamID, RecipientAgentID: root.AgentID,
		States: []orchestration.DeliveryState{orchestration.DeliveryDelivered},
	})
	if err != nil || len(messages) != 1 {
		t.Fatalf("delivered mailbox message was lost after failed turn: messages=%#v err=%v", messages, err)
	}
}

func TestRootConversationWakeTurnConsumesChildResult(t *testing.T) {
	ctx := context.Background()
	model := &rootMailboxTestModel{}
	rootChat, store, root, child := newRootMailboxConversation(t, model)
	if _, err := rootChat.RunBackgroundTurn(ctx, nil); !errors.Is(err, agentruntime.ErrNoBackgroundInput) {
		t.Fatalf("empty wake = %v, want ErrNoBackgroundInput", err)
	}
	enqueueRootResult(t, store, root, child, "child finished")
	if pending, err := rootChat.PendingBackground(ctx); err != nil || pending != 1 {
		t.Fatalf("pending = %d err=%v", pending, err)
	}
	if _, err := rootChat.RunBackgroundTurn(ctx, nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	lastInput := model.lastInput
	model.mu.Unlock()
	if !strings.HasPrefix(lastInput, agentruntime.BackgroundInputHeader) || !strings.Contains(lastInput, "child finished") {
		t.Fatalf("wake turn input = %q", lastInput)
	}
	if pending, err := rootChat.PendingBackground(ctx); err != nil || pending != 0 {
		t.Fatalf("pending after wake = %d err=%v", pending, err)
	}
	if rootChat.CompletedTurns() != 0 {
		t.Fatalf("wake turn counted as a user turn: %d", rootChat.CompletedTurns())
	}
	history := rootChat.History()
	if len(history) != 2 || history[0].Role != schema.User || !strings.Contains(history[0].Content, "child finished") {
		t.Fatalf("wake turn missing from history: %#v", history)
	}
	if _, err := rootChat.RunTurn(ctx, "next", nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if model.lastInput != "next" {
		t.Fatalf("acknowledged message was injected again: %q", model.lastInput)
	}
}

func TestRootConversationSettlesTurnsCommittedBeforeCrash(t *testing.T) {
	ctx := context.Background()
	model := &rootMailboxTestModel{}
	fixture := newRootMailboxFixture(t, model)
	rootChat, store, root, child := fixture.chat, fixture.store, fixture.root, fixture.child
	result, err := rootChat.RunTurn(ctx, "first", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between the turn commit and its acknowledgement, and a
	// message held by a turn that never finished.
	enqueueRootResult(t, store, root, child, "committed")
	if _, err := store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: result.TurnID}); err != nil {
		t.Fatal(err)
	}
	enqueueRootResult(t, store, root, child, "unfinished")
	if _, err := store.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: root.TeamID, RecipientAgentID: root.AgentID, TurnID: "turn-unfinished"}); err != nil {
		t.Fatal(err)
	}
	supervisor, err := agentmanager.NewSupervisor(agentmanager.Config{Agents: store, Mailbox: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	reopened, err := openRootConversation(ctx, fixture.factory, supervisor, root)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := reopened.PendingBackground(ctx); err != nil || pending != 0 {
		t.Fatalf("settled restart exposed messages: pending=%d err=%v", pending, err)
	}
	acknowledged, err := store.ListMessages(ctx, orchestration.MessageQuery{TeamID: root.TeamID, RecipientAgentID: root.AgentID, States: []orchestration.DeliveryState{orchestration.DeliveryAcknowledged}})
	if err != nil || len(acknowledged) != 1 || !strings.Contains(string(acknowledged[0].Body), "committed") {
		t.Fatalf("committed turn was not acknowledged: %#v err=%v", acknowledged, err)
	}
	if _, err := reopened.RunTurn(ctx, "second", nil); err != nil {
		t.Fatal(err)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if !strings.Contains(model.lastInput, "unfinished") || strings.Contains(model.lastInput, "committed") {
		t.Fatalf("new turn input = %q, want only the released message", model.lastInput)
	}
}

func newRootMailboxConversation(t *testing.T, chatModel model.ToolCallingChatModel) (*conversation.Conversation, *sqlitestore.OrchestrationStore, orchestration.RootThread, orchestration.AgentSnapshot) {
	t.Helper()
	fixture := newRootMailboxFixture(t, chatModel)
	return fixture.chat, fixture.store, fixture.root, fixture.child
}

// rootMailboxFixture keeps the base factory so a test can reopen the root
// conversation as a restarted process would.
type rootMailboxFixture struct {
	chat    *conversation.Conversation
	store   *sqlitestore.OrchestrationStore
	root    orchestration.RootThread
	child   orchestration.AgentSnapshot
	factory *agentruntime.Factory
}

func newRootMailboxFixture(t *testing.T, chatModel model.ToolCallingChatModel) rootMailboxFixture {
	t.Helper()
	database, err := sqlitestore.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store := database.Orchestration()
	root, _, err := store.EnsureRootThread(context.Background(), orchestration.RootThreadSeed{SessionID: "root-mailbox-test"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.CreateChild(context.Background(), orchestration.SpawnSpec{
		TeamID: root.TeamID, ParentAgentID: root.AgentID, ProfileRevision: "worker-v1", InitialPrompt: "child",
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := agentmanager.NewSupervisor(agentmanager.Config{Agents: store, Mailbox: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	registry := tool.NewRegistry()
	policyEngine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: policyEngine, Events: database.SessionEvents()})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := agentruntime.NewFactory(agentruntime.Config{ActorScope: root.ActorScope()}, chatModel, registry, pipeline, database.SessionEvents(), database.Checkpoints())
	if err != nil {
		t.Fatal(err)
	}
	rootChat, err := openRootConversation(context.Background(), factory, supervisor, root)
	if err != nil {
		t.Fatal(err)
	}
	return rootMailboxFixture{chat: rootChat, store: store, root: root, child: child, factory: factory}
}

func enqueueRootResult(t *testing.T, store *sqlitestore.OrchestrationStore, root orchestration.RootThread, child orchestration.AgentSnapshot, summary string) {
	t.Helper()
	body, err := json.Marshal(orchestration.AgentResult{Status: orchestration.ResultSucceeded, Summary: summary})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), orchestration.MessageSpec{
		TeamID: root.TeamID, SenderAgentID: child.ID, RecipientAgentID: root.AgentID,
		Type: orchestration.MessageResult, Body: body,
	}); err != nil {
		t.Fatal(err)
	}
}

type rootMailboxTestModel struct {
	mu        sync.Mutex
	lastInput string
	err       error
}

var _ model.ToolCallingChatModel = (*rootMailboxTestModel)(nil)

func (m *rootMailboxTestModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	if len(messages) > 0 {
		m.lastInput = messages[len(messages)-1].Content
	}
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return schema.AssistantMessage("root reply", nil), nil
}

func (m *rootMailboxTestModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (m *rootMailboxTestModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}
