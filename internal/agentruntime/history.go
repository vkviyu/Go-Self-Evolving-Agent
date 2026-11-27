package agentruntime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type historyTurn struct {
	openedAt    uint64
	completedAt uint64
	user        string
	hasUser     bool
	messages    []*schema.Message
	context     *historyContextCheckpoint
	partial     map[string][]*schema.Message
	confirmed   map[string]bool
}

type historyContextCheckpoint struct {
	Summary  string            `json:"summary"`
	Version  int               `json:"context_checkpoint_version"`
	Retained []*schema.Message `json:"retained_messages"`
	TurnID   string            `json:"turn_id"`
	offset   int
}

type historyMessagePayload struct {
	Purpose            string            `json:"purpose"`
	RequestID          string            `json:"request_id"`
	Content            string            `json:"content"`
	ToolCalls          []schema.ToolCall `json:"tool_calls"`
	FinishReason       string            `json:"finish_reason"`
	PromptTokens       int               `json:"prompt_tokens"`
	CachedPromptTokens int               `json:"cached_prompt_tokens"`
	CompletionTokens   int               `json:"completion_tokens"`
	ReasoningTokens    int               `json:"reasoning_tokens"`
	TotalTokens        int               `json:"total_tokens"`
}

type historyToolPayload struct {
	TurnID string          `json:"turn_id"`
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Result json.RawMessage `json:"result"`
}

type historyToolFailurePayload struct {
	TurnID string `json:"turn_id"`
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	Error  string `json:"error"`
}

// LoadHistory projects only completed turns into the model conversation. The
// projection preserves assistant tool calls and matching tool results; failed
// or interrupted turns are intentionally excluded.
func (f *Factory) LoadHistory(ctx context.Context, sessionID session.ID) ([]*schema.Message, error) {
	return f.projectHistory(ctx, sessionID, false)
}

// LoadTranscript projects every turn of sessionID for display, including the
// turn that is still running or was interrupted. It never calibrates the
// factory's tokenizer, so it can read another Agent's session.
func (f *Factory) LoadTranscript(ctx context.Context, sessionID session.ID) ([]*schema.Message, error) {
	return f.projectHistory(ctx, sessionID, true)
}

// TranscriptRevision returns the session event high-water mark. Read it before
// loading the transcript so that events appended during the projection can
// trigger another refresh.
func (f *Factory) TranscriptRevision(ctx context.Context, sessionID session.ID) (uint64, error) {
	if sessionID == "" {
		return 0, errors.New("transcript requires a session ID")
	}
	return f.events.CurrentSequence(ctx, sessionID)
}

// projectHistory replays the events of sessionID. A transcript keeps open
// turns and skips provider calibration; a model history keeps only completed
// turns.
func (f *Factory) projectHistory(ctx context.Context, sessionID session.ID, transcript bool) ([]*schema.Message, error) {
	if sessionID == "" {
		return nil, errors.New("history requires a session ID")
	}
	turns := make(map[string]*historyTurn)
	order := make([]string, 0)
	callToTurn := make(map[string]string)
	estimatedByTurn := make(map[string]int)
	compactedSummary := ""
	var compactedRetained []*schema.Message
	var lastHistoryCommit uint64
	pruneCompactedTurns := func() {
		// Checkpoints cover completed context. An unresolved turn was not in
		// that context and may still complete through its original journal.
		order = slices.DeleteFunc(order, func(id string) bool {
			if turns[id].completedAt == 0 {
				return false
			}
			delete(turns, id)
			delete(estimatedByTurn, id)
			return true
		})
		maps.DeleteFunc(callToTurn, func(_ string, id string) bool { return turns[id] == nil })
	}
	err := session.VisitEvents(ctx, f.events, sessionID, func(event session.Event) error {
		switch event.Type {
		case session.EventContextCompacted:
			if transcript {
				return nil
			}
			var payload historyContextCheckpoint
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode context compaction event %s: %w", event.EventID, err)
			}
			if payload.Version != 1 && payload.Version != 2 {
				return fmt.Errorf("unsupported context checkpoint version %d", payload.Version)
			}
			if payload.Version == 2 {
				if payload.TurnID == "" {
					return errors.New("request context checkpoint is missing its turn ID")
				}
				turn := ensureHistoryTurn(turns, &order, payload.TurnID, event.Sequence)
				payload.offset = len(turn.messages)
				turn.context = &payload
				return nil
			}
			compactedSummary = payload.Summary
			compactedRetained = payload.Retained
			lastHistoryCommit = event.Sequence
			pruneCompactedTurns()
		case session.EventTurnStarted:
			ensureHistoryTurn(turns, &order, event.CorrelationID, event.Sequence)
		case session.EventUserMessage:
			turn := ensureHistoryTurn(turns, &order, event.CorrelationID, event.Sequence)
			var payload struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode user history event %s: %w", event.EventID, err)
			}
			turn.user = payload.Content
			turn.hasUser = true
		case session.EventBackgroundInput:
			turn := ensureHistoryTurn(turns, &order, event.CorrelationID, event.Sequence)
			var payload backgroundRecord
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode background history event %s: %w", event.EventID, err)
			}
			// An opening block takes the place of the user message; a later
			// block was injected after the messages recorded before it.
			if payload.Opening {
				turn.user = payload.Content
				turn.hasUser = true
			} else {
				turn.messages = append(turn.messages, schema.UserMessage(payload.Content))
			}
		case session.EventModelRequested:
			if transcript {
				return nil
			}
			var payload struct {
				EstimatedInputTokens int    `json:"estimated_input_tokens"`
				Purpose              string `json:"purpose"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err == nil && payload.Purpose == "" {
				estimatedByTurn[event.CorrelationID] = payload.EstimatedInputTokens
			}
		case session.EventAssistantMessage:
			var payload historyMessagePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode assistant history event %s: %w", event.EventID, err)
			}
			if payload.Purpose != "" {
				return nil
			}
			turn := ensureHistoryTurn(turns, &order, event.CorrelationID, event.Sequence)
			if turn.confirmed == nil {
				turn.confirmed = make(map[string]bool)
			}
			turn.confirmed[payload.RequestID] = true
			assistant := schema.AssistantMessage(payload.Content, payload.ToolCalls)
			if payload.FinishReason != "" {
				assistant.ResponseMeta = &schema.ResponseMeta{FinishReason: payload.FinishReason}
			}
			if payload.PromptTokens > 0 {
				if assistant.ResponseMeta == nil {
					assistant.ResponseMeta = &schema.ResponseMeta{}
				}
				assistant.ResponseMeta.Usage = &schema.TokenUsage{
					PromptTokens: payload.PromptTokens,
					PromptTokenDetails: schema.PromptTokenDetails{
						CachedTokens: payload.CachedPromptTokens,
					},
					CompletionTokens: payload.CompletionTokens,
					TotalTokens:      payload.TotalTokens,
					CompletionTokensDetails: schema.CompletionTokensDetails{
						ReasoningTokens: payload.ReasoningTokens,
					},
				}
				if estimated, ok := estimatedByTurn[event.CorrelationID]; ok && !transcript {
					key := payload.RequestID
					if key == "" {
						key = event.EventID
					}
					f.observeProviderInput(key, payload.PromptTokens, estimated)
				}
			}
			turn.messages = append(turn.messages, assistant)
		case session.EventModelPartial:
			if !transcript {
				return nil
			}
			var payload struct {
				RequestID string          `json:"request_id"`
				Chunk     *schema.Message `json:"chunk"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return err
			}
			turn := ensureHistoryTurn(turns, &order, event.CorrelationID, event.Sequence)
			if turn.partial == nil {
				turn.partial = make(map[string][]*schema.Message)
			}
			turn.partial[payload.RequestID] = append(turn.partial[payload.RequestID], payload.Chunk)
		case session.EventToolProposed:
			var payload historyToolPayload
			if err := json.Unmarshal(event.Payload, &payload); err == nil && payload.CallID != "" && payload.TurnID != "" {
				callToTurn[payload.CallID] = payload.TurnID
			}
		case session.EventToolSucceeded:
			var payload historyToolPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode tool history event %s: %w", event.EventID, err)
			}
			turnID := payload.TurnID
			if turnID == "" {
				turnID = callToTurn[payload.CallID]
			}
			if turnID == "" || payload.CallID == "" {
				return nil
			}
			turn := ensureHistoryTurn(turns, &order, turnID, event.Sequence)
			result := string(payload.Result)
			if result == "" || result == "null" {
				result = "{}"
			}
			turn.messages = append(turn.messages, schema.ToolMessage(result, payload.CallID, schema.WithToolName(payload.Tool)))
		case session.EventToolFailed:
			var payload historyToolFailurePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return fmt.Errorf("decode failed tool history event %s: %w", event.EventID, err)
			}
			turnID := payload.TurnID
			if turnID == "" {
				turnID = callToTurn[payload.CallID]
			}
			if turnID == "" || payload.CallID == "" {
				return nil
			}
			turn := ensureHistoryTurn(turns, &order, turnID, event.Sequence)
			failure := map[string]string{"error": payload.Error}
			encoded, err := json.Marshal(failure)
			if err != nil {
				return fmt.Errorf("encode failed tool history event %s: %w", event.EventID, err)
			}
			turn.messages = append(turn.messages, schema.ToolMessage(string(encoded), payload.CallID, schema.WithToolName(payload.Tool)))
		case session.EventTurnCompleted:
			turn := ensureHistoryTurn(turns, &order, event.CorrelationID, event.Sequence)
			turn.completedAt = event.Sequence
			if transcript {
				return nil
			}
			// Resume reconstructs the original turn input, even when newer
			// context has committed in the meantime. Publish this checkpoint
			// only if it was derived from the current conversation baseline.
			if checkpoint := turn.context; checkpoint != nil && turn.openedAt > lastHistoryCommit {
				compactedSummary = checkpoint.Summary
				compactedRetained = append(append([]*schema.Message(nil), checkpoint.Retained...), turn.messages[checkpoint.offset:]...)
				pruneCompactedTurns()
			}
			lastHistoryCommit = event.Sequence
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	history := make([]*schema.Message, 0)
	if compactedSummary != "" {
		history = append(history, schema.SystemMessage("Conversation summary:\n"+compactedSummary))
	}
	history = append(history, compactedRetained...)
	if !transcript {
		// Late recovery becomes available to the model when it completes.
		// The display keeps the opening order of all turns.
		slices.SortStableFunc(order, func(a, b string) int { return cmp.Compare(turns[a].completedAt, turns[b].completedAt) })
	}
	for _, id := range order {
		turn := turns[id]
		if !turn.hasUser || (turn.completedAt == 0 && !transcript) {
			continue
		}
		history = append(history, schema.UserMessage(turn.user))
		history = append(history, turn.messages...)
		if transcript && turn.completedAt == 0 {
			// Request IDs contain a monotonic sequence. Event order is retained
			// separately below rather than relying on map iteration.
			keys := make([]string, 0, len(turn.partial))
			for requestID := range turn.partial {
				keys = append(keys, requestID)
			}
			slices.Sort(keys)
			for _, requestID := range keys {
				if turn.confirmed[requestID] {
					continue
				}
				message, err := schema.ConcatMessages(turn.partial[requestID])
				if err != nil {
					return nil, fmt.Errorf("restore partial model output: %w", err)
				}
				if message != nil && message.Content != "" {
					message.ToolCalls = nil
					message.Content += "\n[Partial model response]"
					history = append(history, message)
				}
			}
		}
	}
	return durableContextMessages(history), nil
}

// Reasoning deltas are live output, not replayed assistant history. Apply the
// same content/tool-call projection to checkpoint suffixes and post-turn counts;
// generated reasoning tokens remain in their separate usage records. Never
// mutate Eino's or the caller's message.
func durableContextMessages(messages []*schema.Message) []*schema.Message {
	result := append([]*schema.Message(nil), messages...)
	for i, message := range result {
		if message != nil && message.Role == schema.Assistant && message.ReasoningContent != "" {
			copy := *message
			copy.ReasoningContent = ""
			result[i] = &copy
		}
	}
	return result
}

// ensureHistoryTurn preserves first-seen event order when collecting later events for the
// same turn.
func ensureHistoryTurn(turns map[string]*historyTurn, order *[]string, id string, sequence uint64) *historyTurn {
	if turn, ok := turns[id]; ok {
		return turn
	}
	turn := &historyTurn{openedAt: sequence}
	turns[id] = turn
	*order = append(*order, id)
	return turn
}
