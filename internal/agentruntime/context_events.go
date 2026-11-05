package agentruntime

import (
	"context"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func (f *Factory) recordContextManagement(
	ctx context.Context,
	sessionID session.ID,
	level contextPolicyLevel,
	action string,
	tokensBefore, tokensAfter, messagesBefore, messagesAfter int,
	artifacts []contextArtifact,
	hints ContextHints,
	fallback bool,
) error {
	managementID, err := randomID()
	if err != nil {
		return err
	}
	_, err = f.events.Append(ctx, sessionID, session.AnySequence, session.PendingEvent{
		Type:          session.EventContextManaged,
		CorrelationID: managementID,
		Payload: session.MustJSONPayload(map[string]any{
			"management_id":   managementID,
			"policy_level":    level,
			"action":          action,
			"tokens_before":   tokensBefore,
			"tokens_after":    tokensAfter,
			"messages_before": messagesBefore,
			"messages_after":  messagesAfter,
			"artifacts":       artifacts,
			"goal_id":         hints.GoalID,
			"goal_round":      hints.GoalRound,
			"fallback":        fallback,
		}),
	})
	if err != nil {
		return fmt.Errorf("record context management: %w", err)
	}
	return nil
}
