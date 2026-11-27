package agentruntime

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

type contextPolicyLevel string

const (
	contextLevelNormal   contextPolicyLevel = "normal"
	contextLevelElevated contextPolicyLevel = "elevated"
	contextLevelCritical contextPolicyLevel = "critical"
)

// ContextHints are authoritative anchors supplied by the execution layer.
// They are repeated in a compacted summary so an active plan contract cannot
// disappear behind a summary boundary.
type ContextHints struct {
	ContractID   string
	ContractText string
	Attempt      int
}

// PrepareMessages applies the configured context policy before a new turn. A
// compaction is itself a durable event, so every process reconstructs the same
// summary boundary instead of repeatedly summarizing the full old transcript.
func (f *Factory) PrepareMessages(ctx context.Context, sessionID session.ID, messages []*schema.Message) ([]*schema.Message, error) {
	return f.PrepareMessagesWithHints(ctx, sessionID, messages, ContextHints{})
}

func (f *Factory) PrepareMessagesWithHints(ctx context.Context, sessionID session.ID, messages []*schema.Message, hints ContextHints) ([]*schema.Message, error) {
	if len(messages) == 0 {
		return nil, errors.New("context requires at least one message")
	}
	prepared, _, err := f.applyContextPolicy(ctx, sessionID, messages, hints, false)
	return prepared, err
}

// Compact requests compaction even below the automatic threshold, committing
// only a context-reducing boundary. The interactive /compress command shares
// the exact same durable event format as automatic compaction.
func (f *Factory) Compact(ctx context.Context, sessionID session.ID, messages []*schema.Message) ([]*schema.Message, bool, error) {
	return f.CompactWithSink(ctx, sessionID, messages, nil)
}

// CompactWithSink runs explicit context compaction and reports preparation progress to the
// event sink.
func (f *Factory) CompactWithSink(ctx context.Context, sessionID session.ID, messages []*schema.Message, sink EventSink) ([]*schema.Message, bool, error) {
	if len(messages) == 0 {
		return nil, false, errors.New("context requires at least one message")
	}
	return f.applyContextPolicyWithSink(ctx, sessionID, messages, ContextHints{}, true, sink)
}

func (f *Factory) applyContextPolicy(ctx context.Context, sessionID session.ID, messages []*schema.Message, hints ContextHints, force bool) ([]*schema.Message, bool, error) {
	return f.applyContextPolicyWithSink(ctx, sessionID, messages, hints, force, nil)
}

func (f *Factory) applyContextPolicyWithSink(ctx context.Context, sessionID session.ID, messages []*schema.Message, hints ContextHints, force bool, sink EventSink) ([]*schema.Message, bool, error) {
	tokensBefore := f.tokenizer.CountMessages(messages)
	managed, artifacts, err := f.externalizeToolResults(ctx, messages)
	if err != nil {
		return nil, false, err
	}
	tokensManaged := f.tokenizer.CountMessages(managed)
	plan := planContext(f.config.ContextWindowTokens, f.config.CompactionTriggerTokens, tokensManaged, force)
	level := plan.level
	if !plan.compact {
		if len(artifacts) == 0 {
			return managed, false, nil
		}
		if err := f.recordContextManagement(ctx, sessionID, level, "externalize", tokensBefore, tokensManaged, len(messages), len(managed), artifacts, hints, false); err != nil {
			return nil, false, err
		}
		return managed, true, nil
	}
	if force && sink != nil {
		_ = sink(ctx, Event{Type: EventContextCompactionStarted, Purpose: "compaction"})
	}
	// PrepareMessages includes the new, not-yet-durable user input. Compact
	// operates on committed history only. Never persist that pending input as
	// retained history or it will appear twice after the turn starts.
	pending := 0
	if !force && managed[len(managed)-1].Role == schema.User {
		pending = 1
	}
	prepared, compacted, fallback, err := f.compactWithResultWithSink(ctx, sessionID, managed, plan.retainTokens, level, hints, pending, sink)
	if err != nil {
		return nil, false, err
	}
	action := "compact"
	if len(artifacts) > 0 {
		action = "externalize+compact"
	}
	if fallback {
		action += "+extractive_fallback"
	}
	tokensAfter := f.tokenizer.CountMessages(prepared)
	if compacted || len(artifacts) > 0 {
		if err := f.recordContextManagement(ctx, sessionID, level, action, tokensBefore, tokensAfter, len(messages), len(prepared), artifacts, hints, fallback); err != nil {
			return nil, false, err
		}
	}
	return prepared, compacted || len(artifacts) > 0, nil
}

// contextPlan is an effect-free decision. Artifact writes, model requests and
// durable events stay in the orchestration path above. Team spending limits
// govern request admission, never context pressure or information retention.
type contextPlan struct {
	level        contextPolicyLevel
	compact      bool
	retainTokens int
}

// planContext chooses the compaction level and retained token budget from the window,
// trigger and current input size.
func planContext(window, trigger, tokens int, force bool) contextPlan {
	level := contextLevelNormal
	// Mark the context critical when it reaches approximately 90% of the window.
	if tokens >= window-window/10 {
		level = contextLevelCritical
	} else if tokens >= trigger {
		level = contextLevelElevated
	}
	retain := min(window/4, trigger/3)
	if level == contextLevelCritical {
		retain = min(window/6, trigger/4)
	} else if force && level == contextLevelNormal {
		retain = min(window/3, trigger/2)
	}
	return contextPlan{
		level:        level,
		compact:      force || tokens >= trigger,
		retainTokens: retain,
	}
}
