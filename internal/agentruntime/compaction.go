package agentruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

// compactWithResult commits the summary and retained-history boundary before
// returning prepared messages. Pending user input is never part of that checkpoint.
func (f *Factory) compactWithResult(ctx context.Context, sessionID session.ID, messages []*schema.Message, retainTokens int, level contextPolicyLevel, hints ContextHints, pending int) ([]*schema.Message, bool, bool, error) {
	return f.compactWithResultWithSink(ctx, sessionID, messages, retainTokens, level, hints, pending, nil)
}

func (f *Factory) compactWithResultWithSink(ctx context.Context, sessionID session.ID, messages []*schema.Message, retainTokens int, level contextPolicyLevel, hints ContextHints, pending int, sink EventSink) ([]*schema.Message, bool, bool, error) {
	usedTokens := f.tokenizer.CountMessages(messages)
	if usedTokens > f.config.ContextWindowTokens {
		lastMessageTokens := f.tokenizer.CountMessages(messages[len(messages)-1:])
		if lastMessageTokens > f.config.ContextWindowTokens {
			return nil, false, false, fmt.Errorf("latest message uses %d tokens, exceeding context window %d", lastMessageTokens, f.config.ContextWindowTokens)
		}
	}
	keep := max(latestMessageCountWithCounter(f.tokenizer, messages, retainTokens), 1)
	if keep >= len(messages) {
		return messages, false, false, nil
	}
	prefix := messages[:len(messages)-keep]
	if len(prefix) == 0 {
		return messages, false, false, nil
	}
	summary, err := f.summarizeWithSink(ctx, sessionID, prefix, hints, sink)
	fallback := false
	if err != nil {
		if level != contextLevelCritical || !canFallbackSummary(ctx, err) {
			return nil, false, false, err
		}
		summary, err = extractiveCompactionSummary(prefix)
		if err != nil {
			return nil, false, false, errors.Join(err, errors.New("semantic context summary failed"))
		}
		fallback = true
	}
	source := summaryFormatSource(summary)
	fallback = fallback || source == summarySourceExtractive
	summary = anchorContract(summary, hints)
	result := make([]*schema.Message, 0, keep+1)
	result = append(result, schema.SystemMessage("Conversation summary:\n"+summary))
	result = append(result, messages[len(messages)-keep:]...)
	if f.tokenizer.CountMessages(result) >= usedTokens {
		return messages, false, false, nil
	}
	compactionID, err := randomID()
	if err != nil {
		return nil, false, false, err
	}
	if _, err := f.events.Append(ctx, sessionID, session.AnySequence, session.PendingEvent{
		Type:          session.EventContextCompacted,
		CorrelationID: compactionID,
		Payload: session.MustJSONPayload(map[string]any{
			"compaction_id":              compactionID,
			"summary":                    summary,
			"messages_before":            len(messages),
			"messages_after":             keep + 1,
			"tokens_before":              usedTokens,
			"retain_tokens":              retainTokens,
			"policy_level":               level,
			"fallback":                   fallback,
			"summary_source":             source,
			"contract_id":                hints.ContractID,
			"attempt":                    hints.Attempt,
			"context_checkpoint_version": 1,
			"retained_messages":          durableContextMessages(messages[len(messages)-keep : len(messages)-pending]),
		}),
	}); err != nil {
		return nil, false, false, fmt.Errorf("record context compaction: %w", err)
	}
	return result, true, fallback, nil
}

func canFallbackSummary(ctx context.Context, err error) bool {
	var profileErr *ProfileConstraintError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, tokenusage.ErrBudgetExceeded) || errors.As(err, &profileErr) {
		return false
	}
	var invocationErr *modelInvocationError
	return errors.Is(err, ErrContextCapacity) || errors.As(err, &invocationErr)
}
