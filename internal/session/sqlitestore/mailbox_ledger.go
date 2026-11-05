package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// mailboxColumns is the column order scanMailboxMessage expects.
const mailboxColumns = `message_id, team_id, sender_agent_id, recipient_agent_id, message_type, body, correlation_id, idempotency_key, sequence, delivery_state, created_at, delivered_at, acknowledged_at, consumer_turn_id, consumer_request, bound_at`

// ClaimMessages binds the recipient's available messages to the claiming turn
// in one transaction. Messages already bound to that turn are returned again,
// so a turn resumed after a crash sees what it had claimed.
func (store *OrchestrationStore) ClaimMessages(ctx context.Context, claim orchestration.MessageClaim) ([]orchestration.Message, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	limit := claim.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin mailbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+mailboxColumns+` FROM mailbox_messages
WHERE team_id = ? AND recipient_agent_id = ?
  AND (delivery_state = ? OR (delivery_state = ? AND consumer_turn_id IN ('', ?)))
ORDER BY sequence ASC LIMIT ?`,
		string(claim.TeamID), string(claim.RecipientAgentID), string(orchestration.DeliveryEnqueued), string(orchestration.DeliveryDelivered), claim.TurnID, limit)
	if err != nil {
		return nil, fmt.Errorf("list claimable mailbox messages: %w", err)
	}
	var messages []orchestration.Message
	for rows.Next() {
		message, scanErr := scanMailboxMessage(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate claimable mailbox messages: %w", err)
	}
	rows.Close()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	for index := range messages {
		message := &messages[index]
		if message.ConsumerTurnID == claim.TurnID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailbox_messages SET delivery_state = ?, delivered_at = COALESCE(delivered_at, ?), consumer_turn_id = ?, consumer_request = ?, bound_at = ? WHERE message_id = ?`,
			string(orchestration.DeliveryDelivered), stamp, claim.TurnID, claim.Request, stamp, string(message.ID)); err != nil {
			return nil, fmt.Errorf("bind mailbox message: %w", err)
		}
		if err := appendMailboxLedgerEvent(ctx, tx, *message, orchestration.EventMailboxBound, map[string]any{
			"message_id": message.ID, "from": message.DeliveryState, "turn_id": claim.TurnID, "request": claim.Request,
		}); err != nil {
			return nil, err
		}
		if message.DeliveredAt == nil {
			message.DeliveredAt = &now
		}
		message.DeliveryState, message.ConsumerTurnID, message.ConsumerRequest, message.BoundAt = orchestration.DeliveryDelivered, claim.TurnID, claim.Request, &now
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit mailbox claim: %w", err)
	}
	return messages, nil
}

// AcknowledgeTurn completes delivery of every message the committed turn
// consumed. Repeating it after a crash acknowledges nothing new.
func (store *OrchestrationStore) AcknowledgeTurn(ctx context.Context, teamID orchestration.TeamID, recipient orchestration.AgentID, turnID string) (int, error) {
	return store.settleTurn(ctx, teamID, recipient, turnID, orchestration.EventMailboxAcknowledged,
		`UPDATE mailbox_messages SET delivery_state = ?, acknowledged_at = ? WHERE message_id = ?`,
		func(stamp string) []any { return []any{string(orchestration.DeliveryAcknowledged), stamp} })
}

// ReleaseTurn unbinds the messages of a turn that will not commit, so the
// next turn of the recipient receives them.
func (store *OrchestrationStore) ReleaseTurn(ctx context.Context, teamID orchestration.TeamID, recipient orchestration.AgentID, turnID string) (int, error) {
	return store.settleTurn(ctx, teamID, recipient, turnID, orchestration.EventMailboxReleased,
		`UPDATE mailbox_messages SET consumer_turn_id = '', consumer_request = 0, bound_at = NULL WHERE message_id = ?`,
		func(string) []any { return nil })
}

// settleTurn applies update to each delivered message bound to turnID and
// records eventType for it. update ends with a message_id placeholder that
// follows the arguments returned by values.
func (store *OrchestrationStore) settleTurn(ctx context.Context, teamID orchestration.TeamID, recipient orchestration.AgentID, turnID string, eventType orchestration.EventType, update string, values func(string) []any) (int, error) {
	claim := orchestration.MessageClaim{TeamID: teamID, RecipientAgentID: recipient, TurnID: turnID}
	if err := claim.Validate(); err != nil {
		return 0, err
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin mailbox settlement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+mailboxColumns+` FROM mailbox_messages WHERE team_id = ? AND recipient_agent_id = ? AND consumer_turn_id = ? AND delivery_state = ? ORDER BY sequence ASC`,
		string(teamID), string(recipient), turnID, string(orchestration.DeliveryDelivered))
	if err != nil {
		return 0, fmt.Errorf("list bound mailbox messages: %w", err)
	}
	var messages []orchestration.Message
	for rows.Next() {
		message, scanErr := scanMailboxMessage(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate bound mailbox messages: %w", err)
	}
	rows.Close()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, message := range messages {
		arguments := append(values(stamp), string(message.ID))
		if _, err := tx.ExecContext(ctx, update, arguments...); err != nil {
			return 0, fmt.Errorf("settle mailbox message: %w", err)
		}
		if err := appendMailboxLedgerEvent(ctx, tx, message, eventType, map[string]any{"message_id": message.ID, "turn_id": turnID}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit mailbox settlement: %w", err)
	}
	return len(messages), nil
}

// appendMailboxLedgerEvent records one ledger change on the recipient's
// mailbox stream.
func appendMailboxLedgerEvent(ctx context.Context, tx *sql.Tx, message orchestration.Message, eventType orchestration.EventType, payload map[string]any) error {
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamMailbox, ID: string(message.RecipientAgentID), TeamID: message.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: message.RecipientAgentID, CorrelationID: message.CorrelationID, Payload: orchestration.MustJSONPayload(payload)},
	); err != nil {
		return fmt.Errorf("record mailbox %s: %w", eventType, err)
	}
	return nil
}
