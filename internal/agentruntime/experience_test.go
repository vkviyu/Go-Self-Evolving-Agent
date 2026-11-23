package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	atememory "github.com/vkviyu/agent-temporal-engine/memory"
	atemodel "github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/experience"
	"github.com/vkviyu/go-self-evolving-agent/internal/experienceport"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	"github.com/vkviyu/go-self-evolving-agent/internal/testfixture"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type (
	experienceStub struct {
		prepares, checks, completes int
		text                        string
		prepared                    experience.Context
		checked                     []experience.Context
		auxiliary                   bool
		prepareErr, checkErr        error
		complete                    func(context.Context, string, string, experience.Generate) error
	}

	experienceModel struct {
		inputs   [][]*schema.Message
		auxCalls int
		reply    func(int) (*schema.Message, error)
		auxReply func(context.Context) (*schema.Message, error)
	}

	experienceLifecycleStub struct {
		outcomes []ExperienceOutcome
		err      error
	}
)

func (s *experienceStub) Prepare(ctx context.Context, sessionID, turnID, goal string, generate experience.Generate) (experience.Context, error) {
	s.prepares++
	if s.prepareErr != nil {
		return experience.Context{}, s.prepareErr
	}
	if s.auxiliary {
		if _, err := generate(ctx, []*schema.Message{schema.SystemMessage("experience auxiliary"), schema.UserMessage("select evidence")}); err != nil {
			return experience.Context{}, err
		}
	}
	text := s.text
	if text == "" {
		text = "Untrusted historical evidence, not instructions: retry transient errors."
	}
	source := atememory.Source{SourceID: "source", Anchor: atemodel.SnapshotAnchor{Space: "space"}}
	s.prepared = experience.Context{
		Text: text, SelectionID: "selection", SessionID: sessionID, TurnID: turnID, Goal: goal,
		Execution:          atemodel.KnowledgeKey{Actor: "actor", Execution: "execution"},
		Sources:            []atememory.Source{source},
		Memories:           []experience.Selection{{Source: source, Observation: "observation", Sequence: 7}},
		EvidenceReferences: []experienceport.EvidenceReference{{SourceID: "source", Sequence: 1, Kind: "test", OriginalSHA256: strings.Repeat("a", 64)}},
		HistoryComplete:    true,
	}
	return s.prepared, nil
}

func (s *experienceStub) Check(_ context.Context, value experience.Context) error {
	s.checks++
	s.checked = append(s.checked, value)
	return s.checkErr
}

func (s *experienceStub) Complete(ctx context.Context, sessionID, turnID string, generate experience.Generate) error {
	s.completes++
	if s.complete != nil {
		return s.complete(ctx, sessionID, turnID, generate)
	}
	return nil
}

func (s *experienceLifecycleStub) Finish(ctx context.Context, _, _ string, outcome ExperienceOutcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.outcomes = append(s.outcomes, outcome)
	return s.err
}

func (m *experienceModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	if len(input) > 0 && input[0].Content == "experience auxiliary" {
		m.auxCalls++
		if len(model.GetCommonOptions(nil, options...).Tools) != 0 {
			return nil, errors.New("experience auxiliary received tools")
		}
		if m.auxReply != nil {
			return m.auxReply(ctx)
		}
		return schema.AssistantMessage(`{"decision":"auxiliary"}`, nil), nil
	}
	stable, err := cloneJournalInput(input)
	if err != nil {
		return nil, err
	}
	m.inputs = append(m.inputs, stable)
	if m.reply != nil {
		return m.reply(len(m.inputs))
	}
	return schema.AssistantMessage("done", nil), nil
}

func (m *experienceModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestExperienceOrdinaryPromptUsesRecordedAuxiliaryAndCompletes(t *testing.T) {
	ctx, store := t.Context(), memorystore.New()
	service, provider, ledger := &experienceStub{auxiliary: true}, &experienceModel{}, &accountingLedger{}
	f := profileFactory(t, store, provider)
	f.config.Experience, f.config.UsageLedger = service, ledger
	hook := &experienceLifecycleStub{}
	f.config.ExperienceLifecycle = hook
	service.complete = func(ctx context.Context, id, turn string, generate experience.Generate) error {
		journal, err := f.loadTurnJournal(ctx, session.ID(id), turn)
		if err != nil || journal == nil || !journal.Completed {
			return errors.New("maintenance ran before durable turn completion")
		}
		_, err = generate(ctx, []*schema.Message{schema.SystemMessage("experience auxiliary"), schema.UserMessage("review evidence")})
		return err
	}
	turn, err := f.NewTurn(ctx, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil || result.Final.Content != "done" {
		t.Fatalf("ordinary turn: %+v %v", result, err)
	}
	if service.prepares != 1 || service.checks != 1 || service.completes != 1 || provider.auxCalls != 2 {
		t.Fatalf("hooks: %+v, auxiliary=%d", service, provider.auxCalls)
	}
	if len(hook.outcomes) != 1 || !hook.outcomes[0].Succeeded || result.ExperienceMaintenance.Status != "completed" {
		t.Fatalf("completion lifecycle: %+v %+v", hook.outcomes, result.ExperienceMaintenance)
	}
	input := provider.inputs[0]
	if len(input) != 3 || input[0].Role != schema.System || input[1].Role != schema.User ||
		input[1].Content != service.prepared.Text || input[2].Content != "hello" {
		t.Fatalf("experience placement: %+v", input)
	}
	if len(ledger.records) != 3 || len(ledger.requests) != 3 {
		t.Fatalf("unaccounted auxiliary calls: %+v", ledger)
	}
	seen := map[string]bool{}
	for _, record := range ledger.records {
		if seen[record.RequestID] || record.TurnID != turn.ID() || record.Usage.Total == 0 {
			t.Fatalf("invalid usage: %+v", record)
		}
		seen[record.RequestID] = true
	}
	journal, err := f.loadTurnJournal(ctx, "ordinary", turn.ID())
	if err != nil || len(journal.Responses) != 1 || !reflect.DeepEqual(journal.Responses[0].Experience, &service.prepared) {
		t.Fatalf("full experience receipt or auxiliary filtering: %+v %v", journal, err)
	}
	history, err := f.LoadHistory(ctx, "ordinary")
	if err != nil || len(history) != 2 || history[1].Content != "done" {
		t.Fatalf("auxiliary leaked into history: %+v %v", history, err)
	}
	auxRequests, auxResponses := 0, 0
	err = session.VisitEvents(ctx, store, "ordinary", func(event session.Event) error {
		var payload struct {
			Purpose  string            `json:"purpose"`
			Messages []*schema.Message `json:"messages"`
			Message  *schema.Message   `json:"message"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.Purpose == "experience" {
			switch event.Type {
			case session.EventModelRequested:
				if len(payload.Messages) != 2 {
					t.Error("missing auxiliary source request")
				}
				auxRequests++
			case session.EventAssistantMessage:
				if payload.Message == nil {
					t.Error("missing auxiliary source response")
				}
				auxResponses++
			}
		}
		return nil
	})
	if err != nil || auxRequests != 2 || auxResponses != 2 {
		t.Fatalf("source events: %d %d %v", auxRequests, auxResponses, err)
	}
}

func TestExperiencePendingReplayStableAndChecksCurrentPolicy(t *testing.T) {
	ctx, store := t.Context(), memorystore.New()
	service, provider := &experienceStub{auxiliary: true}, &experienceModel{}
	provider.reply = func(n int) (*schema.Message, error) {
		if n == 1 {
			return nil, errors.New("provider interrupted")
		}
		return schema.AssistantMessage("recovered", nil), nil
	}
	f := profileFactory(t, store, provider)
	f.config.Experience = service
	turn, err := f.NewTurn(ctx, "replay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = turn.Run(ctx, []*schema.Message{schema.UserMessage("ordinary request")}, nil); err == nil {
		t.Fatal("expected interruption")
	}
	saved, err := cloneExperienceContext(&service.prepared)
	if err != nil {
		t.Fatal(err)
	}
	service.text = "a newly preferred selection must not replace the pending selection"
	service.checkErr = errors.New("source is now withdrawn")
	resume, err := f.ResumeTurn(ctx, "replay", turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resume.Resume(ctx, nil); !errors.Is(err, service.checkErr) {
		t.Fatalf("prepared replay bypassed Check: %v", err)
	}
	if len(provider.inputs) != 1 || service.prepares != 1 || service.checks != 2 || provider.auxCalls != 1 {
		t.Fatalf("replay reselected or dispatched: inputs=%d service=%+v", len(provider.inputs), service)
	}
	service.checkErr = nil
	resume, err = f.ResumeTurn(ctx, "replay", turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	result, err := resume.Resume(ctx, nil)
	if err != nil || result.Final.Content != "recovered" {
		t.Fatalf("resume: %+v %v", result, err)
	}
	if service.prepares != 1 || service.checks != 3 || service.completes != 1 ||
		rawRequestDigest(provider.inputs[0]) != rawRequestDigest(provider.inputs[1]) ||
		!reflect.DeepEqual(service.checked[2], *saved) {
		t.Fatal("pending replay lost exact selection, evidence, prompt, or completion hook")
	}
}

func TestExperienceCachedResponseReplayAndToolLoopReuse(t *testing.T) {
	ctx, store := t.Context(), memorystore.New()
	service, provider := &experienceStub{}, &experienceModel{}
	provider.reply = func(n int) (*schema.Message, error) {
		switch n {
		case 1:
			return schema.AssistantMessage("", []schema.ToolCall{{ID: "once", Type: "function", Function: schema.FunctionCall{Name: "echo", Arguments: `{}`}}}), nil
		case 2:
			return nil, errors.New("interrupted after tool")
		}
		return schema.AssistantMessage("resumed tool loop", nil), nil
	}
	executed := 0
	f := profileFactory(t, store, provider, profileBinding("echo", tool.RiskPure, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		executed++
		return json.RawMessage(`{"value":"result"}`), nil
	}))
	f.config.Experience = service
	turn, err := f.NewTurn(ctx, "tools")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = turn.Run(ctx, []*schema.Message{schema.UserMessage("use echo")}, nil); err == nil {
		t.Fatal("expected interruption")
	}
	resumed, err := f.ResumeTurn(ctx, "tools", turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resumed.Resume(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if executed != 1 || service.prepares != 1 || service.checks != 3 || len(provider.inputs) != 3 {
		t.Fatalf("cached responses repeated work: tools=%d prepares=%d checks=%d requests=%d", executed, service.prepares, service.checks, len(provider.inputs))
	}
	for _, input := range provider.inputs {
		count := 0
		for i, message := range input {
			if message.Content == service.prepared.Text {
				count++
			}
			if message.Role == schema.Tool && (i == 0 || len(input[i-1].ToolCalls) == 0) {
				t.Fatal("experience split a tool response group")
			}
		}
		if count != 1 {
			t.Fatalf("duplicated experience context: %d", count)
		}
	}
	if rawRequestDigest(provider.inputs[1]) != rawRequestDigest(provider.inputs[2]) {
		t.Fatal("pending tool-loop request changed on replay")
	}
}

func TestExperienceEveryDispatchChecksIncludingAfterCachedResponse(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[streaming], func(t *testing.T) {
			ctx, store := t.Context(), memorystore.New()
			service, provider := &experienceStub{}, &experienceModel{}
			f := profileFactory(t, store, provider)
			f.config.Experience = service
			m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "check", turnID: "turn"}
			input := []*schema.Message{schema.SystemMessage("system"), schema.UserMessage("request")}
			if _, err := m.Generate(ctx, input); err != nil {
				t.Fatal(err)
			}
			first := m.receipt
			replayed := &recordingModel{inner: provider, factory: f, events: store, sessionID: "check", turnID: "turn",
				replay: &journalReplay{Responses: []modelReceipt{first}}}
			service.checkErr = errors.New("current policy denies reuse")
			if _, err := replayed.Generate(ctx, input); err != nil {
				t.Fatalf("cached response required fresh policy or retrieval: %v", err)
			}
			next := append(append([]*schema.Message(nil), input...), first.Response, schema.UserMessage("continue"))
			var err error
			if streaming {
				_, err = replayed.Stream(ctx, next)
			} else {
				_, err = replayed.Generate(ctx, next)
			}
			if !errors.Is(err, service.checkErr) || service.prepares != 1 || service.checks != 2 || len(provider.inputs) != 1 {
				t.Fatalf("current policy bypass: err=%v prepares=%d checks=%d dispatched=%d", err, service.prepares, service.checks, len(provider.inputs))
			}
		})
	}
}

func TestExperienceCapacityAndPrepareErrorsFailClosed(t *testing.T) {
	for _, scenario := range []string{"capacity", "bytes", "prepare", "profile", "auxiliary-profile"} {
		t.Run(scenario, func(t *testing.T) {
			store, provider := memorystore.New(), &experienceModel{}
			service := &experienceStub{}
			f := profileFactory(t, store, provider)
			f.tokenizer = tokenizer.NewApproximate("experience")
			f.config.Experience = service
			switch scenario {
			case "capacity":
				f.config.ContextWindowTokens, f.config.MaxOutputTokens = 1024, 100
				service.text = strings.Repeat("historical experience ", 900)
			case "bytes":
				service.text = strings.Repeat("x", experience.MaxContextBytes+1)
			case "prepare":
				service.prepareErr = errors.New("ATE unavailable")
			case "profile", "auxiliary-profile":
				profile := runtimeTestProfile()
				profile.Limits.MaxInputTokens = 1
				var err error
				f, err = f.WithProfile(t.Context(), runtimeTestScope(t), "bounded", profile)
				if err != nil {
					t.Fatal(err)
				}
				service.auxiliary = scenario == "auxiliary-profile"
			}
			m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "bounded", turnID: "turn"}
			_, err := m.Generate(t.Context(), []*schema.Message{schema.UserMessage("hello")})
			if err == nil || len(provider.inputs) != 0 || provider.auxCalls != 0 {
				t.Fatalf("failed open: %v calls=%d auxiliary=%d", err, len(provider.inputs), provider.auxCalls)
			}
			if scenario == "capacity" && !errors.Is(err, ErrContextCapacity) {
				t.Fatalf("capacity error=%v", err)
			}
		})
	}
}

func TestExperienceCompletionFailureAndDeadlineStayPending(t *testing.T) {
	for _, scenario := range []string{"error", "deadline", "cancelled", "lifecycle"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
			service, hook := &experienceStub{}, &experienceLifecycleStub{}
			f := profileFactory(t, store, provider)
			f.config.Experience, f.config.ExperienceLifecycle = service, hook
			f.config.ExperienceTimeout = 20 * time.Millisecond
			service.complete = func(ctx context.Context, _, _ string, _ experience.Generate) error {
				if scenario == "deadline" {
					<-ctx.Done()
					return ctx.Err()
				}
				return errors.New("experience maintenance unavailable")
			}
			if scenario == "lifecycle" {
				hook.err = errors.New("typed execution unavailable")
			}
			if scenario == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				f.config.ExperienceLifecycle = experienceLifecycleFunc(func(context.Context, string, string, ExperienceOutcome) error {
					cancel()
					return context.Canceled
				})
			}
			turn, err := f.NewTurn(ctx, "maintenance")
			if err != nil {
				t.Fatal(err)
			}
			result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("finish normally")}, nil)
			if err != nil || result.Final.Content != "done" || result.ExperienceMaintenance.Status != "pending" || result.ExperienceMaintenance.Error == "" {
				t.Fatalf("maintenance changed turn outcome: %+v %v", result, err)
			}
			journal, err := f.loadTurnJournal(t.Context(), "maintenance", turn.ID())
			if err != nil || !journal.Completed {
				t.Fatalf("lost durable completion: %+v %v", journal, err)
			}
			completed, found, err := f.CompletedTurn(t.Context(), "maintenance", turn.ID())
			if err != nil || !found || completed.ExperienceMaintenance == nil || completed.ExperienceMaintenance.Status != "pending" {
				t.Fatalf("durable maintenance status missing from completed turn: %+v %v", completed, err)
			}
			if _, err := f.ResumeTurn(t.Context(), "maintenance", turn.ID()); err == nil {
				t.Fatal("maintenance failure made turn executable again")
			}
			failures := 0
			_ = session.VisitEvents(t.Context(), store, "maintenance", func(event session.Event) error {
				if event.Type == session.EventTurnFailed {
					failures++
				}
				return nil
			})
			if failures != 0 {
				t.Fatal("completed turn relabeled failed")
			}
			service.complete, hook.err = nil, nil
			f.config.ExperienceLifecycle = hook
			next, err := f.NewTurn(t.Context(), "maintenance")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := next.Run(t.Context(), []*schema.Message{schema.UserMessage("next normal prompt")}, nil); err != nil {
				t.Fatal(err)
			}
			var last ExperienceMaintenance
			_ = session.VisitEvents(t.Context(), store, "maintenance", func(event session.Event) error {
				if event.Type == EventExperienceMaintenance && event.CorrelationID == turn.ID() {
					return json.Unmarshal(event.Payload, &last)
				}
				return nil
			})
			if last.Status != "completed" || len(provider.inputs) != 2 || service.prepares != 2 {
				t.Fatalf("unfinished maintenance not retried independently: %+v", last)
			}
		})
	}
}

type experienceLifecycleFunc func(context.Context, string, string, ExperienceOutcome) error

func (f experienceLifecycleFunc) Finish(ctx context.Context, sessionID, turnID string, outcome ExperienceOutcome) error {
	return f(ctx, sessionID, turnID, outcome)
}

func TestExperienceFailedAndCancelledTurnsCallLifecycle(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "cancelled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store, service, hook := memorystore.New(), &experienceStub{}, &experienceLifecycleStub{}
			provider := &experienceModel{reply: func(int) (*schema.Message, error) {
				if canceled {
					cancel()
					return nil, context.Canceled
				}
				return nil, errors.New("provider failed")
			}}
			f := profileFactory(t, store, provider)
			f.config.Experience, f.config.ExperienceLifecycle = service, hook
			turn, err := f.NewTurn(ctx, "failed")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("attempt")}, nil); err == nil {
				t.Fatal("expected failure")
			}
			if len(hook.outcomes) != 1 || hook.outcomes[0].Succeeded || !hook.outcomes[0].Recoverable || service.completes != 0 {
				t.Fatalf("failure lifecycle: %+v complete=%d", hook.outcomes, service.completes)
			}
		})
	}
}

func TestExperienceChildConfigInheritance(t *testing.T) {
	ctx, store := t.Context(), memorystore.New()
	service, hook := &experienceStub{}, &experienceLifecycleStub{}
	provider := &experienceModel{}
	parent := profileFactory(t, store, provider)
	parent.config.Experience, parent.config.ExperienceLifecycle = service, hook
	parent.config.ExperienceTimeout = time.Second
	scope := runtimeTestScope(t)
	child, err := parent.WithProfile(ctx, scope, "child-experience", runtimeTestProfile())
	if err != nil {
		t.Fatal(err)
	}
	if child.config.Experience != service || child.config.ExperienceLifecycle != hook || child.config.ExperienceTimeout != time.Second {
		t.Fatal("child dropped experience configuration")
	}
	turn, err := child.NewTurn(ctx, "child-experience")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("child prompt")}, nil); err != nil {
		t.Fatal(err)
	}
	if service.prepared.SessionID != "child-experience" || service.prepared.Goal != "child prompt" || service.completes != 1 {
		t.Fatalf("child hooks used wrong scope: %+v", service)
	}
}

type (
	runtimeExperienceServiceModel struct {
		experienceModel
		stages map[string]int
	}
)

func (m *runtimeExperienceServiceModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	if len(input) == 2 && strings.Contains(input[0].Content, "\nStage: ") {
		stage := strings.Split(strings.Split(input[0].Content, "\nStage: ")[1], "\n")[0]
		m.stages[stage]++
		if len(model.GetCommonOptions(nil, options...).Tools) != 0 {
			return nil, errors.New("curation received ordinary tools")
		}
		switch stage {
		case "discover":
			return schema.AssistantMessage(`{"space_ids":[],"new_space":null,"search_terms":[],"full_history":false}`, nil), nil
		case "history-map", "history-reduce":
			return schema.AssistantMessage(`{"notes":[]}`, nil), nil
		case "extract":
			return schema.AssistantMessage(`{"lessons":[]}`, nil), nil
		default:
			return nil, errors.New("unexpected experience stage: " + stage)
		}
	}
	return m.experienceModel.Generate(ctx, input, options...)
}

func (m *runtimeExperienceServiceModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.Generate(ctx, input, options...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func TestExperienceActualServiceRootResumeAndTypedCompletion(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "experience.db")
	db, err := testfixture.Open(t, ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state := testfixture.State{Authority: db.Authority}
	bridge, err := atebridge.New(atebridge.Options{
		Process: db.Process, Pending: db.ATERequests(), State: state, Namespace: "runtime-experience",
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := experience.New(experience.Options{Bridge: bridge, State: state, History: db.SessionEvents()})
	if err != nil {
		t.Fatal(err)
	}
	provider := &runtimeExperienceServiceModel{stages: map[string]int{}}
	provider.reply = func(n int) (*schema.Message, error) {
		if n == 1 {
			return nil, errors.New("interrupted main model")
		}
		return schema.AssistantMessage("native experience resumed", nil), nil
	}
	base := profileFactory(t, memorystore.New(), provider)
	base.config.Experience = service
	base.config.ExperienceLifecycle = experienceLifecycleFunc(func(ctx context.Context, sessionID, turnID string, outcome ExperienceOutcome) error {
		if err := bridge.RecoverPending(ctx); err != nil {
			return err
		}
		if outcome.Recoverable {
			return nil
		}
		return bridge.CompleteExecution(ctx, sessionID, turnID, outcome.Succeeded)
	})
	f, err := NewFactory(base.config, provider, base.registry, base.pipeline, db.SessionEvents(), db.Checkpoints())
	if err != nil {
		t.Fatal(err)
	}
	turn, err := f.NewTurn(ctx, "native-root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("ordinary greeting")}, nil); err == nil {
		t.Fatal("expected main provider interruption")
	}
	resumed, err := f.ResumeTurn(ctx, "native-root", turn.ID())
	if err != nil {
		t.Fatal(err)
	}
	result, err := resumed.Resume(ctx, nil)
	if err != nil || result.Final.Content != "native experience resumed" ||
		result.ExperienceMaintenance == nil || result.ExperienceMaintenance.Status != "completed" {
		t.Fatalf("native resumed completion: %+v %v", result, err)
	}
	if provider.stages["discover"] != 1 || provider.stages["history-map"] == 0 || provider.stages["extract"] != 1 ||
		len(provider.inputs) != 2 || rawRequestDigest(provider.inputs[0]) != rawRequestDigest(provider.inputs[1]) {
		t.Fatalf("native service calls or replay: stages=%v calls=%d", provider.stages, len(provider.inputs))
	}
	if err := bridge.CompleteExecution(ctx, "native-root", turn.ID(), true); err != nil {
		t.Fatalf("recoverable failure conflicted with native success: %v", err)
	}
	history, err := f.LoadHistory(ctx, "native-root")
	if err != nil || len(history) != 2 {
		t.Fatalf("native auxiliary messages polluted history: %+v %v", history, err)
	}
}

func TestExperiencePendingReplayCannotBypassConfigurationOrCapacity(t *testing.T) {
	for _, scenario := range []string{"disabled", "capacity", "missing-context", "cached-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
			service := &experienceStub{text: strings.Repeat("untrusted historical evidence ", 100)}
			f := profileFactory(t, store, provider)
			f.tokenizer = tokenizer.NewApproximate("experience")
			f.config.Experience = service
			original := &recordingModel{inner: provider, factory: f, events: store, sessionID: "guard", turnID: "turn"}
			input := []*schema.Message{schema.UserMessage("hello")}
			if _, err := original.Generate(ctx, input); err != nil {
				t.Fatal(err)
			}
			receipt := original.receipt
			replayed := &recordingModel{inner: provider, factory: f, events: store, sessionID: "guard", turnID: "turn",
				replay: &journalReplay{Pending: &receipt}}
			switch scenario {
			case "disabled":
				f.config.Experience = nil
			case "capacity":
				f.config.ContextWindowTokens = 256
			case "missing-context":
				index := *receipt.ExperienceIndex
				receipt.Input = append(receipt.Input[:index:index], receipt.Input[index+1:]...)
				receipt.Experience, receipt.ExperienceIndex = nil, nil
			case "cached-disabled":
				f.config.Experience = nil
				replayed.replay = &journalReplay{Responses: []modelReceipt{receipt}}
				if _, err := replayed.Generate(ctx, input); err != nil {
					t.Fatal(err)
				}
				input = append(input, receipt.Response, schema.UserMessage("continue"))
			}
			if _, err := replayed.Generate(ctx, input); err == nil {
				t.Fatal("prepared dispatch bypassed required experience or current capacity")
			}
			if len(provider.inputs) != 1 || service.prepares != 1 {
				t.Fatal("replay dispatched or reselected")
			}
		})
	}
}

type experienceStatusStore struct {
	session.EventStore
	reject bool
}

func (s *experienceStatusStore) Append(ctx context.Context, id session.ID, sequence uint64, events ...session.PendingEvent) ([]session.Event, error) {
	if s.reject {
		for _, event := range events {
			if event.Type != EventExperienceMaintenance {
				continue
			}
			var status ExperienceMaintenance
			if err := json.Unmarshal(event.Payload, &status); err != nil {
				return nil, err
			}
			if status.Status == "completed" {
				return nil, errors.New("maintenance status storage unavailable")
			}
		}
	}
	return s.EventStore.Append(ctx, id, sequence, events...)
}

func TestExperienceMaintenanceStatusFailureRetainsAtomicPendingJob(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service := &experienceStub{}
	f := profileFactory(t, store, provider)
	f.config.Experience = service
	failing := &experienceStatusStore{EventStore: store, reject: true}
	f.events = failing
	turn, err := f.NewTurn(ctx, "status")
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("hello")}, nil)
	if err != nil || result.Final.Content != "done" || result.ExperienceMaintenance.Status != "pending" {
		t.Fatalf("status append failure altered outcome: %+v %v", result, err)
	}
	completed, found, err := f.CompletedTurn(ctx, "status", turn.ID())
	if err != nil || !found || completed.ExperienceMaintenance.Status != "pending" {
		t.Fatalf("atomic pending job was lost: %+v %v", completed, err)
	}
	failing.reject = false
	next, err := f.NewTurn(ctx, "status")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Run(ctx, []*schema.Message{schema.UserMessage("continue")}, nil); err != nil {
		t.Fatal(err)
	}
	if service.completes != 3 || len(provider.inputs) != 2 {
		t.Fatalf("maintenance retried execution: complete=%d dispatch=%d", service.completes, len(provider.inputs))
	}
}

func seedExperienceMaintenance(t *testing.T, store session.EventStore, id session.ID, turnID string) {
	t.Helper()
	_, err := store.Append(t.Context(), id, session.AnySequence, session.PendingEvent{
		Type: EventExperienceMaintenance, CorrelationID: turnID,
		Payload: session.MustJSONPayload(ExperienceMaintenance{Status: "pending", Outcome: ExperienceOutcome{Succeeded: true}}),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func latestExperienceMaintenance(t *testing.T, store session.EventStore, id session.ID, turnID string) ExperienceMaintenance {
	t.Helper()
	var status ExperienceMaintenance
	if err := session.VisitEvents(t.Context(), store, id, func(event session.Event) error {
		if event.Type == EventExperienceMaintenance && event.CorrelationID == turnID {
			return json.Unmarshal(event.Payload, &status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestExperienceFailedRetriesAllowNewTurnsWithDurableBackoff(t *testing.T) {
	for _, failure := range []error{experience.ErrIncomplete, experience.ErrDecision, errors.New("maintenance unavailable")} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
			service := &experienceStub{complete: func(context.Context, string, string, experience.Generate) error {
				return failure
			}}
			f := profileFactory(t, store, provider)
			f.config.Experience = service
			for _, id := range []string{"old-a", "old-b", "old-c"} {
				seedExperienceMaintenance(t, store, "retry", id)
			}
			for iteration := range 3 {
				// Recreate the recorder: backoff must survive process-local state.
				m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "retry", turnID: "new"}
				if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("new ordinary task")}); err != nil {
					t.Fatalf("old maintenance blocked new retrieval: %v", err)
				}
				if service.completes != iteration {
					t.Fatal("foreground preparation executed maintenance")
				}
				m.runExperienceRetry(ctx)
				m.runExperienceRetry(ctx) // An admission may execute only once.
				want := min(3, (iteration+1)*maxExperienceRetryJobs)
				if service.completes != want || service.prepares != iteration+1 || service.checks != iteration+1 {
					t.Fatalf("unbounded retry or missing new gate: complete=%d prepare=%d check=%d", service.completes, service.prepares, service.checks)
				}
			}
			for _, id := range []string{"old-a", "old-b", "old-c"} {
				status := latestExperienceMaintenance(t, store, "retry", id)
				if status.Status != "pending" || status.Error != failure.Error() || status.RetryAttempts != 1 ||
					status.RetryAfter == nil || !status.RetryAfter.After(time.Now()) {
					t.Fatalf("lost failure or durable cooldown: %+v", status)
				}
			}
			// A fresh Prepare failure is still fatal even while old jobs back off.
			service.prepareErr = errors.New("new retrieval failed")
			m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "retry", turnID: "new-failure"}
			if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("new task")}); !errors.Is(err, service.prepareErr) {
				t.Fatalf("new preparation failed open: %v", err)
			}
			if len(provider.inputs) != 3 {
				t.Fatal("new preparation failure reached provider")
			}
		})
	}
}

func TestExperienceRetryDeadlineDoesNotCancelPreparation(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service := &experienceStub{complete: func(ctx context.Context, _, _ string, _ experience.Generate) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	f := profileFactory(t, store, provider)
	f.config.Experience, f.config.ExperienceTimeout = service, 20*time.Millisecond
	seedExperienceMaintenance(t, store, "deadline", "old-a")
	seedExperienceMaintenance(t, store, "deadline", "old-b")
	m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "deadline", turnID: "new"}
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("new task")}); err != nil {
		t.Fatal(err)
	}
	if service.completes != 0 {
		t.Fatal("foreground preparation executed maintenance")
	}
	m.runExperienceRetry(ctx)
	if ctx.Err() != nil {
		t.Fatal("retry deadline cancelled the caller")
	}
	status := latestExperienceMaintenance(t, store, "deadline", "old-a")
	if service.completes != 1 || service.prepares != 1 || status.Error != context.DeadlineExceeded.Error() || status.Status != "pending" {
		t.Fatalf("retry timeout blocked preparation or launched more work: %+v completes=%d", status, service.completes)
	}
}

func TestExperienceRetryParentCancellationStopsPreparation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, provider := memorystore.New(), &experienceModel{}
	service := &experienceStub{complete: func(context.Context, string, string, experience.Generate) error {
		cancel()
		return context.Canceled
	}}
	f := profileFactory(t, store, provider)
	f.config.Experience = service
	seedExperienceMaintenance(t, store, "cancel", "old")
	m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "cancel", turnID: "new"}
	if _, err := m.Generate(ctx, []*schema.Message{schema.UserMessage("new task")}); err != nil {
		t.Fatal(err)
	}
	m.runExperienceRetry(ctx)
	next := &recordingModel{inner: provider, factory: f, events: store, sessionID: "cancel", turnID: "next"}
	if _, err := next.Generate(ctx, []*schema.Message{schema.UserMessage("next task")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation ignored: %v", err)
	}
	if service.prepares != 1 || len(provider.inputs) != 1 {
		t.Fatal("preparation ran after parent cancellation")
	}
	status := latestExperienceMaintenance(t, store, "cancel", "old")
	if status.Error != context.Canceled.Error() || status.Status != "pending" || status.RetryAfter == nil {
		t.Fatalf("cancellation lost pending failure: %+v", status)
	}
}

func TestExperienceReceivesFullOpeningGoalAndRejectsOversize(t *testing.T) {
	for _, size := range []int{4096, maxExperienceGoalBytes, maxExperienceGoalBytes + 1} {
		store, provider, service := memorystore.New(), &experienceModel{}, &experienceStub{}
		f := profileFactory(t, store, provider)
		f.config.Experience = service
		goal := strings.Repeat("a", size-4) + "TAIL"
		m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "goal", turnID: "turn", openingInput: goal}
		err := m.prepareExperience(t.Context(), []*schema.Message{schema.UserMessage("later user-role context")})
		if size > maxExperienceGoalBytes {
			if err == nil || service.prepares != 0 {
				t.Fatal("oversized goal was silently shortened or accepted")
			}
		} else if err != nil || service.prepared.Goal != goal {
			t.Fatalf("opening goal changed at size %d: %v", size, err)
		}
	}
}

func TestExperienceMaintenanceDefaultAndOverrideDeadlines(t *testing.T) {
	for _, timeout := range []time.Duration{0, 5 * time.Minute} {
		store, provider, service := memorystore.New(), &experienceModel{}, &experienceStub{}
		f := profileFactory(t, store, provider)
		f.config.Experience, f.config.ExperienceTimeout = service, timeout
		service.complete = func(ctx context.Context, _, _ string, _ experience.Generate) error {
			deadline, ok := ctx.Deadline()
			expected := timeout
			if expected == 0 {
				expected = 2 * time.Minute
			}
			remaining := time.Until(deadline)
			if !ok || remaining > expected || remaining < expected-time.Second {
				t.Errorf("maintenance deadline: %v, want %v", remaining, expected)
			}
			return nil
		}
		m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "timeout", turnID: "turn"}
		if status := m.maintainExperience(t.Context(), ExperienceOutcome{Succeeded: true}); status.Status != "completed" {
			t.Fatalf("maintenance failed: %+v", status)
		}
	}
}

func TestExperienceSlowRetryRunsAfterDurableTurn(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service := &experienceStub{}
	f := profileFactory(t, store, provider)
	f.config.Experience, f.config.ExperienceTimeout = service, 10*time.Second
	seedExperienceMaintenance(t, store, "slow-retry", "old")
	turn, err := f.NewTurn(ctx, "slow-retry")
	if err != nil {
		t.Fatal(err)
	}
	service.complete = func(ctx context.Context, id, turnID string, generate experience.Generate) error {
		if turnID != "old" {
			return nil
		}
		journal, err := f.loadTurnJournal(ctx, session.ID(id), turn.ID())
		if err != nil || journal == nil || !journal.Completed {
			t.Fatal("retry ran before durable foreground completion")
		}
		_, err = generate(ctx, []*schema.Message{schema.SystemMessage("experience auxiliary"), schema.UserMessage("slow review")})
		return err
	}
	provider.auxReply = func(ctx context.Context) (*schema.Message, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= experienceRetryBudget {
			t.Fatal("retry inherited the foreground admission deadline")
		}
		timer := time.NewTimer(experienceRetryBudget + 100*time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return schema.AssistantMessage("slow review completed", nil), nil
		}
	}
	result, err := turn.Run(ctx, []*schema.Message{schema.UserMessage("new task")}, nil)
	if err != nil || result.Final.Content != "done" {
		t.Fatalf("foreground result: %+v %v", result, err)
	}
	status := latestExperienceMaintenance(t, store, "slow-retry", "old")
	if status.Status != "completed" || status.RetryAttempts != 1 || provider.auxCalls != 1 {
		t.Fatalf("slow retry never completed: %+v calls=%d", status, provider.auxCalls)
	}
}

func TestExperienceCompactionReservesExactSelectionAndReplays(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &contextLoopModel{}
	provider.calls.Store(2) // Return a final answer rather than a tool call.
	service := &experienceStub{text: strings.Repeat("Untrusted unique evidence αβ ", 170)}
	f := profileFactory(t, store, provider)
	f.tokenizer = tokenizer.NewApproximate("experience")
	f.config.Experience = service
	f.config.ContextWindowTokens, f.config.MaxOutputTokens = 5000, 256
	limit := modelcontext.InputLimit(5000, 256)
	f.config.CompactionTriggerTokens = limit
	m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "compact", turnID: "turn"}
	input := []*schema.Message{schema.SystemMessage("host policy")}
	for range 8 {
		input = append(input, schema.UserMessage(strings.Repeat("old question ", 65)),
			schema.AssistantMessage(strings.Repeat("old findings ", 65), nil))
	}
	input = append(input, schema.UserMessage("preserve this current goal"))
	if err := m.prepareExperience(ctx, input); err != nil {
		t.Fatal(err)
	}
	withExperience, _, err := m.experienceInput(input)
	if err != nil || m.countRequestInput(input, nil) >= limit || m.countRequestInput(withExperience, nil) <= limit {
		t.Fatalf("fixture must fit only without experience: base=%d full=%d limit=%d err=%v",
			m.countRequestInput(input, nil), m.countRequestInput(withExperience, nil), limit, err)
	}
	if _, err := m.Generate(ctx, input); err != nil {
		t.Fatal(err)
	}
	if provider.summaries.Load() != 1 || m.receipt.Experience.Text != service.text {
		t.Fatal("compaction skipped or changed selected bytes")
	}
	if err := m.checkDispatchCapacity(provider.inputs[0], nil); err != nil {
		t.Fatal(err)
	}
	receipt := m.receipt
	service.text = "replacement selection must not be used"
	replay := &recordingModel{inner: provider, factory: f, events: store, sessionID: "compact", turnID: "turn",
		replay: &journalReplay{Pending: &receipt}}
	if _, err := replay.Generate(ctx, input); err != nil {
		t.Fatal(err)
	}
	if provider.summaries.Load() != 1 || service.prepares != 1 ||
		rawRequestDigest(provider.inputs[0]) != rawRequestDigest(provider.inputs[1]) {
		t.Fatal("pending replay changed the compacted prompt or repeated retrieval")
	}
}

type experienceSyncStub struct {
	*experienceStub
	syncs int
	sync  func(context.Context) error
}

func (s *experienceSyncStub) Sync(ctx context.Context) error {
	s.syncs++
	if s.sync != nil {
		return s.sync(ctx)
	}
	return nil
}

func TestExperienceSyncDispatchReplayAndMaintenance(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "generate", true: "stream"}[streaming], func(t *testing.T) {
			ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
			service := &experienceSyncStub{experienceStub: &experienceStub{}}
			f := profileFactory(t, store, provider)
			f.config.Experience = service
			m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "sync", turnID: "turn"}
			input := []*schema.Message{schema.UserMessage("hello")}
			if _, err := m.Generate(ctx, input); err != nil {
				t.Fatal(err)
			}
			receipt := m.receipt
			failure := errors.New("source synchronization unavailable")
			service.sync = func(context.Context) error { return failure }
			cached := &recordingModel{inner: provider, factory: f, events: store, sessionID: "sync", turnID: "turn",
				replay: &journalReplay{Responses: []modelReceipt{receipt}}}
			if _, err := cached.Generate(ctx, input); err != nil || service.syncs != 1 {
				t.Fatalf("cached response synchronized sources: %v syncs=%d", err, service.syncs)
			}
			pending := &recordingModel{inner: provider, factory: f, events: store, sessionID: "sync", turnID: "turn",
				replay: &journalReplay{Pending: &receipt}}
			var err error
			if streaming {
				_, err = pending.Stream(ctx, input)
			} else {
				_, err = pending.Generate(ctx, input)
			}
			if !errors.Is(err, failure) || len(provider.inputs) != 1 || service.syncs != 2 || service.prepares != 1 {
				t.Fatalf("pending dispatch ignored source sync: %v syncs=%d", err, service.syncs)
			}
			service.sync = func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second || ctx.Err() != nil {
					t.Fatal("maintenance sync lacks its own live bounded context")
				}
				return nil
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			service.complete = func(ctx context.Context, _, _ string, _ experience.Generate) error { return ctx.Err() }
			status := m.maintainExperience(cancelled, ExperienceOutcome{Succeeded: true})
			if service.syncs != 3 || status.Status != "pending" || !strings.Contains(status.Error, context.Canceled.Error()) {
				t.Fatalf("cancelled maintenance skipped source sync: %+v syncs=%d", status, service.syncs)
			}
			f.config.ExperienceTimeout = 10 * time.Millisecond
			service.sync = func(ctx context.Context) error { <-ctx.Done(); return nil }
			if err := m.syncExperience(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("sync deadline ignored: %v", err)
			}
		})
	}
}

func TestExperienceSuspensionSyncsAfterCheckpoint(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service := &experienceSyncStub{experienceStub: &experienceStub{}}
	f := profileFactory(t, store, provider)
	f.config.Experience = service
	turn, err := f.NewTurn(ctx, "suspend")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, turn.checkpointID, []byte("checkpoint")); err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(ctx, "suspend", session.AnySequence, session.PendingEvent{
		Type: session.EventTurnStarted, CorrelationID: turn.ID(),
		Payload: session.MustJSONPayload(map[string]any{"journal": turnJournal{
			Version: 1, Input: []*schema.Message{schema.UserMessage("wait")}, Prepared: true,
		}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.sync = func(ctx context.Context) error {
		_, found, err := f.checkpointReplayOffset(ctx, "suspend", turn.ID(), []byte("checkpoint"))
		if err != nil || !found {
			t.Fatalf("sync preceded durable checkpoint: %v", err)
		}
		return nil
	}
	scope := runtimeTestScope(t)
	target, _ := orchestration.NewAgentID()
	encoded, err := orchestration.EncodeWaitInterrupt(orchestration.WaitSet{
		TeamID: scope.TeamID, AgentID: scope.AgentID, TargetAgentIDs: []orchestration.AgentID{target},
		Condition: "completed", ToolCallID: "wait-call", ResumeToken: "resume", IdempotencyKey: "once",
	})
	if err != nil {
		t.Fatal(err)
	}
	iterator, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	generator.Send(&adk.AgentEvent{Action: &adk.AgentAction{Interrupted: &adk.InterruptInfo{
		InterruptContexts: []*adk.InterruptCtx{{Info: encoded}},
	}}})
	generator.Close()
	_, err = turn.consumeIterator(ctx, iterator, nil, false)
	if _, ok := errors.AsType[*orchestration.WaitSuspendedError](err); !ok || service.syncs != 1 || service.completes != 0 {
		t.Fatalf("suspension hooks: %v syncs=%d completes=%d", err, service.syncs, service.completes)
	}
}

func TestExperienceFailureMaintenanceCannotStarveStrategy(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service := &experienceStub{}
	f := profileFactory(t, store, provider)
	f.config.Experience, f.config.ExperienceTimeout = service, 20*time.Millisecond
	strategyCalled, lifecycleCalled := false, false
	f.config.ExperienceLifecycle = experienceLifecycleFunc(func(ctx context.Context, _, _ string, _ ExperienceOutcome) error {
		lifecycleCalled = true
		if !strategyCalled {
			t.Error("maintenance ran before required strategy fallback")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	turn, err := f.NewTurn(ctx, "strategy")
	if err != nil {
		t.Fatal(err)
	}
	fallbackFailure := errors.New("strategy fallback error")
	turn.strategyFailure = func(ctx context.Context, _ error) error {
		strategyCalled = true
		deadline, ok := ctx.Deadline()
		if ctx.Err() != nil || !ok || time.Until(deadline) < 4*time.Second {
			t.Error("strategy fallback inherited an exhausted durability context")
		}
		return fallbackFailure
	}
	err = turn.fail(ctx, errors.New("provider failed"))
	if !strategyCalled || !lifecycleCalled || !errors.Is(err, fallbackFailure) || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("failure path lost strategy or maintenance failure: %v", err)
	}
}

type experiencePendingStore struct {
	session.EventStore
	pending []session.Event
}

func (s *experiencePendingStore) PendingExperienceMaintenance(ctx context.Context, limit int) ([]session.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.pending[:min(limit, len(s.pending))], nil
}

func TestExperienceCrossSessionRetryUsesHostBudgetAndSourceProvenance(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service, ledger := &experienceStub{}, &accountingLedger{}
	f := profileFactory(t, store, provider)
	f.config.Experience, f.config.UsageLedger = service, ledger
	scope := runtimeTestScope(t)
	f.config.ActorScope = scope
	lookup := &experiencePendingStore{EventStore: store}
	f.events = lookup
	seedExperienceMaintenance(t, store, "inactive-source", "old-turn")
	if err := session.VisitEvents(ctx, store, "inactive-source", func(event session.Event) error {
		lookup.pending = append(lookup.pending, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	provider.auxReply = func(context.Context) (*schema.Message, error) {
		response := schema.AssistantMessage("source review response", nil)
		response.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 17, CompletionTokens: 9, TotalTokens: 26}}
		return response, nil
	}
	service.complete = func(ctx context.Context, id, turn string, generate experience.Generate) error {
		if id != "inactive-source" || turn != "old-turn" {
			t.Fatalf("maintenance lost origin: %s %s", id, turn)
		}
		_, err := generate(ctx, []*schema.Message{schema.SystemMessage("experience auxiliary"), schema.UserMessage("private source evidence")})
		return err
	}
	host := &recordingModel{inner: provider, factory: f, events: lookup, sessionID: "host", turnID: "current"}
	if err := host.retryExperienceMaintenance(ctx); err != nil || host.experienceRetry == nil || service.completes != 0 {
		t.Fatalf("cross-session foreground admission: %v", err)
	}
	// Even an adapter returning a stale result cannot claim an admitted job.
	competitor := &recordingModel{inner: provider, factory: f, events: lookup, sessionID: "other-host", turnID: "other"}
	if err := competitor.retryExperienceMaintenance(ctx); err != nil || competitor.experienceRetry != nil {
		t.Fatalf("stale lookup admitted duplicate maintenance: %v", err)
	}
	host.runExperienceRetry(ctx)
	if status := latestExperienceMaintenance(t, store, "inactive-source", "old-turn"); status.Status != "completed" {
		t.Fatalf("cross-session completion failed: %+v", status)
	}
	if len(ledger.requests) != 1 || len(ledger.records) != 1 ||
		ledger.requests[0].AgentID != scope.AgentID || ledger.records[0].TeamID != scope.TeamID ||
		ledger.records[0].TurnID != "old-turn" {
		t.Fatalf("maintenance charged wrong actor: %+v", ledger)
	}
	input, output, err := host.profileTokenUsage(ctx)
	if err != nil || input != 17 || output != 9 {
		t.Fatalf("host profile usage: %d %d %v", input, output, err)
	}
	source := &recordingModel{events: lookup, sessionID: "inactive-source"}
	input, output, err = source.profileTokenUsage(ctx)
	if err != nil || input != 0 || output != 0 {
		t.Fatalf("origin charged for host maintenance: %d %d %v", input, output, err)
	}
	sourceRequests, hostUsage := 0, 0
	if err := session.VisitEvents(ctx, store, "inactive-source", func(event session.Event) error {
		if event.Type == session.EventModelRequested {
			sourceRequests++
			var payload struct {
				Messages        []*schema.Message        `json:"messages"`
				BudgetSessionID session.ID               `json:"budget_session_id"`
				ActorScope      orchestration.ActorScope `json:"actor_scope"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			if event.CorrelationID != "old-turn" || len(payload.Messages) != 2 ||
				payload.Messages[1].Content != "private source evidence" ||
				payload.BudgetSessionID != "host" || payload.ActorScope != scope {
				t.Fatalf("source request lost provenance: %+v", payload)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.VisitEvents(ctx, store, "host", func(event session.Event) error {
		if event.Type != eventExperienceUsage || strings.Contains(string(event.Payload), "private source evidence") ||
			!strings.Contains(string(event.Payload), `"source_session_id":"inactive-source"`) {
			t.Fatalf("host journal contains source content or lacks provenance: %s", event.Payload)
		}
		hostUsage++
		return nil
	}); err != nil || sourceRequests != 1 || hostUsage != 2 {
		t.Fatalf("source/mirror event counts: %d %d %v", sourceRequests, hostUsage, err)
	}
	// A subsequent host-funded auxiliary call must honor the host's spend.
	profile := runtimeTestProfile()
	profile.Limits.MaxInputTokens = 17
	f.profile = &profile
	auxiliary := &recordingModel{inner: provider, factory: f, events: lookup, sessionID: "inactive-source",
		turnID: "old-turn", budgetSessionID: "host", purpose: "experience"}
	_, err = auxiliary.Generate(ctx, []*schema.Message{schema.UserMessage("another review")})
	if _, ok := errors.AsType[*ProfileConstraintError](err); !ok || provider.auxCalls != 1 {
		t.Fatalf("cross-session auxiliary bypassed host quota: %v", err)
	}
}

func TestExperienceRetrySkipsSupersededAdmission(t *testing.T) {
	ctx, store, provider := t.Context(), memorystore.New(), &experienceModel{}
	service := &experienceStub{}
	f := profileFactory(t, store, provider)
	f.config.Experience = service
	seedExperienceMaintenance(t, store, "stale", "old")
	m := &recordingModel{inner: provider, factory: f, events: store, sessionID: "stale", turnID: "new"}
	if err := m.retryExperienceMaintenance(ctx); err != nil || m.experienceRetry == nil {
		t.Fatalf("admission failed: %v", err)
	}
	_, err := store.Append(ctx, "stale", session.AnySequence, session.PendingEvent{
		Type: EventExperienceMaintenance, CorrelationID: "old",
		Payload: session.MustJSONPayload(ExperienceMaintenance{Status: "completed"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	m.runExperienceRetry(ctx)
	if service.completes != 0 || latestExperienceMaintenance(t, store, "stale", "old").Status != "completed" {
		t.Fatal("superseded admission executed or overwrote completion")
	}
}
