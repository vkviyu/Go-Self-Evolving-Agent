package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	atemodel "github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Only the provider is fake: Open retains its experience service, and Eino
// dispatches the model's structured choice through the production policy,
// registry, memory service and native ATE process.
type memoryRoutingModel struct {
	mu          sync.Mutex
	calls       int
	prompt      string
	toolName    string
	arguments   string
	answer      string
	observation string
}

func (m *memoryRoutingModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.calls++
	offered := map[string]bool{}
	for _, info := range model.GetCommonOptions(nil, options...).Tools {
		offered[info.Name] = true
	}
	if !offered["memory_query"] || !offered["memory_manage"] {
		return nil, errors.New("primary model was not offered both memory tools")
	}
	instruction, user, observations := false, false, 0
	for _, msg := range input {
		if msg.Role == schema.System && strings.Contains(msg.Content, experience.MemoryInstruction) {
			instruction = true
		}
		if msg.Role == schema.System && strings.Contains(msg.Content, "\nStage: ") {
			return nil, errors.New("unexpected auxiliary Experience model call")
		}
		if msg.Role == schema.User && msg.Content == m.prompt {
			user = true
		}
		if msg.Role == schema.Tool {
			observations++
			if msg.ToolCallID != "memory-choice" {
				return nil, errors.New("unexpected tool observation")
			}
			m.observation = msg.Content
		}
	}
	if !instruction || !user {
		return nil, errors.New("primary input lacks host memory instruction or original user message")
	}
	switch {
	case m.calls == 1 && observations == 0:
		if m.toolName == "" {
			return schema.AssistantMessage(m.answer, nil), nil
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "memory-choice", Type: "function",
			Function: schema.FunctionCall{Name: m.toolName, Arguments: m.arguments},
		}}), nil
	case m.calls == 2 && m.toolName != "" && observations == 1:
		if !json.Valid([]byte(m.observation)) {
			return nil, errors.New("memory tool did not return structured JSON")
		}
		return schema.AssistantMessage(m.answer, nil), nil
	default:
		return nil, errors.New("unexpected model call or tool observation count")
	}
}

func (m *memoryRoutingModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func memoryRoutingFactory(t *testing.T, a *App, chat model.BaseChatModel, id session.ID, approvers ...policy.Approver) *agentruntime.Factory {
	t.Helper()
	root, _, err := a.EnsureRootThread(t.Context(), id)
	temporalRequire(t, "ensure memory routing root", err)
	f, err := a.NewAgentFactory(chat, agentruntime.Config{
		ActorScope: root.ActorScope(), Instruction: "Answer the user's request.",
		MaxIterations: 4, MaxOutputTokens: 1024, UsageLedger: a.Orchestration(),
	}, approvers...)
	temporalRequire(t, "construct default memory routing factory", err)
	return f
}

func memoryRoutingRun(t *testing.T, a *App, id session.ID, fake *memoryRoutingModel, approvers ...policy.Approver) (agentruntime.Result, *agentruntime.Factory) {
	t.Helper()
	f := memoryRoutingFactory(t, a, fake, id, approvers...)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	turn, err := f.NewTurn(ctx, id)
	temporalRequire(t, "create memory routing turn", err)
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage(fake.prompt)}, nil)
	temporalRequire(t, "run memory routing through Eino", err)
	if result.Final == nil || result.Final.Content != fake.answer || result.ExperienceMaintenance != nil {
		t.Fatalf("default turn must return its primary answer without maintenance: %+v", result)
	}
	wantCalls := 1
	if fake.toolName != "" {
		wantCalls = 2
	}
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()
	if calls != wantCalls {
		t.Fatalf("model calls = %d, want %d primary calls only", calls, wantCalls)
	}
	return result, f
}

// Check both primary request purpose and the durable response receipts. Merely
// counting fake calls would miss maintenance records or injected experience.
func memoryRoutingEvents(t *testing.T, a *App, id session.ID, result agentruntime.Result, fake *memoryRoutingModel) []session.Event {
	t.Helper()
	var events []session.Event
	var flow []session.EventType
	err := session.VisitEvents(t.Context(), a.events, id, func(e session.Event) error {
		events = append(events, e)
		if e.Type == agentruntime.EventExperienceMaintenance || strings.HasPrefix(string(e.Type), "experience.") {
			return fmt.Errorf("default turn recorded automatic experience work: %s", e.Type)
		}
		switch e.Type {
		case session.EventUserMessage, session.EventModelRequested, session.EventAssistantMessage,
			session.EventToolProposed, session.EventToolStarted, session.EventToolSucceeded, session.EventTurnCompleted:
			flow = append(flow, e.Type)
		case session.EventModelFailed, session.EventToolFailed:
			return fmt.Errorf("unexpected durable failure: %s", e.Type)
		}
		switch e.Type {
		case session.EventModelRequested, session.EventAssistantMessage:
			var p struct {
				Purpose string `json:"purpose"`
				Receipt *struct {
					Input      []*schema.Message `json:"input"`
					Experience json.RawMessage   `json:"experience"`
				} `json:"receipt"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if p.Purpose != "" || e.CorrelationID != result.TurnID {
				return errors.New("non-primary or incorrectly correlated model event")
			}
			if e.Type == session.EventAssistantMessage &&
				(p.Receipt == nil || len(p.Receipt.Input) == 0 || len(p.Receipt.Experience) != 0) {
				return errors.New("primary receipt missing or contains automatic experience")
			}
		case session.EventToolSucceeded:
			var p struct {
				TurnID string          `json:"turn_id"`
				Tool   string          `json:"tool"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if e.CorrelationID != "memory-choice" || p.TurnID != result.TurnID || p.Tool != fake.toolName {
				return errors.New("tool result lost model call or turn identity")
			}
			var durable, observed any
			if err := json.Unmarshal(p.Result, &durable); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(fake.observation), &observed); err != nil {
				return err
			}
			if !reflect.DeepEqual(durable, observed) {
				return errors.New("second model did not receive the durable tool result")
			}
		}
		return nil
	})
	temporalRequire(t, "inspect native memory routing events", err)
	want := []session.EventType{session.EventUserMessage, session.EventModelRequested, session.EventAssistantMessage}
	if fake.toolName != "" {
		want = append(want, session.EventToolProposed, session.EventToolStarted, session.EventToolSucceeded,
			session.EventModelRequested, session.EventAssistantMessage)
	}
	want = append(want, session.EventTurnCompleted)
	if !reflect.DeepEqual(flow, want) {
		t.Fatalf("durable primary/tool flow = %v, want %v", flow, want)
	}
	return events
}

func TestMemoryRoutingPrimaryCountAndDirectResponse(t *testing.T) {
	binary := temporalBinary(t)
	for _, tc := range []struct {
		name, prompt, toolName, arguments, answer string
		seeded                                    bool
	}{
		{"count", "How many reusable memories are stored?", "memory_query", `{"operation":"count"}`, "There are 0 reusable memories.", false},
		{"count-paraphrase", "Give me the size of the reusable experience collection.", "memory_query", `{"operation":"count"}`, "There are 0 reusable memories.", false},
		{"count-populated", "How many reusable memories are stored?", "memory_query", `{"operation":"count"}`, "There is 1 reusable memory.", true},
		{"direct", "Reply with a brief greeting.", "", "", "Hello.", false},
		{"direct-populated", "Reply with a brief greeting.", "", "", "Hello.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := temporalConfig(t, config.Config{}, binary, t.TempDir())
			a := temporalOpen(t, cfg)
			wantCount := 0
			if tc.seeded {
				// The existing explicit opt-in legacy fixture supplies a
				// genuinely promoted memory in a separate session. The turn
				// under test still uses the default factory and strict model.
				temporalRequire(t, "write memory seed fixture", os.WriteFile(filepath.Join(cfg.WorkspaceDirectory, "wire.txt"), []byte(temporalLesson), 0o600))
				temporalRun(t, a, &temporalModel{}, "memory-seed", "Inspect wire.txt and explain the length parser.")
				wantCount = len(temporalPromoted(t, a))
				if wantCount != 1 {
					t.Fatalf("seeded %d promoted memories, want 1", wantCount)
				}
			}
			fake := &memoryRoutingModel{prompt: tc.prompt, toolName: tc.toolName, arguments: tc.arguments, answer: tc.answer}
			result, f := memoryRoutingRun(t, a, "memory-routing", fake)
			if tc.toolName != "" {
				var out struct {
					Operation string `json:"operation"`
					Scope     string `json:"scope"`
					Count     *int   `json:"count"`
					Adopted   bool   `json:"adopted"`
				}
				temporalRequire(t, "decode count observation", json.Unmarshal([]byte(fake.observation), &out))
				if out.Operation != "count" || out.Scope != "currently_reusable" || out.Count == nil || *out.Count != wantCount || out.Adopted {
					t.Fatalf("unexpected authoritative count: %+v", out)
				}
			}
			before := memoryRoutingEvents(t, a, "memory-routing", result, fake)
			history, err := f.LoadHistory(t.Context(), "memory-routing")
			temporalRequire(t, "load memory routing transcript", err)
			temporalRequire(t, "close memory routing app", a.Close())
			reopened := temporalOpen(t, cfg)
			replay := memoryRoutingFactory(t, reopened, temporalNoModel{}, "memory-routing")
			completed, found, err := replay.CompletedTurn(t.Context(), "memory-routing", result.TurnID)
			temporalRequire(t, "restore completed memory turn without a model", err)
			if !found || completed.Final == nil || completed.Final.Content != tc.answer || completed.ExperienceMaintenance != nil {
				t.Fatalf("completed memory turn did not survive reopen: %+v", completed)
			}
			restored, err := replay.LoadHistory(t.Context(), "memory-routing")
			temporalRequire(t, "restore memory routing transcript", err)
			if !reflect.DeepEqual(history, restored) {
				t.Fatal("memory routing transcript changed after native reopen")
			}
			if after := memoryRoutingEvents(t, reopened, "memory-routing", result, fake); !reflect.DeepEqual(before, after) {
				t.Fatal("native memory routing events changed after reopen")
			}
		})
	}
}

func TestMemoryRoutingCandidateQueriesAndMutationApproval(t *testing.T) {
	a := temporalOpen(t, temporalConfig(t, config.Config{}, temporalBinary(t), t.TempDir()))
	const candidateArgs = `{"operation":"create_candidate","new_space":{"name":"Parser preferences","description":"User preferences for parser tests"},"claim":"Test truncated payloads.","kind":"preference"}`
	root, _, err := a.EnsureRootThread(t.Context(), "memory-denied")
	temporalRequire(t, "create denied mutation root", err)
	// Exercise the real policy pipeline with a denying approver. If writes ever
	// become implicitly allowed, this call succeeds and the test fails.
	call := tool.Call{
		SessionID: "memory-denied", TurnID: "denied-turn", CallID: "denied-candidate",
		Actor: root.ActorScope(), Registry: a.Registry().Snapshot(),
		ToolName: "memory_manage", Arguments: json.RawMessage(candidateArgs),
	}
	_, err = a.Pipeline().WithApprover(approval.DenyApprover{}).Execute(t.Context(), call)
	if !errors.Is(err, tool.ErrApprovalDenied) {
		t.Fatalf("candidate mutation must reach approval and be denied, got %v", err)
	}
	memoryRoutingApproval(t, a, call.SessionID, call.CallID, false)

	fake := &memoryRoutingModel{
		prompt: "Remember my preference to test truncated payloads.", toolName: "memory_manage",
		arguments: candidateArgs, answer: "Recorded an unvalidated candidate.",
	}
	result, _ := memoryRoutingRun(t, a, "memory-candidate", fake, approval.NewToolAllowlistApprover("memory_manage"))
	candidateEvents := memoryRoutingEvents(t, a, "memory-candidate", result, fake)
	memoryRoutingApproval(t, a, "memory-candidate", "memory-choice", true)
	var candidate struct {
		Status     string                         `json:"status"`
		Ref        string                         `json:"ref"`
		Claim      string                         `json:"claim"`
		Adopted    bool                           `json:"adopted"`
		Provenance []experience.EvidenceReference `json:"provenance"`
	}
	temporalRequire(t, "decode candidate observation", json.Unmarshal([]byte(fake.observation), &candidate))
	if candidate.Status != "candidate" || candidate.Ref == "" || candidate.Claim != "Test truncated payloads." || candidate.Adopted || len(candidate.Provenance) != 1 {
		t.Fatalf("candidate lost provenance or was automatically promoted/adopted: %+v", candidate)
	}
	provenance := candidate.Provenance[0]
	if provenance.Kind != string(session.EventUserMessage) || provenance.SourceID == "" || provenance.OriginalSHA256 == "" {
		t.Fatalf("candidate provenance must identify original durable user input: %+v", provenance)
	}
	userSequence := uint64(0)
	for _, e := range candidateEvents {
		if e.Type == session.EventUserMessage {
			userSequence = e.Sequence
		}
	}
	if userSequence == 0 || provenance.Sequence != userSequence {
		t.Fatal("candidate provenance does not match the current session's durable user event")
	}
	space, memoryID, ok := strings.Cut(candidate.Ref, "/")
	if !ok {
		t.Fatal("candidate returned no exact memory ref")
	}
	for _, operation := range []string{"directory", "count", "search", "read"} {
		t.Run(operation, func(t *testing.T) {
			args := map[string]any{"operation": operation}
			if operation == "read" {
				args["refs"] = []string{candidate.Ref}
			}
			raw, err := json.Marshal(args)
			temporalRequire(t, "encode query", err)
			query := &memoryRoutingModel{prompt: "Inspect stored memory using " + operation + ".", toolName: "memory_query", arguments: string(raw), answer: "Memory inspection complete."}
			id := session.ID("memory-query-" + operation)
			result, _ := memoryRoutingRun(t, a, id, query, approval.DenyApprover{})
			memoryRoutingEvents(t, a, id, result, query)
			var out struct {
				Operation string                        `json:"operation"`
				Count     *int                          `json:"count"`
				Adopted   bool                          `json:"adopted"`
				Memories  []experience.MemoryToolRecord `json:"memories"`
				Spaces    []struct{ ID string }         `json:"spaces"`
			}
			temporalRequire(t, "decode query observation", json.Unmarshal([]byte(query.observation), &out))
			if out.Operation != operation || out.Adopted {
				t.Fatalf("query changed operation or adopted memory: %+v", out)
			}
			switch operation {
			case "count", "search":
				if out.Count == nil || *out.Count != 0 || len(out.Memories) != 0 {
					t.Fatal("unvalidated candidate became reusable")
				}
			case "directory":
				if len(out.Spaces) != 1 || out.Spaces[0].ID != space {
					t.Fatalf("denied mutation created a space or approved space is missing: %+v", out.Spaces)
				}
			case "read":
				if len(out.Memories) != 1 || out.Memories[0].Ref != candidate.Ref ||
					out.Memories[0].Claim != candidate.Claim || out.Memories[0].Status != memory.StatusCandidate || out.Memories[0].Reusable {
					t.Fatalf("exact candidate read = %+v", out.Memories)
				}
			}
		})
	}
	raw, err := json.Marshal(map[string]string{"operation": "withdraw_source", "ref": candidate.Ref, "reason": "User withdrew this preference."})
	temporalRequire(t, "encode withdrawal", err)
	call.CallID, call.Arguments = "denied-withdrawal", raw
	_, err = a.Pipeline().WithApprover(approval.DenyApprover{}).Execute(t.Context(), call)
	if !errors.Is(err, tool.ErrApprovalDenied) {
		t.Fatalf("withdrawal bypassed approval: %v", err)
	}
	memoryRoutingApproval(t, a, call.SessionID, call.CallID, false)
	nativeState := func() memory.Snapshot {
		t.Helper()
		var snapshot memory.Snapshot
		err := a.temporalBridge.WithClient(t.Context(), func(c *client.Client) error {
			state, err := c.GetMemoryState(t.Context(), api.ReadMemoryStateRequest{Space: atemodel.SpaceID(space)})
			snapshot = state.Snapshot
			return err
		})
		temporalRequire(t, "inspect native candidate state", err)
		return snapshot
	}
	before := nativeState()
	if len(before.Bans) != 0 || len(before.Memories) != 1 {
		t.Fatal("denied withdrawal changed native memory state")
	}
	withdraw := &memoryRoutingModel{prompt: "Withdraw the saved preference.", toolName: "memory_manage", arguments: string(raw), answer: "The source can no longer be reused."}
	result, _ = memoryRoutingRun(t, a, "memory-withdraw", withdraw, approval.NewToolAllowlistApprover("memory_manage"))
	memoryRoutingEvents(t, a, "memory-withdraw", result, withdraw)
	memoryRoutingApproval(t, a, "memory-withdraw", "memory-choice", true)
	var withdrawn struct {
		Status    string `json:"status"`
		SourceRef string `json:"source_ref"`
	}
	temporalRequire(t, "decode withdrawal", json.Unmarshal([]byte(withdraw.observation), &withdrawn))
	if withdrawn.Status != "source_reuse_banned" || withdrawn.SourceRef != candidate.Ref {
		t.Fatalf("withdrawal = %+v", withdrawn)
	}
	// Inspect native state independently of the tool's response.
	after := nativeState()
	if len(after.Memories) != 2 || len(after.Successors) != 1 || string(after.Successors[0].Before) != memoryID {
		t.Fatal("withdrawal must preserve the candidate and append one retracted successor")
	}
	original, retracted := false, false
	for _, m := range after.Memories {
		if string(m.ID) == memoryID {
			original = reflect.DeepEqual(m, before.Memories[0]) && m.Status == memory.StatusCandidate
		}
		if m.ID == after.Successors[0].After {
			retracted = m.Status == memory.StatusRetracted && string(m.Claim) == candidate.Claim &&
				m.Retraction != nil && m.Retraction.Kind == memory.RetractionUseBan
		}
	}
	if !original || !retracted {
		t.Fatal("withdrawal changed history or failed to append its retracted version")
	}
	if len(after.Bans) != 1 || string(after.Bans[0].Source) != memoryID ||
		after.Bans[0].Retraction.Kind != memory.RetractionUseBan || after.Bans[0].Retraction.Reason != "User withdrew this preference." {
		t.Fatalf("approved withdrawal did not persist the native source ban: %+v", after.Bans)
	}
}

func memoryRoutingApproval(t *testing.T, a *App, id session.ID, callID string, approved bool) {
	t.Helper()
	var flow []session.EventType
	err := session.VisitEvents(t.Context(), a.events, id, func(e session.Event) error {
		if e.CorrelationID != callID {
			return nil
		}
		var p struct {
			Effect   policy.Effect `json:"effect"`
			Approved bool          `json:"approved"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		switch e.Type {
		case session.EventPolicyDecided:
			if p.Effect != policy.EffectRequireApproval {
				return errors.New("memory mutation policy did not require approval")
			}
		case session.EventApprovalRequested:
		case session.EventApprovalResolved:
			if p.Approved != approved {
				return errors.New("durable approval outcome disagrees with approver")
			}
		case session.EventToolStarted, session.EventToolSucceeded:
			if !approved {
				return errors.New("denied memory mutation executed")
			}
			return nil
		default:
			return nil
		}
		flow = append(flow, e.Type)
		return nil
	})
	temporalRequire(t, "inspect memory mutation approval audit", err)
	want := []session.EventType{session.EventPolicyDecided, session.EventApprovalRequested, session.EventApprovalResolved}
	if !reflect.DeepEqual(flow, want) {
		t.Fatalf("memory mutation approval flow = %v, want %v", flow, want)
	}
}
