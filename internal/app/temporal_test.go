package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	atemodel "github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func temporalRepo(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate temporal test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func temporalBinary(t *testing.T) string {
	t.Helper()
	return testATEBinary(t)
}

func temporalConfig(t *testing.T, base config.Config, binary, shared string) config.Config {
	t.Helper()
	base.HomeDirectory = t.TempDir()
	base.DataDirectory = t.TempDir()
	base.WorkspaceDirectory = t.TempDir()
	base.CoordinationDirectory = t.TempDir()
	base.ATE = config.ATEConfig{Enabled: true, Binary: binary, Directory: shared, Timeout: 30 * time.Second}
	base.LSP.Enabled = false
	return base
}

func temporalOpen(t *testing.T, cfg config.Config) *App {
	t.Helper()
	a, err := Open(t.Context(), cfg)
	temporalRequire(t, "open App", err)
	t.Cleanup(func() { temporalRequire(t, "close App", a.Close()) })
	if a.temporalProcess == nil || a.temporalBridge == nil || a.experience == nil {
		t.Fatal("App did not wire native temporal services")
	}
	return a
}

// Error bodies may contain provider URLs, headers or response text. Report
// only a fixed classification, including on cleanup and assertion failures.
func temporalErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, experience.ErrDecision) {
		return "experience-decision"
	}
	text := strings.ToLower(err.Error())
	for _, item := range []struct{ match, class string }{
		{"invalid canonical event", "native-canonical-event"}, {"store_fenced", "native-store-fenced"},
		{"401", "authentication"}, {"403", "authorization"}, {"429", "rate-limit"},
		{"credential", "credential"}, {"no such host", "dns"}, {"connection refused", "connection"},
		{"context deadline", "deadline"}, {"invalid model decision", "experience-decision"},
		{"maximum iterations", "iteration-limit"}, {"max iteration", "iteration-limit"},
		{"invalid arguments", "tool-arguments"}, {"budget", "budget"},
		{"responses stream ended before response.completed", "provider-stream-truncated"},
		{"unexpected eof", "provider-unexpected-eof"},
		{"responses sse event name and payload type disagree", "provider-stream-event-mismatch"},
		{"responses function call is missing an id or name", "provider-tool-identity-missing"},
		{"responses function call stream item is missing an id", "provider-tool-identity-missing"},
		{"responses response contains no assistant output", "provider-output-empty"},
		{"response.completed has invalid status", "provider-completion-status-invalid"},
		{"responses stream error:", "provider-stream-error"},
		{"connection reset", "connection-reset"}, {"tls handshake", "tls-handshake"},
		{"decode responses", "provider-decode"}, {"status 500", "provider-http-500"},
		{"status 502", "provider-http-502"}, {"status 503", "provider-http-503"},
	} {
		if strings.Contains(text, item.match) {
			return item.class
		}
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range e.Unwrap() {
			class := temporalErrorClass(cause)
			if !strings.HasPrefix(class, "error-type:") && class != "none" {
				return class
			}
		}
	case interface{ Unwrap() error }:
		if cause := e.Unwrap(); cause != nil {
			return temporalErrorClass(cause)
		}
	}
	return fmt.Sprintf("error-type:%T", err)
}

func temporalRequire(t *testing.T, stage string, err error) {
	t.Helper()
	if err != nil {
		if os.Getenv("GSEA_TEST_REAL_ATE") != "1" {
			temporalLogCauses(t, err)
			t.Fatalf("%s failed: %v", stage, err)
		}
		t.Fatalf("%s failed (%s; details withheld)", stage, temporalErrorClass(err))
	}
}

func temporalLogCauses(t *testing.T, err error) {
	t.Helper()
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range e.Unwrap() {
			temporalLogCauses(t, cause)
		}
	case interface{ Unwrap() error }:
		temporalLogCauses(t, e.Unwrap())
	default:
		if err != nil {
			t.Logf("deterministic root cause (%T): %v", err, err)
		}
	}
}

func temporalFactory(t *testing.T, ctx context.Context, a *App, chat model.BaseChatModel, id string) *agentruntime.Factory {
	t.Helper()
	root, _, err := a.EnsureRootThread(ctx, session.ID(id))
	temporalRequire(t, "ensure root thread", err)
	cfg := a.config
	factory, err := a.NewAgentFactory(chat, agentruntime.Config{
		ActorScope: root.ActorScope(), Instruction: cfg.Agent.Instruction,
		MaxIterations: cfg.Agent.MaxIterations, MaxOutputTokens: cfg.Agent.MaxOutputTokens,
		ContextWindowTokens: cfg.Agent.ContextWindowTokens, CompactionTriggerTokens: cfg.Agent.CompactionTriggerTokens,
		Provider: cfg.Model.Provider, Model: cfg.Model.Name, UsageLedger: a.Orchestration(),
		// These are explicit opt-in legacy service tests, including the real
		// provider tests that share this helper; default turns use memory tools.
		Experience:          temporalExperience{Service: a.experience, bridge: a.temporalBridge},
		ExperienceLifecycle: temporalLifecycle{bridge: a.temporalBridge},
	}, approval.NewToolAllowlistApprover("workspace_write", "workspace_edit", "shell_exec"))
	temporalRequire(t, "construct actual runtime factory", err)
	return factory
}

func temporalRun(t *testing.T, a *App, chat model.BaseChatModel, id, prompt string) (agentruntime.Result, experience.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	factory := temporalFactory(t, ctx, a, chat, id)
	turn, err := factory.NewTurn(ctx, session.ID(id))
	temporalRequire(t, "new runtime turn", err)
	start := time.Now()
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage(prompt)}, nil)
	t.Logf("session=%s runtime_with_maintenance=%s", id, time.Since(start).Round(time.Millisecond))
	temporalTrace(t, a, id)
	temporalRequire(t, "run runtime turn", err)
	if result.Final == nil || result.ExperienceMaintenance == nil {
		t.Fatal("runtime did not return final output and maintenance status")
	}
	if result.ExperienceMaintenance.Status == "pending" &&
		temporalErrorClassIfPresent(result.ExperienceMaintenance.Error) == "deadline" {
		// Exercise the normal post-turn retry admission exactly once. The
		// original completed turn is never executed again, and its persisted
		// maintenance must actually finish before any success assertion.
		retryStart := time.Now()
		// A separate ordinary turn receives its own bounded caller context.
		// Its production maintenance timeout remains unchanged.
		retryCtx, retryCancel := context.WithTimeout(t.Context(), 12*time.Minute)
		defer retryCancel()
		ctx = retryCtx
		// The production pending-work index spans this App's sessions. Use a
		// fresh acknowledgment session so its own evidence is only the
		// acknowledgment, rather than re-curating the completed parser task.
		followupID := "acknowledgment-" + id
		followupFactory := temporalFactory(t, ctx, a, chat, followupID)
		followup, err := followupFactory.NewTurn(ctx, session.ID(followupID))
		temporalRequire(t, "create ordinary maintenance-recovery follow-up", err)
		recovered, err := followup.Run(ctx, []*schema.Message{schema.UserMessage("Thanks. No further changes or commands are needed; briefly acknowledge.")}, nil)
		t.Logf("session=%s ordinary_followup_with_retry=%s", id, time.Since(retryStart).Round(time.Millisecond))
		temporalTrace(t, a, followupID)
		temporalTrace(t, a, id)
		temporalRequire(t, "run ordinary follow-up and production maintenance retry", err)
		if recovered.Final == nil || recovered.ExperienceMaintenance == nil || recovered.ExperienceMaintenance.Status != "completed" {
			t.Fatal("ordinary follow-up did not complete its own durable maintenance")
		}
		completed, found, err := factory.CompletedTurn(ctx, session.ID(id), result.TurnID)
		temporalRequire(t, "reload original turn after production maintenance retry", err)
		if !found || completed.ExperienceMaintenance == nil || completed.ExperienceMaintenance.Status != "completed" ||
			completed.ExperienceMaintenance.RetryAttempts == 0 {
			_, indexed := a.events.(agentruntime.PendingExperienceMaintenanceStore)
			t.Fatalf("original turn maintenance did not durably complete through production retry: found=%t cross_session_index=%t",
				found, indexed)
		}
		result.ExperienceMaintenance = completed.ExperienceMaintenance
		t.Logf("session=%s original_turn_maintenance=completed production_retry_attempts=%d",
			id, completed.ExperienceMaintenance.RetryAttempts)
	}
	if result.ExperienceMaintenance.Status != "completed" {
		if os.Getenv("GSEA_TEST_REAL_ATE") != "1" {
			t.Logf("deterministic maintenance error: %s", result.ExperienceMaintenance.Error)
		}
		t.Fatalf("maintenance incomplete (%s)", temporalErrorClass(errors.New(result.ExperienceMaintenance.Error)))
	}
	var last agentruntime.ExperienceMaintenance
	var selected experience.Context
	toolSuccesses := 0
	err = session.VisitEvents(ctx, a.events, session.ID(id), func(e session.Event) error {
		// Pipeline correlations name calls; the turn identity is in payload.
		if e.Type == session.EventToolSucceeded {
			var payload struct {
				TurnID string `json:"turn_id"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				return err
			}
			if payload.TurnID == result.TurnID {
				toolSuccesses++
			}
		}
		if e.CorrelationID != result.TurnID {
			return nil
		}
		switch e.Type {
		case agentruntime.EventExperienceMaintenance:
			return json.Unmarshal(e.Payload, &last)
		case session.EventAssistantMessage:
			var payload struct {
				Purpose string `json:"purpose"`
				Receipt *struct {
					Experience *experience.Context `json:"experience"`
					Input      []*schema.Message   `json:"input"`
					Index      *int                `json:"experience_index"`
				} `json:"receipt"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				return err
			}
			if payload.Purpose == "" && payload.Receipt != nil && payload.Receipt.Experience != nil {
				r := payload.Receipt
				if r.Index == nil || *r.Index < 0 || *r.Index >= len(r.Input) ||
					r.Input[*r.Index].Role != schema.User || r.Input[*r.Index].Content != r.Experience.Text {
					return errors.New("experience not in actual dispatched user-role input")
				}
				selected = *r.Experience
			}
		}
		return nil
	})
	temporalRequire(t, "read durable runtime evidence", err)
	if last.Status != "completed" || !last.Outcome.Succeeded || toolSuccesses == 0 || selected.SelectionID == "" {
		t.Fatalf("durable evidence missing: completed=%t succeeded=%t tool_successes=%d selection=%t",
			last.Status == "completed", last.Outcome.Succeeded, toolSuccesses, selected.SelectionID != "")
	}
	temporalRequire(t, "validate native adoption", a.experience.Check(ctx, selected))
	temporalNativeCompleted(t, a, selected)
	t.Logf("session=%s durable_maintenance=completed tool_successes=%d adopted_memories=%d", id, toolSuccesses, len(selected.Memories))
	return result, selected
}

func temporalNativeCompleted(t *testing.T, a *App, selected experience.Context) {
	t.Helper()
	ctx := t.Context()
	err := a.temporalBridge.WithClient(ctx, func(c *client.Client) error {
		state, err := c.ReadFullAgentState(ctx, api.ReadAgentStateRequest{Space: a.temporalBridge.OperationalSpace()})
		if err != nil {
			return err
		}
		stepDone, actionApplied := false, false
		for _, step := range state.Snapshot.Steps {
			if step.Execution == selected.Execution.Execution && step.Status == agentprofile.StepCompleted {
				stepDone = true
			}
		}
		for _, action := range state.Snapshot.Actions {
			if action.Owner == selected.Execution.Execution && action.Status == agentprofile.ActionApplied {
				actionApplied = true
			}
		}
		if !stepDone || !actionApplied {
			return errors.New("native step or checkpoint continuation not completed")
		}
		for _, execution := range state.Snapshot.Executions {
			if execution.ID == selected.Execution.Execution && execution.Status == agentprofile.ExecutionCompleted {
				for _, task := range state.Snapshot.Tasks {
					if task.ID == execution.Task && task.Status == agentprofile.TaskCompleted {
						return nil
					}
				}
			}
		}
		return errors.New("native task or execution not completed")
	})
	temporalRequire(t, "verify native terminal outcome", err)
	t.Log("native task/execution/step completed; checkpoint continuation applied")
}

// Report counts and a closed set of internal stage names, never model content.
func temporalTrace(t *testing.T, a *App, id string) {
	t.Helper()
	counts := map[string]int{}
	failures := map[string]int{}
	started := map[string]bool{}
	lastStage := "ordinary"
	var lastInput []byte
	var requestedAt time.Time
	maintenanceStarts := map[string]time.Time{}
	err := session.VisitEvents(t.Context(), a.events, session.ID(id), func(e session.Event) error {
		switch e.Type {
		case agentruntime.EventExperienceMaintenance:
			var p agentruntime.ExperienceMaintenance
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if maintenanceStarts[e.CorrelationID].IsZero() {
				maintenanceStarts[e.CorrelationID] = e.OccurredAt
			}
			t.Logf("maintenance_completed=%t retry_attempts=%d elapsed=%s error_class=%s",
				p.Status == "completed", p.RetryAttempts, e.OccurredAt.Sub(maintenanceStarts[e.CorrelationID]).Round(time.Millisecond),
				temporalErrorClassIfPresent(p.Error))
		case session.EventModelFailed:
			var p struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			t.Logf("model_failure stage=%s elapsed=%s class=%s", lastStage,
				e.OccurredAt.Sub(requestedAt).Round(time.Millisecond), temporalErrorClassIfPresent(p.Error))
		case session.EventToolProposed:
			var p struct {
				Tool      string          `json:"tool"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			name := "other"
			switch p.Tool {
			case "workspace_read", "workspace_list", "workspace_glob", "workspace_grep", "workspace_write", "workspace_edit", "shell_exec":
				name = p.Tool
			}
			t.Logf("tool=%s arguments=%s", name, temporalJSONShape(p.Arguments))
		case session.EventToolStarted:
			started[e.CorrelationID] = true
		case session.EventToolSucceeded:
			counts["tool_succeeded"]++
		case session.EventToolFailed:
			counts["tool_failed"]++
			var p struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			class := "other"
			for _, marker := range []string{"offset must be non-negative and cannot be combined with a line range", "additionalProperties", "missing properties", "invalid character", "unknown field", "json:", "valid JSON", "not a regular file", "relative", "outside", "no such file", "not found", "actor", "lease", "deadline", "denied", "empty", "null", "required", "minimum", "maximum", "expected", "schema", "offset", "line", "range", "budget"} {
				if strings.Contains(p.Error, marker) {
					class = marker
					break
				}
			}
			phase := "before-executor"
			if started[e.CorrelationID] {
				phase = "executor"
			}
			failures[phase+":"+class]++
		case session.EventAssistantMessage:
			counts["model_responses"]++
			var p struct {
				Purpose string          `json:"purpose"`
				Message *schema.Message `json:"message"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if p.Purpose == "experience" && p.Message != nil {
				t.Logf("auxiliary_stage=%s elapsed=%s tool_calls=%d response_shape=%s", lastStage,
					e.OccurredAt.Sub(requestedAt).Round(time.Millisecond), len(p.Message.ToolCalls), temporalJSONShape([]byte(p.Message.Content)))
				temporalDecisionFields(t, lastStage, p.Message.Content)
				if lastStage == "organize" {
					temporalOrganizationTrace(t, lastInput, []byte(p.Message.Content))
				}
			}
		case session.EventModelRequested:
			counts["model_requests"]++
			requestedAt = e.OccurredAt
			var p struct {
				Purpose  string            `json:"purpose"`
				Messages []*schema.Message `json:"messages"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return err
			}
			if p.Purpose == "experience" {
				counts["auxiliary_requests"]++
				lastInput = nil
				for _, message := range p.Messages {
					if message != nil && message.Role == schema.User {
						lastInput = []byte(message.Content)
					}
				}
				for _, m := range p.Messages {
					if m == nil || m.Role != schema.System {
						continue
					}
					for _, stage := range []string{"discover", "history-map", "history-reduce", "extract", "verify", "select", "organize"} {
						if strings.Contains(m.Content, "\nStage: "+stage+"\n") {
							lastStage = stage
						}
					}
				}
			} else {
				lastStage = "ordinary"
			}
		}
		return nil
	})
	if err != nil {
		t.Log("sanitized execution trace unavailable")
		return
	}
	t.Logf("session=%s requests=%d responses=%d auxiliary_requests=%d tool_successes=%d tool_failures=%d last_stage=%s",
		id, counts["model_requests"], counts["model_responses"], counts["auxiliary_requests"], counts["tool_succeeded"], counts["tool_failed"], lastStage)
	if len(failures) > 0 {
		t.Logf("sanitized_tool_failure_classes=%v", failures)
	}
}

func temporalOrganizationTrace(t *testing.T, input, output []byte) {
	t.Helper()
	var catalog struct {
		Spaces []struct {
			ID string `json:"space_id"`
		} `json:"spaces"`
		Memories []struct {
			ID string `json:"source_id"`
		} `json:"memories"`
	}
	var decision struct {
		Actions []struct {
			Kind        string `json:"kind"`
			Space       string `json:"space_id"`
			Target      string `json:"target_id"`
			Source      string `json:"source_id"`
			To          string `json:"to_id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Relation    string `json:"relation_kind"`
		} `json:"actions"`
	}
	if json.Unmarshal(input, &catalog) != nil || json.Unmarshal(output, &decision) != nil {
		return
	}
	spaces, memories := map[string]bool{}, map[string]bool{}
	for _, item := range catalog.Spaces {
		spaces[item.ID] = true
	}
	for _, item := range catalog.Memories {
		memories[item.ID] = true
	}
	for i, action := range decision.Actions {
		kind := "unknown"
		switch action.Kind {
		case "rename", "archive", "attach", "remove", "relate", "remove_relation":
			kind = action.Kind
		}
		// Emit only known enums, counts and booleans, never identifiers/text.
		t.Logf("organization_action=%d kind=%s supplied_space=%t supplied_source=%t supplied_to=%t nonempty_target=%t nonempty_name=%t nonempty_description=%t nonempty_relation=%t",
			i, kind, spaces[action.Space], memories[action.Source], memories[action.To],
			action.Target != "", action.Name != "", action.Description != "", action.Relation != "")
	}
}

func temporalErrorClassIfPresent(text string) string {
	if text == "" {
		return "none"
	}
	return temporalErrorClass(errors.New(text))
}

// Only known schema keys are printable. Unknown keys and all scalar values
// remain hidden, including malformed JSON and nested model-generated text.
func temporalJSONShape(raw []byte) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Sprintf("invalid-json(bytes=%d)", len(raw))
	}
	var shape func(any, int) string
	shape = func(v any, depth int) string {
		if depth > 4 {
			return "nested"
		}
		switch v := v.(type) {
		case nil:
			return "null"
		case string:
			return "string"
		case bool:
			return "boolean"
		case float64:
			return "number"
		case []any:
			types := map[string]bool{}
			for _, item := range v {
				types[shape(item, depth+1)] = true
			}
			items := []string{}
			for item := range types {
				items = append(items, item)
			}
			sort.Strings(items)
			return fmt.Sprintf("array(count=%d,items=%s)", len(v), strings.Join(items, "|"))
		case map[string]any:
			fields, unknown := []string{}, 0
			for key, item := range v {
				switch key {
				case "path", "start_line", "end_line", "offset", "max_bytes", "max_entries",
					"pattern", "glob", "ignore_case", "max_results", "content", "mode", "expected_sha256",
					"old_text", "new_text", "replace_all", "command", "working_directory", "timeout_ms", "max_output_bytes",
					"space_ids", "new_space", "name", "description", "search_terms", "full_history",
					"notes", "text", "event_ids", "lessons", "claim", "kind", "tags", "passed", "reason",
					"claims_tool_success", "tool_success_event_ids", "memory_ids", "actions", "space_id", "target_id", "source_id", "to_id", "relation_kind":
					fields = append(fields, key+":"+shape(item, depth+1))
				default:
					unknown++
				}
			}
			sort.Strings(fields)
			return fmt.Sprintf("object{%s;unknown_fields=%d}", strings.Join(fields, ","), unknown)
		}
		return "unknown-type"
	}
	return shape(value, 0)
}

func temporalDecisionFields(t *testing.T, stage, content string) {
	t.Helper()
	required := map[string][]string{
		"discover":    {"space_ids", "new_space", "search_terms", "full_history"},
		"history-map": {"notes"}, "history-reduce": {"notes"},
		"extract": {"lessons"}, "verify": {"passed", "reason", "event_ids", "claims_tool_success", "tool_success_event_ids"},
		"select": {"memory_ids"}, "organize": {"actions"},
	}[stage]
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &fields) != nil || fields == nil {
		return
	}
	missing := []string{}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			missing = append(missing, key)
		}
		delete(fields, key)
	}
	if len(missing) > 0 || len(fields) > 0 {
		t.Logf("auxiliary_stage=%s missing_fields=%v extra_field_count=%d", stage, missing, len(fields))
	}
}

func temporalPromoted(t *testing.T, a *App) map[atemodel.ObjectID]memory.Memory {
	t.Helper()
	found := make(map[atemodel.ObjectID]memory.Memory)
	err := a.temporalBridge.WithClient(t.Context(), func(c *client.Client) error {
		spaces, err := c.ListSpaces(t.Context(), api.ListSpacesRequest{Limit: 128})
		if err != nil {
			return err
		}
		if spaces.Truncated {
			return errors.New("unexpected space truncation")
		}
		for _, space := range spaces.Items {
			state, err := c.GetMemoryState(t.Context(), api.ReadMemoryStateRequest{Space: space.ID})
			if err != nil {
				return err
			}
			for _, m := range state.Snapshot.Memories {
				if m.Status == memory.StatusPromoted {
					found[m.ID] = m
				}
			}
		}
		return nil
	})
	temporalRequire(t, "read native promoted memory", err)
	if len(found) == 0 {
		t.Fatal("no native promoted memory")
	}
	return found
}

func temporalAdopted(t *testing.T, a *App, selected experience.Context, originals map[atemodel.ObjectID]memory.Memory) {
	t.Helper()
	if len(selected.Memories) == 0 {
		t.Fatal("cross-project execution adopted no memory")
	}
	matched := false
	for _, s := range selected.Memories {
		if m, ok := originals[s.Source.SourceID]; ok && m.Space == s.Source.Anchor.Space {
			matched = true
		}
	}
	if !matched {
		t.Fatal("adoption does not reference first project's promoted memory")
	}
	temporalRequire(t, "verify native execution observations", a.experience.Check(t.Context(), selected))
	t.Logf("native adoption verified: selected=%d original_promoted_source_match=true", len(selected.Memories))
}

func temporalReplay(t *testing.T, cfg config.Config, selected experience.Context) {
	t.Helper()
	a := temporalOpen(t, cfg)
	again, err := a.experience.Prepare(t.Context(), selected.SessionID, selected.TurnID, selected.Goal, nil)
	temporalRequire(t, "replay exact selection without a model", err)
	if !reflect.DeepEqual(selected, again) {
		t.Fatal("reopen changed durable selection")
	}
	temporalRequire(t, "verify native adoption after reopen", a.experience.Check(t.Context(), again))
	temporalNativeCompleted(t, a, again)
	temporalRequire(t, "replay completed maintenance without a model", a.experience.Complete(t.Context(), selected.SessionID, selected.TurnID, nil))
	factory := temporalFactory(t, t.Context(), a, temporalNoModel{}, selected.SessionID)
	completed, found, err := factory.CompletedTurn(t.Context(), session.ID(selected.SessionID), selected.TurnID)
	temporalRequire(t, "restore completed runtime turn", err)
	if !found || completed.Final == nil || completed.ExperienceMaintenance == nil ||
		completed.ExperienceMaintenance.Status != "completed" {
		t.Fatal("reopened runtime lost completed turn or maintenance")
	}
	if _, err := factory.ResumeTurn(t.Context(), session.ID(selected.SessionID), selected.TurnID); err == nil {
		t.Fatal("completed runtime turn became executable after reopen")
	}
	temporalRequire(t, "close replayed App", a.Close())
	t.Log("completed runtime turn and exact experience replayed after reopen without a model")
}

type temporalNoModel struct{}

func (temporalNoModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("replay must not call a model")
}

func (temporalNoModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("replay must not call a model")
}

func TestTemporalStdioRuntimeCrossProjectAndReplay(t *testing.T) {
	binary, shared := temporalBinary(t), t.TempDir()
	base := config.Config{Agent: config.AgentConfig{MaxIterations: 5, MaxOutputTokens: 2048}}
	firstCfg := temporalConfig(t, base, binary, shared)
	secondCfg := temporalConfig(t, base, binary, shared)
	for _, cfg := range []config.Config{firstCfg, secondCfg} {
		temporalRequire(t, "write fixture", os.WriteFile(filepath.Join(cfg.WorkspaceDirectory, "wire.txt"), []byte(temporalLesson), 0o600))
	}
	first := temporalOpen(t, firstCfg)
	_, firstSelection := temporalRun(t, first, &temporalModel{}, "alpha", "Inspect wire.txt and explain the length parser.")
	if len(firstSelection.Memories) != 0 {
		t.Fatal("fresh store was not empty")
	}
	promoted := temporalPromoted(t, first)
	temporalRequire(t, "close first App", first.Close())
	temporalReplay(t, firstCfg, firstSelection)
	second := temporalOpen(t, secondCfg)
	_, selection := temporalRun(t, second, &temporalModel{}, "beta", "Inspect wire.txt for this independent length parser project.")
	temporalAdopted(t, second, selection, promoted)
	temporalRequire(t, "close second App", second.Close())
	temporalReplay(t, secondCfg, selection)
}

// Trace the actual status append without replacing its context or write path.
type temporalMaintenanceTrace struct {
	temporalEvents
	t *testing.T
}

func (s temporalMaintenanceTrace) Append(ctx context.Context, id session.ID, expected uint64, pending ...session.PendingEvent) ([]session.Event, error) {
	start := time.Now()
	deadline, bounded := ctx.Deadline()
	events, err := s.temporalEvents.Append(ctx, id, expected, pending...)
	for _, event := range pending {
		if event.Type != agentruntime.EventExperienceMaintenance {
			continue
		}
		var status agentruntime.ExperienceMaintenance
		if json.Unmarshal(event.Payload, &status) == nil && status.RetryAttempts > 0 {
			s.t.Logf("retry_status=%s bounded=%t append_budget=%s append_elapsed=%s error_class=%s",
				status.Status, bounded, deadline.Sub(start).Round(time.Millisecond),
				time.Since(start).Round(time.Millisecond), temporalErrorClass(err))
		}
	}
	return events, err
}

func TestTemporalStdioMaintenanceRetry(t *testing.T) {
	cfg := temporalConfig(t, config.Config{Agent: config.AgentConfig{MaxIterations: 5, MaxOutputTokens: 2048}}, temporalBinary(t), t.TempDir())
	temporalRequire(t, "write retry fixture", os.WriteFile(filepath.Join(cfg.WorkspaceDirectory, "wire.txt"), []byte(temporalLesson), 0o600))
	a := temporalOpen(t, cfg)
	a.events = temporalMaintenanceTrace{temporalEvents: a.events.(temporalEvents), t: t}
	result, selected := temporalRun(t, a, &temporalModel{failOrganizeOnce: true}, "retry", "Inspect wire.txt and explain the length parser.")
	if result.ExperienceMaintenance.RetryAttempts != 1 {
		t.Fatal("expected exactly one production maintenance retry")
	}
	if len(temporalPromoted(t, a)) != 1 {
		t.Fatal("maintenance recovery or follow-up duplicated the promoted lesson")
	}
	temporalRequire(t, "close recovered App", a.Close())
	temporalReplay(t, cfg, selected)
}

func TestTemporalPendingMaintenanceNativeOwnership(t *testing.T) {
	cfg := temporalConfig(t, config.Config{}, temporalBinary(t), t.TempDir())
	a := temporalOpen(t, cfg)
	own := func(a *App, id session.ID) {
		t.Helper()
		a.stateMu.Lock()
		err := a.ownSession(id)
		a.stateMu.Unlock()
		temporalRequire(t, "own maintenance session", err)
	}
	for _, id := range []session.ID{"a-future", "b-completed", "z-due"} {
		own(a, id)
	}
	appendStatus := func(id session.ID, turn string, status agentruntime.ExperienceMaintenance) session.Event {
		t.Helper()
		events, err := a.events.Append(t.Context(), id, session.AnySequence, session.PendingEvent{
			Type: agentruntime.EventExperienceMaintenance, CorrelationID: turn,
			RuntimeSnapshotID: "original-snapshot", Payload: session.MustJSONPayload(status),
		})
		temporalRequire(t, "append native maintenance", err)
		return events[0]
	}
	future := time.Now().Add(time.Hour)
	appendStatus("a-future", "future", agentruntime.ExperienceMaintenance{Status: "pending", RetryAfter: &future})
	appendStatus("b-completed", "done", agentruntime.ExperienceMaintenance{Status: "pending"})
	appendStatus("b-completed", "done", agentruntime.ExperienceMaintenance{Status: "completed"})
	appendStatus("z-due", "retry", agentruntime.ExperienceMaintenance{Status: "pending"})
	want := appendStatus("z-due", "retry", agentruntime.ExperienceMaintenance{
		Status: "pending", RetryAttempts: 2, Outcome: agentruntime.ExperienceOutcome{Succeeded: true},
	})
	appendStatus("0-unowned", "foreign", agentruntime.ExperienceMaintenance{Status: "pending"})
	check := func(a *App) {
		t.Helper()
		index, ok := a.events.(agentruntime.PendingExperienceMaintenanceStore)
		if !ok {
			t.Fatal("production native event store lacks maintenance discovery")
		}
		events, err := index.PendingExperienceMaintenance(t.Context(), 1)
		temporalRequire(t, "discover due native maintenance", err)
		if len(events) != 1 || !reflect.DeepEqual(events[0], want) {
			t.Fatalf("latest original maintenance envelope changed: got %+v, want %+v", events, want)
		}
		events, err = index.PendingExperienceMaintenance(t.Context(), 10)
		temporalRequire(t, "discover all owned pending maintenance", err)
		if len(events) != 2 || events[0].SessionID != "z-due" || events[1].SessionID != "a-future" {
			t.Fatalf("completed/unowned filtering or due ordering failed: %+v", events)
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := index.PendingExperienceMaintenance(canceled, 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled discovery returned %v", err)
		}
	}
	check(a)
	temporalRequire(t, "close pending maintenance App", a.Close())
	reopened := temporalOpen(t, cfg)
	for _, id := range []session.ID{"a-future", "b-completed", "z-due"} {
		own(reopened, id)
	}
	check(reopened)
	// Even a latest completed record must be decoded before filtering. A bad
	// status must not silently erase a prior durable pending obligation.
	_, err := reopened.events.Append(t.Context(), "b-completed", session.AnySequence, session.PendingEvent{
		Type: agentruntime.EventExperienceMaintenance, CorrelationID: "done",
		Payload: session.MustJSONPayload(map[string]string{"status": "invalid"}),
	})
	temporalRequire(t, "append invalid maintenance fixture", err)
	if _, err := reopened.events.(agentruntime.PendingExperienceMaintenanceStore).
		PendingExperienceMaintenance(t.Context(), 10); err == nil {
		t.Fatal("invalid latest maintenance silently hid pending work")
	}
}

func TestTemporalFailedOpenCleanup(t *testing.T) {
	binary := temporalBinary(t)
	for _, phase := range []string{"launch", "after-temporal"} {
		t.Run(phase, func(t *testing.T) {
			cfg := temporalConfig(t, config.Config{}, binary, t.TempDir())
			options := OpenOptions{}
			if phase == "launch" {
				cfg.ATE.Binary = filepath.Join(t.TempDir(), "missing-ate")
			} else {
				// An invalid binding fails registry activation after openTemporal.
				options.ToolBindings = []tool.Binding{{Descriptor: tool.Descriptor{Name: "temporal_invalid"}}}
			}
			a, err := OpenWithOptions(t.Context(), cfg, options)
			if a != nil {
				_ = a.Close()
			}
			if err == nil {
				t.Fatal("expected failed App construction")
			}
			if phase == "after-temporal" && !strings.Contains(err.Error(), "activate built-in tools") {
				t.Fatal("failure did not occur after temporal initialization")
			}
			cfg.ATE.Binary = binary
			reopened := temporalOpen(t, cfg)
			temporalRequire(t, "reuse store and ownership after failed open", reopened.Close())
		})
	}
}

func TestTemporalRootWorkspaceTools(t *testing.T) {
	cfg := temporalConfig(t, config.Config{}, temporalBinary(t), t.TempDir())
	a := temporalOpen(t, cfg)
	root, _, err := a.EnsureRootThread(t.Context(), "tool-preflight")
	temporalRequire(t, "create tool actor", err)
	pipeline := a.pipeline.WithApprover(approval.NewToolAllowlistApprover("workspace_write", "shell_exec"))
	for i, input := range []struct{ name, args string }{
		{"workspace_write", `{"path":"test_smoke.py","content":"import unittest\nclass Smoke(unittest.TestCase):\n    def test_ok(self):\n        self.assertEqual(1, 1)\n"}`},
		{"workspace_read", `{"path":"test_smoke.py"}`},
		{"workspace_read", `{"path":"test_smoke.py","start_line":1,"end_line":4}`},
		{"workspace_read", `{"path":"test_smoke.py","offset":0}`},
		{"shell_exec", `{"command":"python3 -B -m unittest -v","timeout_ms":10000}`},
	} {
		result, err := pipeline.Execute(t.Context(), tool.Call{
			Actor: root.ActorScope(), SessionID: "tool-preflight", TurnID: "temporal-preflight",
			CallID: fmt.Sprintf("temporal-preflight-%d", i), ToolName: input.name,
			Arguments: json.RawMessage(input.args),
		})
		temporalRequire(t, "real root "+input.name, err)
		if input.name == "shell_exec" {
			var output struct {
				ExitCode *int `json:"exit_code"`
			}
			temporalRequire(t, "decode shell result", json.Unmarshal(result.Data, &output))
			if output.ExitCode == nil || *output.ExitCode != 0 {
				t.Fatal("root shell preflight failed")
			}
		}
	}
	// Reproduce the real provider's incompatible field combination using only
	// synthetic values. Each read mode above works when the other is omitted.
	_, err = pipeline.Execute(t.Context(), tool.Call{
		Actor: root.ActorScope(), SessionID: "tool-preflight", TurnID: "temporal-preflight",
		CallID: "temporal-read-mode-conflict", ToolName: "workspace_read",
		Arguments: json.RawMessage(`{"path":"test_smoke.py","start_line":1,"end_line":4,"offset":0,"max_bytes":16384}`),
	})
	var executionErr *tool.ExecutionError
	if !errors.As(err, &executionErr) || !strings.Contains(err.Error(), "offset must be non-negative and cannot be combined with a line range") {
		t.Fatal("expected executor rejection of simultaneous read modes")
	}
	t.Log("read path-only/line-only/offset-only passed; simultaneous offset and line range reproduced executor rejection")
}

func TestTemporalDirectAuthoritySurvivesCloseAndCleansPendingRequests(t *testing.T) {
	cfg := temporalConfig(t, config.Config{}, temporalBinary(t), t.TempDir())
	a := temporalOpen(t, cfg)
	root, _, err := a.EnsureRootThread(t.Context(), "direct")
	temporalRequire(t, "create authoritative root", err)
	appended, err := a.events.Append(t.Context(), root.SessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventUserMessage, Payload: json.RawMessage(`{"content":"length parser fixture"}`),
	})
	temporalRequire(t, "append authoritative event", err)
	if len(appended) != 1 {
		t.Fatalf("appended events = %d, want 1", len(appended))
	}
	// A successful write is already readable from native authority, without
	// a bridge Sync or a Close-time delivery step.
	before, err := a.authority.sessions.Load(t.Context(), root.SessionID, 0, 100)
	temporalRequire(t, "read native events before close", err)
	if len(before) == 0 || !reflect.DeepEqual(before[len(before)-1], appended[0]) {
		t.Fatal("successful append was not immediately authoritative")
	}
	assertNoPending := func(db *runtimestore.Database) {
		t.Helper()
		pending, err := db.ATERequests().List(t.Context(), aterequest.ListOptions{
			Limit: aterequest.MaxListRecords, MaxBytes: aterequest.MaxListBytes,
		})
		temporalRequire(t, "inspect pending native commands", err)
		if len(pending) != 0 {
			t.Fatalf("acknowledged commands retained %d pending payloads", len(pending))
		}
	}
	assertNoPending(a.database)
	assertRuntimeHasNoMirroredEvents(t, a.config)
	temporalRequire(t, "close authoritative App", a.Close())
	// Inspect the isolated runtime store before App startup can reconcile it.
	db, err := runtimestore.OpenRuntime(t.Context(), a.config.RuntimeDatabasePath())
	temporalRequire(t, "inspect closed runtime database", err)
	t.Cleanup(func() { _ = db.Close() })
	assertNoPending(db)
	assertRuntimeHasNoMirroredEvents(t, a.config)
	temporalRequire(t, "close inspected runtime database", db.Close())

	reopened := temporalOpen(t, cfg)
	again, created, err := reopened.EnsureRootThread(t.Context(), root.SessionID)
	temporalRequire(t, "restore authoritative root", err)
	if created || !reflect.DeepEqual(root, again) {
		t.Fatal("reopen changed or recreated the authoritative root")
	}
	temporalRequire(t, "repeat pending reconciliation", reopened.temporalBridge.RecoverPending(t.Context()))
	after, err := reopened.events.Load(t.Context(), root.SessionID, 0, 100)
	temporalRequire(t, "read authoritative events after reopen", err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reopen or reconciliation changed authoritative event identity or payloads")
	}
	assertNoPending(reopened.database)
	assertRuntimeHasNoMirroredEvents(t, reopened.config)
}

func assertRuntimeHasNoMirroredEvents(t *testing.T, cfg config.Config) {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: cfg.RuntimeDatabasePath(), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	temporalRequire(t, "open runtime schema for inspection", err)
	defer db.Close()
	var count int
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name IN ('session_events', 'orchestration_events', 'temporal_journal')`).Scan(&count)
	temporalRequire(t, "inspect runtime authority boundary", err)
	if count != 0 {
		t.Fatalf("runtime database contains %d mirrored history/outbox tables", count)
	}
	if _, err := os.Stat(cfg.SessionDatabasePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native App created or accessed a legacy session database: %v", err)
	}
}

const temporalLesson = "The fixture wire format uses a two-byte little endian payload length. Reject a truncated header or payload before slicing; do not assume other protocols use the same byte order."

// Only the model is scripted. App, runtime, tools, native authority, bridge and the ATE
// process use production implementations, with citations from actual reads.
type temporalModel struct{ failOrganizeOnce bool }
type temporalNote struct {
	Text string   `json:"text"`
	IDs  []string `json:"event_ids"`
}

func (m *temporalModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stage string
	for _, m := range input {
		if m.Role == schema.System {
			_, after, ok := strings.Cut(m.Content, "\nStage: ")
			if ok {
				stage, _, _ = strings.Cut(after, "\n")
			}
		}
	}
	if stage == "" {
		if last := input[len(input)-1]; last.Role == schema.User && last.Content == "Thanks. No further changes or commands are needed; briefly acknowledge." {
			return schema.AssistantMessage("Acknowledged.", nil), nil
		}
		for _, m := range input {
			if m.Role == schema.Tool {
				if !strings.Contains(m.Content, "little endian") {
					return nil, errors.New("fixture read absent")
				}
				return schema.AssistantMessage("Inspected the two-byte length parser fixture.", nil), nil
			}
		}
		return schema.AssistantMessage("", []schema.ToolCall{{ID: "read-wire", Type: "function", Function: schema.FunctionCall{Name: "workspace_read", Arguments: `{"path":"wire.txt"}`}}}), nil
	}
	raw := []byte(input[len(input)-1].Content)
	var out any
	switch stage {
	case "discover":
		var in struct {
			Directory struct{ Spaces []struct{ ID string } }
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		ids := []string{}
		for _, s := range in.Directory.Spaces {
			ids = append(ids, s.ID)
		}
		var newSpace any
		if len(ids) == 0 {
			newSpace = map[string]string{"name": "Binary parsers", "description": "Scoped binary framing lessons"}
		}
		out = map[string]any{"space_ids": ids, "new_space": newSpace, "search_terms": []string{"length"}, "full_history": false}
	case "history-map":
		var in []struct {
			ID       string `json:"event_id"`
			Kind     string `json:"kind"`
			Original string `json:"original_json"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		notes := []temporalNote{}
		for _, f := range in {
			if f.Kind == string(session.EventToolSucceeded) && strings.Contains(f.Original, "little endian") {
				notes = append(notes, temporalNote{temporalLesson, []string{f.ID}})
			}
		}
		out = map[string]any{"notes": notes}
	case "extract":
		var in struct {
			Notes     []temporalNote
			Coverage  experience.HistoryCoverage
			Published []struct{ Kind, Claim string }
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		if !in.Coverage.Complete {
			return nil, errors.New("no complete primary tool evidence")
		}
		if len(in.Notes) == 0 {
			out = map[string]any{"lessons": []any{}}
			break
		}
		lessons := []any{map[string]any{"claim": temporalLesson, "kind": "fact", "event_ids": in.Notes[0].IDs, "tags": []string{"length"}}}
		for _, prior := range in.Published {
			if prior.Kind == "fact" && prior.Claim == temporalLesson {
				lessons = []any{}
				break
			}
		}
		out = map[string]any{"lessons": lessons}
	case "verify":
		var in struct {
			OriginalEvents []struct{ SourceID, Kind string }
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		ids := []string{}
		for _, e := range in.OriginalEvents {
			if e.Kind == string(session.EventToolSucceeded) {
				ids = append(ids, e.SourceID)
			}
		}
		out = map[string]any{"passed": len(ids) > 0, "reason": "Scoped fixture content was read by the real workspace tool.", "event_ids": ids, "claims_tool_success": true, "tool_success_event_ids": ids}
	case "select":
		var in struct {
			Catalog struct{ Memories []struct{ ID string } }
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		ids := []string{}
		for _, m := range in.Catalog.Memories {
			ids = append(ids, m.ID)
		}
		out = map[string]any{"memory_ids": ids}
	case "organize":
		if m.failOrganizeOnce {
			m.failOrganizeOnce = false
			return nil, context.DeadlineExceeded
		}
		out = map[string]any{"actions": []any{}}
	default:
		return nil, errors.New("unexpected temporal fixture stage")
	}
	encoded, err := json.Marshal(out)
	return schema.AssistantMessage(string(encoded), nil), err
}

func (m *temporalModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}
