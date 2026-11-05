package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
)

// fakeBackgroundMailbox hands out its queued messages once per turn.
type fakeBackgroundMailbox struct {
	queued []orchestration.Message
	bound  map[string][]orchestration.Message
	claims int
}

func (mailbox *fakeBackgroundMailbox) Claim(_ context.Context, turnID string, _ uint64) ([]orchestration.Message, error) {
	mailbox.claims++
	if mailbox.bound == nil {
		mailbox.bound = make(map[string][]orchestration.Message)
	}
	mailbox.bound[turnID] = append(mailbox.bound[turnID], mailbox.queued...)
	mailbox.queued = nil
	return append([]orchestration.Message(nil), mailbox.bound[turnID]...), nil
}

func (mailbox *fakeBackgroundMailbox) Acknowledge(context.Context, string) error { return nil }
func (mailbox *fakeBackgroundMailbox) Release(context.Context, string) error     { return nil }
func (mailbox *fakeBackgroundMailbox) BoundTurns(context.Context) ([]string, error) {
	return nil, nil
}
func (mailbox *fakeBackgroundMailbox) Available(context.Context) (int, error) {
	return len(mailbox.queued), nil
}

func backgroundMessage(id string) orchestration.Message {
	return orchestration.Message{ID: orchestration.MessageID(id), Type: orchestration.MessageResult, SenderAgentID: "agent-child", Body: json.RawMessage(`{"id":"` + id + `"}`)}
}

func TestBackgroundInputIsSplicedAtRecordedPositions(t *testing.T) {
	ctx := context.Background()
	store := memorystore.New()
	mailbox := &fakeBackgroundMailbox{queued: []orchestration.Message{backgroundMessage("m1")}}
	model := &recordingModel{factory: &Factory{mailbox: mailbox}, events: store, sessionID: "background-session", turnID: "turn-1"}
	first := []*schema.Message{schema.UserMessage("hello")}
	prepared, err := model.prepareBackgroundInput(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 || prepared[1].Role != schema.User || prepared[1].Content != NewBackgroundInput([]orchestration.Message{backgroundMessage("m1")}).Content {
		t.Fatalf("first request lacks the background block: %#v", prepared)
	}
	if len(first) != 1 {
		t.Fatal("raw input was modified")
	}
	mailbox.queued = []orchestration.Message{backgroundMessage("m2")}
	second := []*schema.Message{schema.UserMessage("hello"), schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1"}}), schema.ToolMessage("{}", "call-1")}
	prepared, err = model.prepareBackgroundInput(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	roles := make([]schema.RoleType, len(prepared))
	for index, message := range prepared {
		roles[index] = message.Role
	}
	want := []schema.RoleType{schema.User, schema.User, schema.Assistant, schema.Tool, schema.User}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	for index := range want {
		if roles[index] != want[index] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	// A resumed turn restores both blocks from its events without claiming
	// them as new input.
	resumed := &recordingModel{factory: &Factory{mailbox: mailbox}, events: store, sessionID: "background-session", turnID: "turn-1"}
	replayed, err := resumed.prepareBackgroundInput(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(prepared) {
		t.Fatalf("resumed request has %d messages, want %d", len(replayed), len(prepared))
	}
	events, err := store.Load(ctx, "background-session", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, event := range events {
		if event.Type == session.EventBackgroundInput {
			recorded++
		}
	}
	if recorded != 2 {
		t.Fatalf("recorded %d background blocks, want 2", recorded)
	}
}

func TestAuxiliaryRequestsReceiveNoBackgroundInput(t *testing.T) {
	mailbox := &fakeBackgroundMailbox{queued: []orchestration.Message{backgroundMessage("m1")}}
	model := &recordingModel{factory: &Factory{mailbox: mailbox}, events: memorystore.New(), sessionID: "aux-session", turnID: "turn-1", purpose: "compaction"}
	input := []*schema.Message{schema.UserMessage("summarize")}
	prepared, err := model.prepareBackgroundInput(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || mailbox.claims != 0 {
		t.Fatalf("auxiliary request received Agent messages: %#v claims=%d", prepared, mailbox.claims)
	}
}

func TestChildFactoryDropsMailbox(t *testing.T) {
	teamID, _ := orchestration.NewTeamID()
	agentID, _ := orchestration.NewAgentID()
	threadID, _ := orchestration.NewThreadID()
	root := (&Factory{}).WithMailbox(&fakeBackgroundMailbox{})
	child, err := root.WithActorScope(orchestration.ActorScope{TeamID: teamID, AgentID: agentID, ThreadID: threadID})
	if err != nil {
		t.Fatal(err)
	}
	if child.mailbox != nil || root.mailbox == nil {
		t.Fatal("child factory kept the root mailbox")
	}
}
