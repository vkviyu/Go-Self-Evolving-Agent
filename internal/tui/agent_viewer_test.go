package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestAgentViewerRejectsStaleResultsAndRestoresRootReading(t *testing.T) {
	model := navigationFixture(t)
	model.transcripts = fakeTranscriptReader{sessions: map[session.ID][]*schema.Message{
		"session-a": {schema.UserMessage("A's question")},
		"session-b": {schema.UserMessage("B's question")},
	}}
	a := orchestration.AgentSnapshot{ID: "a", ParentAgentID: "root", SessionID: "session-a"}
	b := orchestration.AgentSnapshot{ID: "b", ParentAgentID: "root", SessionID: "session-b"}
	model.autoScroll = false
	model.viewport.SetYOffset(10)
	rootPosition := model.readingPosition()
	model.input.SetValue("root draft")
	model.busy = true

	staleA := model.openAgentTranscript(a)()
	staleB := model.openAgentTranscript(b)()
	currentA := model.openAgentTranscript(a)()
	if model.viewer.agentID != a.ID || !strings.Contains(ansi.Strip(model.transcriptContent), "Loading Agent transcript") {
		t.Fatal("opening a child did not switch immediately")
	}
	model.Update(staleA)
	model.Update(staleB)
	if plain := ansi.Strip(model.transcriptContent); strings.Contains(plain, "question") {
		t.Fatalf("stale response reached the transcript: %q", plain)
	}
	model.Update(currentA)
	if plain := ansi.Strip(model.transcriptContent); !strings.Contains(plain, "A's question") {
		t.Fatalf("current response was not shown: %q", plain)
	}
	model.Update(runtimeEventMsg{event: agentruntime.Event{Type: agentruntime.EventAssistantTextDelta, Delta: "root kept running"}})
	if plain := ansi.Strip(model.transcriptContent); strings.Contains(plain, "root kept running") {
		t.Fatalf("root event leaked into child transcript: %q", plain)
	}
	lateB := model.openAgentTranscript(b)()
	model.openAgentTranscript(orchestration.AgentSnapshot{ID: "root"})
	model.Update(lateB)
	if model.viewer != nil || model.autoScroll || model.readingPosition() != rootPosition || model.input.Value() != "root draft" || !model.busy {
		t.Fatal("returning from the viewer did not restore the root display")
	}
	if plain := ansi.Strip(model.transcriptContent); !strings.Contains(plain, "root kept running") {
		t.Fatal("root execution progress was lost")
	}
}

type versionedViewerReader struct {
	revision uint64
	loads    int
	messages []*schema.Message
}

func (reader *versionedViewerReader) TranscriptRevision(context.Context, session.ID) (uint64, error) {
	return reader.revision, nil
}

func (reader *versionedViewerReader) LoadTranscript(context.Context, session.ID) ([]*schema.Message, error) {
	reader.loads++
	return reader.messages, nil
}

func TestAgentViewerFollowsOnlyChangedSelectedTranscript(t *testing.T) {
	model := newTestModel(t)
	model.resize(80, 12)
	reader := &versionedViewerReader{revision: 1, messages: []*schema.Message{
		schema.UserMessage("child question"), schema.AssistantMessage(strings.Repeat("initial answer\n", 30), nil),
	}}
	model.transcripts = reader
	child := orchestration.AgentSnapshot{ID: "child", ParentAgentID: "root", SessionID: "session-child", State: orchestration.AgentRunning}
	model.activity.Agents = []orchestration.AgentSnapshot{child}
	initial := model.openAgentTranscript(child)
	if duplicate := model.refreshAgentTranscript(); duplicate != nil {
		t.Fatal("a poll started before the initial read returned")
	}
	runAction(t, model, initial)
	runAction(t, model, model.refreshAgentTranscript())
	if reader.loads != 1 {
		t.Fatalf("unchanged revision triggered %d transcript reads", reader.loads)
	}

	model.autoScroll = false
	model.viewport.SetYOffset(3) // Separator before the multiline reply.
	position := model.readingPosition()
	reader.revision++
	reader.messages = append(reader.messages, schema.AssistantMessage("new child progress", nil))
	runAction(t, model, model.refreshAgentTranscript())
	if model.autoScroll || model.readingPosition() != position || !strings.Contains(ansi.Strip(model.transcriptContent), "new child progress") {
		t.Fatalf("live refresh moved the reader or missed progress: follow=%v before=%+v after=%+v content=%q", model.autoScroll, position, model.readingPosition(), ansi.Strip(model.transcriptContent))
	}

	model.activity.Agents[0].State = orchestration.AgentCompleted
	model.activity.Agents[0].Version++
	reader.revision++
	reader.messages = append(reader.messages, schema.AssistantMessage("final child response", nil))
	runAction(t, model, model.refreshAgentTranscript())
	runAction(t, model, model.refreshAgentTranscript())
	if reader.loads != 3 || !strings.Contains(ansi.Strip(model.transcriptContent), "final child response") {
		t.Fatal("terminal transition did not settle the transcript")
	}
	reader.revision++
	reader.messages = append(reader.messages, schema.AssistantMessage("late durable result", nil))
	runAction(t, model, model.refreshAgentTranscript())
	if reader.loads != 4 || !strings.Contains(ansi.Strip(model.transcriptContent), "late durable result") {
		t.Fatal("terminal revision check missed a late transcript event")
	}
}

func TestAgentViewerReturnsToRootForApproval(t *testing.T) {
	model, _ := newPanelModel(t)
	child := model.activity.Agents[1]
	stale := model.openAgentTranscript(child)()
	model.Update(approvalRequestMsg{request: approval.Request{
		ID: "approval", Policy: policy.Request{ToolName: "workspace_write"},
	}})
	model.Update(stale)
	if model.openAgentTranscript(child) != nil || model.viewer != nil || model.approvalPending == nil {
		t.Fatal("switching the transcript hid a pending approval")
	}
	if plain := ansi.Strip(model.View().Content); !strings.Contains(plain, "workspace_write") || !strings.Contains(plain, "approval required") {
		t.Fatalf("pending approval is not visible: %q", plain)
	}
}
