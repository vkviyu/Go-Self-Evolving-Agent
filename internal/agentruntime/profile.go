package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentprofile"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type ModelBinding struct {
	Model               model.BaseChatModel
	Tokenizer           tokenizer.Counter
	Provider            string
	Name                string
	ContextWindowTokens int
}

type ProfileConstraintError struct{ Reason string }

func (err *ProfileConstraintError) Error() string        { return "Agent profile constraint: " + err.Reason }
func (*ProfileConstraintError) ToolExecutionFatal() bool { return true }

// WithProfile binds a durable child profile to a provider already configured
// by the host. An unknown model alias is an error, never silent inheritance.
// Additional model profiles can be supplied through Config.ResolveModel.
func (f *Factory) WithProfile(ctx context.Context, scope orchestration.ActorScope, sessionID session.ID, profile agentprofile.Profile) (*Factory, error) {
	if sessionID == "" {
		return nil, errors.New("profile-scoped Agent requires a session")
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if scope.ProfileRevision != profile.Revision {
		return nil, agentprofile.ErrSnapshot
	}
	child, err := f.WithActorScope(scope)
	if err != nil {
		return nil, err
	}
	if profile.ModelProfile != f.config.ModelProfile {
		selected, err := f.resolveProfileModel(ctx, profile.ModelProfile)
		if err != nil {
			return nil, err
		}
		child.model = selected.Model
		child.tokenizer = selected.Tokenizer
		child.config.Tokenizer = selected.Tokenizer
		child.config.Provider, child.config.Model = selected.Provider, selected.Name
		child.capacity = modelCapacity(selected.Model, selected.Provider, selected.Name)
		child.config.ContextWindowTokens = selected.ContextWindowTokens
		child.config.CompactionTriggerTokens = 0
		// Re-evaluate an automatic allowance for the recipient's window, rather
		// than carrying the parent's already-resolved 8192 into a small model.
		child.config.MaxOutputTokens = f.outputPolicy
		child.calibration = &contextCalibration{}
	}
	encoded, err := agentprofile.Encode(profile)
	if err != nil {
		return nil, err
	}
	stable, err := agentprofile.Decode(encoded, profile.Revision)
	if err != nil {
		return nil, err
	}
	child.profile = &stable
	child.profileSession = sessionID
	child.config.Name, child.config.Description, child.config.Instruction = profile.Name, profile.Description, profile.Instruction
	if child.config.Description == "" {
		child.config.Description = "Profile-scoped local Agent."
	}
	child.config.ModelProfile = profile.ModelProfile
	child.config.MaxIterations = profile.Limits.MaxIterations
	if profile.Limits.MaxOutputTokens > 0 && profile.Limits.MaxOutputTokens < child.config.MaxOutputTokens {
		child.config.MaxOutputTokens = profile.Limits.MaxOutputTokens
	}
	limits, err := modelcontext.Resolve(child.capacity, modelcontext.Overrides{
		Window: child.config.ContextWindowTokens, Output: child.config.MaxOutputTokens, Trigger: child.config.CompactionTriggerTokens,
	})
	if err != nil {
		return nil, err
	}
	if profile.Limits.MaxOutputTokens > 0 && limits.OutputReserve > profile.Limits.MaxOutputTokens {
		limits, err = modelcontext.Resolve(child.capacity, modelcontext.Overrides{
			Window: child.config.ContextWindowTokens, Output: profile.Limits.MaxOutputTokens, Trigger: child.config.CompactionTriggerTokens,
		})
		if err != nil {
			return nil, err
		}
	}
	// An inherited automatic window remains automatic, not a user override.
	if profile.ModelProfile == f.config.ModelProfile {
		limits.WindowSource = f.config.ContextWindowSource
	}
	applyContextLimits(&child.config, limits)
	child.pipeline = f.pipeline.WithExecutionGuard(&profileToolGuard{profile: stable, scope: scope, sessionID: sessionID, events: f.events})
	return child, err
}

func (f *Factory) resolveProfileModel(ctx context.Context, alias string) (ModelBinding, error) {
	if f.config.ResolveModel == nil {
		return ModelBinding{}, fmt.Errorf("model profile %q is not configured; refusing parent model fallback", alias)
	}
	selected, err := f.config.ResolveModel(ctx, alias)
	if err != nil {
		return ModelBinding{}, err
	}
	if selected.Model == nil || selected.Tokenizer == nil || selected.Provider == "" || selected.Name == "" || selected.ContextWindowTokens <= 1 {
		return ModelBinding{}, errors.New("model profile resolver returned incomplete binding")
	}
	return selected, nil
}

// TrimInheritedContextForProfile uses the recipient's model tokenizer, not
// the parent's, when enforcing the durable inherited-context budget.
func (f *Factory) TrimInheritedContextForProfile(ctx context.Context, text string, profile agentprofile.Profile) (string, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	if profile.ModelProfile == f.config.ModelProfile {
		return f.TrimInheritedContext(text, profile.Limits.MaxInheritedTokens), nil
	}
	selected, err := f.resolveProfileModel(ctx, profile.ModelProfile)
	if err != nil {
		return "", err
	}
	return trimInheritedContext(selected.Tokenizer, text, profile.Limits.MaxInheritedTokens), nil
}

// TrimInheritedContext applies the configured model tokenizer to the exact
// untrusted seed. Truncation preserves UTF-8 and the requested token ceiling.
func (f *Factory) TrimInheritedContext(text string, limit int) string {
	return trimInheritedContext(f.tokenizer, text, limit)
}

func trimInheritedContext(counter tokenizer.Counter, text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if counter.CountMessages([]*schema.Message{schema.UserMessage(string(runes[:middle]))}) <= limit {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return string(runes[:low])
}

func (f *Factory) executionConfigID() string {
	profileID := ""
	if f.profile != nil {
		profileID, _ = f.profile.Fingerprint()
	}
	value := struct {
		Name, Description, Instruction, ModelProfile, Provider, Model, Profile string
		Iterations, Output, Context, Trigger                                   int
	}{
		f.config.Name, f.config.Description, f.runtimeInstruction(), f.config.ModelProfile, f.config.Provider, f.config.Model, profileID,
		f.config.MaxIterations, f.config.MaxOutputTokens, f.config.ContextWindowTokens, f.config.CompactionTriggerTokens,
	}
	encoded, _ := json.Marshal(value)
	hash := sha256.Sum256(encoded)
	return "execution-sha256:" + hex.EncodeToString(hash[:])
}

func permitsProfileTool(profile agentprofile.Profile, descriptor tool.Descriptor) bool {
	if !profile.AllowsTool(descriptor.Name) {
		return false
	}
	if profile.WorkspaceMode == agentprofile.WorkspaceSharedReadonly {
		if descriptor.RiskClass == tool.RiskRead || descriptor.RiskClass == tool.RiskPure {
			return true
		}
		// Task verification and messaging mutate control-plane metadata, not
		// the filesystem. Generic write/execute/secret tools remain denied.
		return slices.Contains(descriptor.Capabilities, "agent.control") && (strings.HasPrefix(descriptor.Name, "task_") || strings.HasPrefix(descriptor.Name, "agent_"))
	}
	return true
}

type profileToolGuard struct {
	profile   agentprofile.Profile
	scope     orchestration.ActorScope
	sessionID session.ID
	events    session.EventStore
	mutex     sync.Mutex
}

func (guard *profileToolGuard) CheckCall(_ context.Context, call tool.Call, descriptor tool.Descriptor) error {
	if call.SessionID != guard.sessionID || call.Actor != guard.scope {
		return &ProfileConstraintError{Reason: "tool caller does not match bound Agent"}
	}
	if !permitsProfileTool(guard.profile, descriptor) {
		return &ProfileConstraintError{Reason: "tool " + descriptor.Name + " is not permitted"}
	}
	return nil
}

func (guard *profileToolGuard) BeforeStart(ctx context.Context, call tool.Call, _ tool.Descriptor) (func(), error) {
	guard.mutex.Lock()
	count := 0
	err := session.VisitEvents(ctx, guard.events, call.SessionID, func(event session.Event) error {
		if event.Type == session.EventToolStarted {
			count++
		}
		return nil
	})
	if err == nil && count >= guard.profile.Limits.MaxToolCalls {
		err = &ProfileConstraintError{Reason: "tool-call budget exhausted"}
	}
	if err != nil {
		guard.mutex.Unlock()
		return nil, err
	}
	return guard.mutex.Unlock, nil
}

func (m *recordingModel) profileOptions(ctx context.Context, input []*schema.Message, options []model.Option) ([]model.Option, error) {
	if m.factory == nil {
		return options, nil
	}
	limit := m.factory.config.MaxOutputTokens
	common := model.GetCommonOptions(nil, options...)
	if common.MaxTokens != nil && *common.MaxTokens > 0 && (limit <= 0 || *common.MaxTokens < limit) {
		limit = *common.MaxTokens
	}
	if profile := m.factory.profile; profile != nil {
		usedInput, usedOutput, err := m.profileTokenUsage(ctx)
		if err != nil {
			return nil, err
		}
		if profile.Limits.MaxInputTokens > 0 && usedInput+m.countRequestInput(input, options) > profile.Limits.MaxInputTokens {
			return nil, &ProfileConstraintError{Reason: "input-token budget exhausted"}
		}
		if profile.Limits.MaxOutputTokens > 0 {
			remaining := profile.Limits.MaxOutputTokens - usedOutput
			if remaining <= 0 {
				return nil, &ProfileConstraintError{Reason: "output-token budget exhausted"}
			}
			if m.purpose == "compaction" && limit > 0 && remaining < limit {
				// A summary and its repair must retain their planned output
				// allowance. Stop on an insufficient quota instead of silently
				// asking for a shorter, potentially incomplete handoff.
				return nil, &ProfileConstraintError{Reason: fmt.Sprintf("compaction requires %d output tokens; profile budget has %d remaining", limit, remaining)}
			}
			if limit <= 0 || remaining < limit {
				limit = remaining
			}
		}
	}
	if limit > 0 {
		options = append(append([]model.Option(nil), options...), model.WithMaxTokens(limit))
	}
	return options, nil
}

// Unknown/unfinished requests retain their input estimate and full output
// reservation. Only a durable reported outcome can lower this conservative
// spend, so process restart or a provider omitting usage cannot reset a budget.
func (m *recordingModel) profileTokenUsage(ctx context.Context) (int, int, error) {
	type spend struct{ input, output int }
	requests := map[string]spend{}
	err := session.VisitEvents(ctx, m.events, m.usageSessionID(), func(event session.Event) error {
		if event.Type != session.EventModelRequested && event.Type != session.EventAssistantMessage && event.Type != eventExperienceUsage {
			return nil
		}
		var payload struct {
			RequestID       string            `json:"request_id"`
			Input           int               `json:"estimated_input_tokens"`
			Limit           int               `json:"output_token_limit"`
			ActualInput     *int              `json:"prompt_tokens"`
			ActualOutput    *int              `json:"completion_tokens"`
			Phase           session.EventType `json:"phase"`
			BudgetSessionID session.ID        `json:"budget_session_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		if payload.RequestID == "" {
			return nil
		}
		if payload.BudgetSessionID != "" && payload.BudgetSessionID != m.usageSessionID() {
			return nil // A host paid for this session's maintenance.
		}
		if event.Type == session.EventModelRequested || payload.Phase == session.EventModelRequested {
			requests[payload.RequestID] = spend{payload.Input, payload.Limit}
		} else if payload.ActualInput != nil && payload.ActualOutput != nil {
			requests[payload.RequestID] = spend{*payload.ActualInput, *payload.ActualOutput}
		}
		return nil
	})
	input, output := 0, 0
	for _, request := range requests {
		input += request.input
		output += request.output
	}
	return input, output, err
}
