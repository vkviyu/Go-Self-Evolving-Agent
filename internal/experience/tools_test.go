package experience

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func memoryToolActor(t *testing.T) orchestration.ActorScope {
	t.Helper()
	team, err := orchestration.NewTeamID()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	thread, err := orchestration.NewThreadID()
	if err != nil {
		t.Fatal(err)
	}
	return orchestration.ActorScope{TeamID: team, AgentID: agent, ThreadID: thread}
}

// Direct domain calls exercise durable service replay without a pipeline cache.
func invokeMemoryTool(t *testing.T, f *fixture, actor orchestration.ActorScope, callID, name string, input any) (json.RawMessage, error) {
	t.Helper()
	executor := f.s.QueryMemoryTool
	if name == "memory_manage" {
		executor = f.s.ManageMemoryTool
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return executor(t.Context(), MemoryCaller{
		SessionID: "memory-session", TurnID: "memory-turn", CallID: callID,
		ActorID: string(actor.AgentID), ScopeDigest: digest(actor),
	}, raw)
}

func memoryCandidateInput() map[string]any {
	return map[string]any{
		"operation": "create_candidate", "kind": "preference", "claim": "The user prefers concise status updates; this is a stated preference.",
		"new_space": map[string]string{"name": "Communication", "description": "Explicit user preferences and their scope."},
	}
}

func appendMemoryInput(t *testing.T, f *fixture, text string) {
	t.Helper()
	_, err := f.history.Append(t.Context(), "memory-session", session.AnySequence, session.PendingEvent{
		Type: session.EventUserMessage, CorrelationID: "memory-turn",
		Payload: session.MustJSONPayload(map[string]string{"content": text}),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMemoryToolCandidateFrozenAcrossLostReplyAndRestart(t *testing.T) {
	f := newFixture(t)
	actor := memoryToolActor(t)
	appendMemoryInput(t, f, "Please keep status updates concise.")
	input := memoryCandidateInput()
	f.p.dropMethod = api.MethodApplyMemory
	if _, err := invokeMemoryTool(t, f, actor, "create", "memory_manage", input); err == nil {
		t.Fatal("expected lost mutation response")
	} else {
		if !errors.Is(err, atebridge.ErrOutcomeUnknown) && !errors.Is(err, aterequest.ErrUnknown) {
			t.Fatalf("native uncertainty was lost: %v", err)
		}
	}
	pending, err := f.db.ATERequests().List(t.Context(), aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(pending) != 1 {
		t.Fatalf("original pending request missing: %d %v", len(pending), err)
	}
	appendMemoryInput(t, f, "Later context must not change frozen provenance.")
	f.restart(t)
	raw, err := invokeMemoryTool(t, f, actor, "create", "memory_manage", input)
	if err != nil {
		t.Fatal(err)
	}
	var result memoryManageResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "candidate" || result.Adopted || result.Claim != input["claim"] ||
		len(result.Provenance) != 1 || result.Provenance[0].Sequence != 1 {
		t.Fatalf("candidate/provenance mismatch: %s", raw)
	}
	commits := f.p.commits
	again, err := invokeMemoryTool(t, f, actor, "create", "memory_manage", input)
	if err != nil || !bytes.Equal(raw, again) || f.p.commits != commits {
		t.Fatalf("replay changed result or committed again: %s %v", again, err)
	}
	space, _, _ := memoryToolRef(result.Ref)
	if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		state, err := c.GetMemoryState(t.Context(), api.ReadMemoryStateRequest{Space: space})
		if err != nil {
			return err
		}
		if len(state.Snapshot.Memories) != 1 {
			t.Fatalf("duplicate candidate: %d", len(state.Snapshot.Memories))
		}
		m := state.Snapshot.Memories[0]
		if m.Status != memory.StatusCandidate || m.Validation != nil || len(m.Sources) == 0 || m.Sources[0].Anchor.Space != "gsea-authority" {
			t.Fatalf("fabricated validation/provenance: %+v", m)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pending, err = f.db.ATERequests().List(t.Context(), aterequest.ListOptions{Limit: 128, MaxBytes: aterequest.MaxListBytes})
	if err != nil || len(pending) != 0 {
		t.Fatalf("replay did not reconcile pending: %v", err)
	}
	if _, err := invokeMemoryTool(t, f, memoryToolActor(t), "create", "memory_manage", input); !errors.Is(err, ateports.ErrConflict) {
		t.Fatalf("changed actor accepted under original identity: %v", err)
	}
	input["claim"] = "Changed claim"
	if _, err := invokeMemoryTool(t, f, actor, "create", "memory_manage", input); !errors.Is(err, ateports.ErrConflict) {
		t.Fatalf("changed payload accepted under original identity: %v", err)
	}
}

func TestMemoryToolRejectsForgedProvenanceAndUntrustedInvocation(t *testing.T) {
	f := newFixture(t)
	actor := memoryToolActor(t)
	if _, err := f.s.ManageMemoryTool(context.Background(), MemoryCaller{}, json.RawMessage(`{"operation":"create_candidate"}`)); err == nil {
		t.Fatal("untrusted invocation accepted")
	}
	if _, err := f.s.QueryMemoryTool(context.Background(), MemoryCaller{}, json.RawMessage(`{"operation":"count"}`)); err == nil {
		t.Fatal("untrusted query accepted")
	}
	appendMemoryInput(t, f, "A user assertion is not tool proof.")
	before := f.p.commits
	in := memoryCandidateInput()
	in["event_ids"] = []string{"invented-event"}
	if _, err := invokeMemoryTool(t, f, actor, "forged", "memory_manage", in); err == nil {
		t.Fatal("fabricated event accepted")
	}
	delete(in, "event_ids")
	in["session_id"] = "another-session"
	if _, err := invokeMemoryTool(t, f, actor, "forged-session", "memory_manage", in); err == nil {
		t.Fatal("model-controlled session accepted")
	}
	if f.p.commits != before {
		t.Fatalf("rejected candidate made business writes: %d", f.p.commits)
	}
}

func TestMemoryToolQueriesAndCandidateWithdrawal(t *testing.T) {
	f := newFixture(t)
	actor := memoryToolActor(t)
	appendMemoryInput(t, f, "Please keep status updates concise.")
	raw, err := invokeMemoryTool(t, f, actor, "candidate", "memory_manage", memoryCandidateInput())
	if err != nil {
		t.Fatal(err)
	}
	var candidate memoryManageResult
	if err := json.Unmarshal(raw, &candidate); err != nil {
		t.Fatal(err)
	}
	before := f.p.commits
	for _, op := range []string{"count", "search", "directory", "read"} {
		in := map[string]any{"operation": op}
		if op == "read" {
			in["refs"] = []string{candidate.Ref}
		}
		raw, err := invokeMemoryTool(t, f, actor, op, "memory_query", in)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(`"adopted":false`)) {
			t.Fatalf("view reported adoption: %s", raw)
		}
		if !bytes.Contains(raw, []byte(`"untrusted_data":true`)) {
			t.Fatalf("%s omitted the persisted-data trust boundary", op)
		}
		if op == "read" && (!bytes.Contains(raw, []byte(candidate.Claim)) || !bytes.Contains(raw, []byte(`"reusable":false`))) {
			t.Fatalf("candidate read not plain text or wrong eligibility: %s", raw)
		}
		if op == "count" && !bytes.Contains(raw, []byte(`"count":0`)) {
			t.Fatalf("unvalidated candidate counted as reusable: %s", raw)
		}
	}
	if f.p.commits != before {
		t.Fatal("viewing made business mutations")
	}
	if _, err := invokeMemoryTool(t, f, actor, "bad-adopt", "memory_manage", map[string]any{"operation": "adopt", "ref": candidate.Ref}); err == nil {
		t.Fatal("unvalidated candidate adopted")
	}
	in := map[string]any{"operation": "correct_source", "ref": candidate.Ref, "reason": "The preference was attributed incorrectly."}
	raw, err = invokeMemoryTool(t, f, actor, "correct", "memory_manage", in)
	if err != nil {
		t.Fatal(err)
	}
	before = f.p.commits
	f.restart(t)
	again, err := invokeMemoryTool(t, f, actor, "correct", "memory_manage", in)
	if err != nil || !bytes.Equal(raw, again) || f.p.commits != before {
		t.Fatalf("correction replay: %s %v", again, err)
	}
	space, id, _ := memoryToolRef(candidate.Ref)
	if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		state, err := c.GetMemoryState(t.Context(), api.ReadMemoryStateRequest{Space: space})
		if err != nil {
			return err
		}
		original, retracted := false, false
		for _, m := range state.Snapshot.Memories {
			original = original || m.ID == id
			retracted = retracted || (m.Status == memory.StatusRetracted && m.Retraction.Kind == memory.RetractionFactualCorrection)
		}
		if !original || !retracted {
			t.Fatal("correction erased original or failed to append retraction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryToolExplicitAdoptionReplayAndOriginWithdrawal(t *testing.T) {
	f := newFixture(t)
	actor := memoryToolActor(t)
	// Existing native fixture creates a promoted record using original tool
	// evidence and its separate review path; memory_manage never promotes.
	f.events(t, "training-session", 1)
	if err := f.s.Complete(t.Context(), "training-session", "training-turn", newModel(t, f).generate); err != nil {
		t.Fatal(err)
	}
	items, err := f.s.SearchMemories(t.Context(), "retry", 16)
	if err != nil || len(items) != 1 {
		t.Fatalf("fixture promoted memory: %d %v", len(items), err)
	}
	ref := items[0].ID
	in := map[string]any{"operation": "adopt", "ref": ref}
	f.p.dropMethod = api.MethodApplyKnowledge
	if _, err := invokeMemoryTool(t, f, actor, "adopt", "memory_manage", in); err == nil {
		t.Fatal("expected lost adoption response")
	}
	f.restart(t)
	raw, err := invokeMemoryTool(t, f, actor, "adopt", "memory_manage", in)
	if err != nil {
		t.Fatal(err)
	}
	var result memoryManageResult
	if err := json.Unmarshal(raw, &result); err != nil || !result.Adopted || result.Execution == nil {
		t.Fatalf("adoption result: %s %v", raw, err)
	}
	before := f.p.commits
	again, err := invokeMemoryTool(t, f, actor, "adopt", "memory_manage", in)
	if err != nil || !bytes.Equal(raw, again) || f.p.commits != before {
		t.Fatalf("adoption replay: %s %v", again, err)
	}
	if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		state, err := c.GetKnowledgeState(t.Context(), api.ReadKnowledgeStateRequest{Space: f.b.OperationalSpace(), Knowledge: result.Execution})
		if err != nil {
			return err
		}
		if len(state.Snapshot.Observations) != 1 || state.Snapshot.Observations[0].ID != result.Observation {
			t.Fatal("missing/duplicate native adoption observation")
		}
		agents, err := c.ReadFullAgentState(t.Context(), api.ReadAgentStateRequest{Space: f.b.OperationalSpace()})
		if err != nil {
			return err
		}
		found := false
		for _, execution := range agents.Snapshot.Executions {
			if execution.ID == result.Execution.Execution {
				found = true
				if execution.Status != agentprofile.ExecutionCanceled {
					t.Fatal("record-only adoption left a nonterminal execution")
				}
				for _, task := range agents.Snapshot.Tasks {
					if task.ID == execution.Task && task.Status != agentprofile.TaskCanceled {
						t.Fatal("record-only adoption left a nonterminal task")
					}
				}
			}
		}
		if !found {
			t.Fatal("adoption execution missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sourceRef := memoryIdentity(items[0].Memory.Sources[0])
	if !strings.HasPrefix(sourceRef, "gsea-authority/") {
		t.Fatal("fixture should use foreign native session provenance")
	}
	withdraw := map[string]any{"operation": "withdraw_source", "ref": ref, "source_ref": sourceRef, "reason": "Original source no longer supports reuse."}
	if _, err := invokeMemoryTool(t, f, actor, "withdraw", "memory_manage", withdraw); err != nil {
		t.Fatal(err)
	}
	if _, err := invokeMemoryTool(t, f, actor, "adopt", "memory_manage", in); err == nil {
		t.Fatal("durable adoption receipt bypassed current origin policy")
	}
	items, err = f.s.SearchMemories(t.Context(), "retry", 16)
	if err != nil || len(items) != 0 {
		t.Fatalf("origin withdrawal did not suppress foreign derived memory: %d %v", len(items), err)
	}
}

func TestMemoryToolRecordIncludesCompleteSourceClosure(t *testing.T) {
	source := func(id string) memory.Source {
		return memory.Source{SourceID: model.ObjectID(strings.Repeat(id, 64)), Anchor: model.SnapshotAnchor{Space: "evidence"}}
	}
	direct, evidence, validation, counterexample := source("a"), source("b"), source("c"), source("d")
	record := toolRecord(memory.Memory{
		Sources:  []memory.Source{direct},
		Evidence: []memory.Evidence{{Source: evidence}},
		Validation: &memory.Validation{
			Evidence: []memory.Source{validation, direct}, Counterexamples: []memory.Source{counterexample},
		},
	}, direct, true, 0)
	if len(record.Sources) != 4 || len(record.SourceRefs) != 4 {
		t.Fatalf("incomplete or duplicate source closure: %+v", record.SourceRefs)
	}
	for i, expected := range []memory.Source{direct, evidence, validation, counterexample} {
		if record.SourceRefs[i] != memoryIdentity(expected) || record.Sources[i].SourceID != expected.SourceID {
			t.Fatalf("source/ref mismatch at %d", i)
		}
	}
}
