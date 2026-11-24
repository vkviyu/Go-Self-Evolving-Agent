package agentruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var ErrContextCapacity = errors.New("model context capacity exceeded")

// Eino owns the loop and its full checkpoint. This view replaces an unchanged
// prefix only at the model boundary, so the loop does not reintroduce compacted
// history on its next iteration. A resumed runner can safely rebuild the view.
type requestContextState struct {
	mu       sync.Mutex
	covered  int
	digest   [sha256.Size]byte
	prepared []*schema.Message
}

func (m *recordingModel) prepareModelInput(ctx context.Context, input []*schema.Message, options []model.Option) ([]*schema.Message, []model.Option, error) {
	m.emitLive(ctx, m.modelLifecycleEvent(EventModelPreparationStarted))
	var err error
	if m.purpose == "" {
		if err := checkToolProgress(input); err != nil {
			return nil, nil, err
		}
	}
	if replay := m.replay; replay != nil && replay.Next == len(replay.Responses) && replay.Pending != nil && replay.PendingAfter == len(replay.Responses) {
		// The last dispatch may have stopped after its prompt was prepared.
		// Reuse that durable prompt rather than regenerate a semantic summary
		// or append messages which arrived after that execution boundary.
		if replay.Pending.RawDigest != rawRequestDigest(input) {
			return nil, nil, errors.New("turn journal diverged at the prepared model request; refusing replacement execution")
		}
		if err := m.restoreModelReceipt(*replay.Pending, options); err != nil {
			return nil, nil, err
		}
		input, err = cloneJournalInput(replay.Pending.Input)
		if err != nil {
			return nil, nil, err
		}
		if m.experienceEnabled() && m.receipt.Experience == nil {
			return nil, nil, errors.New("prepared request has no experience context; refusing semantic reselection")
		}
		if err := m.checkDispatchCapacity(input, options); err != nil {
			return nil, nil, err
		}
		options, err = m.profileOptions(ctx, input, options)
		if err == nil {
			err = m.captureRequest(input, options)
		}
		replay.Pending = nil
		return input, options, err
	}
	if err := m.prepareExperience(ctx, input); err != nil {
		return nil, nil, err
	}
	opening := m.takeOpeningInput(input)
	input, err = m.prepareBackgroundInput(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	m.receipt.Covered, m.receipt.Digest = len(input), requestPrefixDigest(input)
	input, err = m.prepareRequestContext(ctx, input, options)
	if err != nil {
		return nil, nil, err
	}
	input, err = m.attachExperience(input, options)
	if err != nil {
		return nil, nil, err
	}
	// Profile quotas are charged against the prompt actually dispatched,
	// including tool schemas, rather than the uncompressed Eino checkpoint.
	baseInput, baseOptions := input, options
	input, err = m.attachFileReferences(ctx, input, opening, options)
	if err != nil {
		return nil, nil, err
	}
	options, err = m.profileOptions(ctx, input, baseOptions)
	if _, constrained := errors.AsType[*ProfileConstraintError](err); constrained && m.receipt.FileReferencePrompt != nil {
		// Optional context should never consume the last of a Profile's
		// input quota and prevent a prompt that otherwise fits.
		m.receipt.FileReferencePrompt = nil
		input = baseInput
		options, err = m.profileOptions(ctx, input, baseOptions)
	}
	if err == nil {
		err = m.captureRequest(input, options)
		if err == nil {
			m.receipt.Input, err = cloneJournalInput(input)
		}
		if err == nil && m.purpose == "" && m.receipt.RawDigest != "" {
			err = m.recordPreparedRequest(ctx)
		}
	}
	return input, options, err
}

func (m *recordingModel) recordPreparedRequest(ctx context.Context) error {
	m.emitLive(ctx, m.modelLifecycleEvent(EventTurnPreparedPersistenceStarted))
	durable, cancel := durabilityContext(ctx)
	defer cancel()
	if _, err := m.events.Append(durable, m.sessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventTurnPrepared, CorrelationID: m.turnID, RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload: session.MustJSONPayload(turnJournal{Version: 2, Prepared: true, Receipt: &m.receipt}),
	}); err != nil {
		return fmt.Errorf("record prepared turn journal: %w", err)
	}
	return nil
}

func (m *recordingModel) prepareRequestContext(ctx context.Context, input []*schema.Message, options []model.Option) ([]*schema.Message, error) {
	if m.factory == nil || m.factory.tokenizer == nil || m.factory.config.ContextWindowTokens <= 0 {
		return input, nil
	}
	f := m.factory
	limit := modelcontext.InputLimit(f.config.ContextWindowTokens, requestOutputLimit(f, options))
	m.emitLive(ctx, m.modelLifecycleEvent(EventContextTokenCountingStarted))
	// Auxiliary requests have purpose-specific structured input. They may not
	// recursively summarize themselves or silently remove evaluator evidence.
	if m.purpose != "" {
		return input, m.checkContextLimit(input, options, limit)
	}
	m.contextState.mu.Lock()
	defer m.contextState.mu.Unlock()
	state := &m.contextState
	managed := input
	hadView, reusedView := state.covered > 0, false
	if state.covered > 0 && len(input) >= state.covered && requestPrefixDigest(input[:state.covered]) == state.digest {
		managed = append(append([]*schema.Message(nil), state.prepared...), input[state.covered:]...)
		reusedView = true
	}
	tokensBefore := m.countRequestInput(managed, options)
	var err error
	managed, artifacts, err := f.externalizeToolResults(ctx, managed)
	if err != nil {
		return nil, err
	}
	before := m.countRequestInput(managed, options)
	reserve, err := m.experienceTokenReserve(managed, options)
	if err != nil {
		return nil, err
	}
	trigger := max(1, min(f.config.CompactionTriggerTokens, limit)-reserve)
	changed := len(artifacts) > 0 || state.covered > 0
	compacted := false
	if before >= trigger {
		m.emitLive(ctx, Event{Type: EventContextCompactionStarted, TurnID: m.turnID, Purpose: m.purpose})
		var prepared []*schema.Message
		prepared, compacted, err = m.compactRequest(ctx, managed, options, trigger)
		if err != nil {
			return nil, err
		}
		if compacted {
			managed = prepared
			changed = true
		}
	}
	reserve, err = m.experienceTokenReserve(managed, options)
	if err != nil {
		return nil, err
	}
	if err := m.checkContextLimit(managed, options, limit-reserve); err != nil {
		return nil, err
	}
	if !compacted && (len(artifacts) > 0 || (hadView && !reusedView)) {
		_, history := splitRequestContext(managed)
		if err := m.recordRequestContext(ctx, "", history, tokensBefore, m.countRequestInput(managed, options), trigger, ""); err != nil {
			return nil, err
		}
	}
	if len(artifacts) > 0 {
		if err := f.recordContextManagement(ctx, m.sessionID, contextLevelNormal, "request_externalize", tokensBefore, m.countRequestInput(managed, options), len(input), len(managed), artifacts, ContextHints{}, false); err != nil {
			return nil, err
		}
	}
	if !changed {
		return managed, nil
	}
	// Preserve a private, stable prefix: Eino may annotate outgoing messages.
	state.covered, state.digest = len(input), requestPrefixDigest(input)
	state.prepared = make([]*schema.Message, len(managed))
	for index, message := range managed {
		if message == nil {
			continue
		}
		state.prepared[index], err = cloneMessage(message)
		if err != nil {
			return nil, err
		}
	}
	return managed, nil
}

func (m *recordingModel) compactRequest(ctx context.Context, input []*schema.Message, options []model.Option, trigger int) ([]*schema.Message, bool, error) {
	pinned, history := splitRequestContext(input)
	if len(history) < 2 {
		return input, false, nil
	}
	f := m.factory
	keep := latestMessageCountWithCounter(f.tokenizer, history, max(1, trigger/3))
	if keep >= len(history) {
		return input, false, nil
	}
	boundary := len(history) - keep
	prefix := append([]*schema.Message(nil), history[:boundary]...)
	retained := append([]*schema.Message(nil), history[boundary:]...)
	// Preserve the current request verbatim even in a very long tool loop.
	for index, message := range slices.Backward(history) {
		if message != nil && message.Role == schema.User {
			if index < boundary {
				retained = append([]*schema.Message{message}, retained...)
				prefix = append(prefix[:index:index], prefix[index+1:]...)
			}
			break
		}
	}
	if len(prefix) == 0 {
		return input, false, nil
	}
	if len(prefix) == 1 && prefix[0] != nil && prefix[0].Role == schema.System && strings.HasPrefix(prefix[0].Content, "Conversation summary:\n") {
		return input, false, nil // No new older work to summarize; avoid compaction thrashing.
	}
	hints := ContextHints{}
	summary, err := f.summarizeWithSink(ctx, m.sessionID, prefix, hints, m.liveSink)
	if err != nil {
		return nil, false, err
	}
	summarySource := summaryFormatSource(summary)
	summary = anchorContract(summary, hints)
	prepared := append(append([]*schema.Message(nil), pinned...), schema.SystemMessage("Conversation summary:\n"+summary))
	prepared = append(prepared, retained...)
	before, after := m.countRequestInput(input, options), m.countRequestInput(prepared, options)
	if after >= before {
		return input, false, nil // The final capacity guard still rejects an oversized prompt.
	}
	limit := modelcontext.InputLimit(f.config.ContextWindowTokens, requestOutputLimit(f, options))
	reserve, err := m.experienceTokenReserve(prepared, options)
	if err != nil {
		return nil, false, err
	}
	if err := m.checkContextLimit(prepared, options, limit-reserve); err != nil {
		return nil, false, err
	}
	if err := m.recordRequestContext(ctx, summary, retained, before, after, trigger, summarySource); err != nil {
		return nil, false, err
	}
	return prepared, true, nil
}

func splitRequestContext(input []*schema.Message) (pinned, history []*schema.Message) {
	for _, message := range input {
		if message != nil && message.Role == schema.System && !strings.HasPrefix(message.Content, "Conversation summary:\n") {
			pinned = append(pinned, message)
		} else {
			history = append(history, message)
		}
	}
	return pinned, history
}

func (m *recordingModel) recordRequestContext(ctx context.Context, summary string, retained []*schema.Message, before, after, trigger int, source string) error {
	// A v2 checkpoint inside a running turn becomes history only if that turn
	// completes. Interrupted turns must not leak partial tools into history.
	_, err := m.events.Append(ctx, m.sessionID, session.AnySequence, session.PendingEvent{
		Type: session.EventContextCompacted, CorrelationID: m.turnID,
		Payload: session.MustJSONPayload(map[string]any{
			"context_checkpoint_version": 2, "turn_id": m.turnID,
			"summary": summary, "retained_messages": durableContextMessages(retained),
			"tokens_before": before, "tokens_after": after, "trigger_tokens": trigger,
			"context_window_tokens": m.factory.config.ContextWindowTokens,
			"summary_source":        source, "fallback": source == summarySourceExtractive,
		}),
	})
	if err != nil {
		return fmt.Errorf("record request context compaction: %w", err)
	}
	return nil
}

func (m *recordingModel) checkContextLimit(input []*schema.Message, options []model.Option, limit int) error {
	if used := m.countRequestInput(input, options); used > limit {
		return fmt.Errorf("%w: prompt needs %d tokens; input budget is %d after output and safety reserves (model window %d). Reduce the latest input/tool output or set the actual deployment capacity with --context-window-tokens", ErrContextCapacity, used, limit, m.factory.config.ContextWindowTokens)
	}
	return nil
}

func requestOutputLimit(f *Factory, options []model.Option) int {
	limit := f.config.MaxOutputTokens
	if common := model.GetCommonOptions(nil, options...); common.MaxTokens != nil && *common.MaxTokens > 0 && (limit <= 0 || *common.MaxTokens < limit) {
		limit = *common.MaxTokens
	}
	return limit
}

func requestPrefixDigest(messages []*schema.Message) [sha256.Size]byte {
	// Runtime IDs and provider usage metadata are not model-visible content.
	// Ignore Eino's message annotations when checking an unchanged prefix.
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, message := range messages {
		if message == nil {
			_ = encoder.Encode(nil)
			continue
		}
		stable := *message
		stable.ResponseMeta = nil
		stable.Extra = maps.Clone(message.Extra)
		delete(stable.Extra, "_eino_msg_id")
		if len(stable.Extra) == 0 {
			stable.Extra = nil
		}
		_ = encoder.Encode(stable)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}
