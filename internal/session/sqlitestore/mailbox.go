package sqlitestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (store *OrchestrationStore) Enqueue(ctx context.Context, spec orchestration.MessageSpec) (orchestration.Message, error) {
	if err := spec.Validate(); err != nil {
		return orchestration.Message{}, fmt.Errorf("validate mailbox message: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.Message{}, fmt.Errorf("begin mailbox enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	message, err := enqueueMessageTx(ctx, tx, spec)
	if err != nil {
		return orchestration.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.Message{}, fmt.Errorf("commit mailbox enqueue: %w", err)
	}
	return message, nil
}

// enqueueMessageTx joins delivery to the caller's durable transition. In
// particular, an Agent result and its parent notification commit together.
func enqueueMessageTx(ctx context.Context, tx *sql.Tx, spec orchestration.MessageSpec) (orchestration.Message, error) {
	if err := spec.Validate(); err != nil {
		return orchestration.Message{}, fmt.Errorf("validate mailbox message: %w", err)
	}
	if spec.IdempotencyKey != "" {
		var messageID string
		err := tx.QueryRowContext(ctx, `SELECT message_id FROM mailbox_messages WHERE team_id = ? AND idempotency_key = ?`, string(spec.TeamID), spec.IdempotencyKey).Scan(&messageID)
		if err == nil {
			message, loadErr := mailboxMessageByID(ctx, tx, orchestration.MessageID(messageID))
			if loadErr != nil {
				return orchestration.Message{}, loadErr
			}
			if !sameMessageRequest(message, spec) {
				return orchestration.Message{}, fmt.Errorf("%w: mailbox key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			return message, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return orchestration.Message{}, fmt.Errorf("inspect mailbox idempotency key: %w", err)
		}
	}
	type agentRecord struct {
		teamID, parentID string
		state            orchestration.AgentState
	}
	records := make(map[orchestration.AgentID]agentRecord, 2)
	for _, agentID := range []orchestration.AgentID{spec.SenderAgentID, spec.RecipientAgentID} {
		var teamID, parentID, state string
		if err := tx.QueryRowContext(ctx, `SELECT team_id, parent_agent_id, state FROM agents WHERE agent_id = ?`, string(agentID)).Scan(&teamID, &parentID, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return orchestration.Message{}, fmt.Errorf("%w: Agent %s", orchestration.ErrNotFound, agentID)
			}
			return orchestration.Message{}, fmt.Errorf("inspect mailbox Agent %s: %w", agentID, err)
		}
		if teamID != string(spec.TeamID) {
			return orchestration.Message{}, fmt.Errorf("%w: mailbox Agent %s belongs to another team", orchestration.ErrInvalidEvent, agentID)
		}
		records[agentID] = agentRecord{teamID: teamID, parentID: parentID, state: orchestration.AgentState(state)}
		if agentID == spec.RecipientAgentID && isTerminalAgentState(orchestration.AgentState(state)) && spec.Type != orchestration.MessageResult && spec.Type != orchestration.MessageControl {
			return orchestration.Message{}, fmt.Errorf("%w: terminal Agent %s does not accept ordinary messages", orchestration.ErrInvalidEvent, agentID)
		}
	}
	sender, recipient := records[spec.SenderAgentID], records[spec.RecipientAgentID]
	if sender.parentID != string(spec.RecipientAgentID) && recipient.parentID != string(spec.SenderAgentID) {
		return orchestration.Message{}, fmt.Errorf("%w: mailbox only supports parent-child delivery", orchestration.ErrInvalidEvent)
	}
	if isTerminalAgentState(sender.state) && spec.Type != orchestration.MessageResult && spec.Type != orchestration.MessageControl {
		return orchestration.Message{}, fmt.Errorf("%w: terminal Agent %s cannot send ordinary messages", orchestration.ErrInvalidEvent, spec.SenderAgentID)
	}
	messageID, err := orchestration.NewMessageID()
	if err != nil {
		return orchestration.Message{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_recipient_sequences(recipient_agent_id, next_sequence) VALUES(?, 2)
ON CONFLICT(recipient_agent_id) DO UPDATE SET next_sequence = next_sequence + 1`, string(spec.RecipientAgentID)); err != nil {
		return orchestration.Message{}, fmt.Errorf("reserve mailbox sequence: %w", err)
	}
	var nextSequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT next_sequence FROM mailbox_recipient_sequences WHERE recipient_agent_id = ?`, string(spec.RecipientAgentID)).Scan(&nextSequence); err != nil {
		return orchestration.Message{}, fmt.Errorf("read mailbox sequence reservation: %w", err)
	}
	if nextSequence < 2 {
		return orchestration.Message{}, fmt.Errorf("%w: mailbox sequence counter is invalid", orchestration.ErrInvalidEvent)
	}
	sequence := nextSequence - 1
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_messages(message_id, team_id, sender_agent_id, recipient_agent_id, message_type, body, correlation_id, idempotency_key, sequence, delivery_state, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(messageID), string(spec.TeamID), string(spec.SenderAgentID), string(spec.RecipientAgentID), string(spec.Type), []byte(spec.Body), spec.CorrelationID, spec.IdempotencyKey, sequence, string(orchestration.DeliveryEnqueued), now.Format(time.RFC3339Nano)); err != nil {
		return orchestration.Message{}, fmt.Errorf("insert mailbox message: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamMailbox, ID: string(spec.RecipientAgentID), TeamID: spec.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventMailboxEnqueued, ActorAgentID: spec.SenderAgentID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{
			"message_id": messageID, "sender_agent_id": spec.SenderAgentID, "recipient_agent_id": spec.RecipientAgentID, "type": spec.Type, "body": spec.Body, "sequence": sequence,
		})},
	); err != nil {
		return orchestration.Message{}, fmt.Errorf("record mailbox enqueue: %w", err)
	}
	return orchestration.Message{ID: messageID, TeamID: spec.TeamID, SenderAgentID: spec.SenderAgentID, RecipientAgentID: spec.RecipientAgentID, Type: spec.Type, Body: append([]byte(nil), spec.Body...), CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Sequence: sequence, DeliveryState: orchestration.DeliveryEnqueued, CreatedAt: now}, nil
}

func (store *OrchestrationStore) FindMessage(ctx context.Context, teamID orchestration.TeamID, idempotencyKey string) (orchestration.Message, bool, error) {
	if err := teamID.Validate(); err != nil {
		return orchestration.Message{}, false, err
	}
	if idempotencyKey == "" {
		return orchestration.Message{}, false, nil
	}
	var messageID string
	err := store.db.QueryRowContext(ctx, `SELECT message_id FROM mailbox_messages WHERE team_id = ? AND idempotency_key = ?`, string(teamID), idempotencyKey).Scan(&messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.Message{}, false, nil
	}
	if err != nil {
		return orchestration.Message{}, false, err
	}
	message, err := mailboxMessageByID(ctx, store.db, orchestration.MessageID(messageID))
	return message, err == nil, err
}

func sameMessageRequest(message orchestration.Message, spec orchestration.MessageSpec) bool {
	return message.TeamID == spec.TeamID &&
		message.SenderAgentID == spec.SenderAgentID &&
		message.RecipientAgentID == spec.RecipientAgentID &&
		message.Type == spec.Type &&
		bytes.Equal(message.Body, spec.Body) &&
		message.CorrelationID == spec.CorrelationID
}

func isTerminalAgentState(state orchestration.AgentState) bool {
	return state == orchestration.AgentCompleted || state == orchestration.AgentFailed || state == orchestration.AgentCanceled
}

func (store *OrchestrationStore) ListMessages(ctx context.Context, query orchestration.MessageQuery) ([]orchestration.Message, error) {
	if err := query.TeamID.Validate(); err != nil {
		return nil, fmt.Errorf("message query team: %w", err)
	}
	if err := query.RecipientAgentID.Validate(); err != nil {
		return nil, fmt.Errorf("message query recipient: %w", err)
	}
	limit := query.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args := []any{string(query.TeamID), string(query.RecipientAgentID)}
	conditions := []string{"team_id = ?", "recipient_agent_id = ?"}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			if state != orchestration.DeliveryEnqueued && state != orchestration.DeliveryDelivered && state != orchestration.DeliveryAcknowledged {
				return nil, fmt.Errorf("%w: unsupported delivery state %q", orchestration.ErrInvalidEvent, state)
			}
			placeholders[index] = "?"
			args = append(args, string(state))
		}
		conditions = append(conditions, "delivery_state IN ("+strings.Join(placeholders, ", ")+")")
	}
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, `SELECT `+mailboxColumns+` FROM mailbox_messages WHERE `+strings.Join(conditions, " AND ")+` ORDER BY sequence ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list mailbox messages: %w", err)
	}
	defer rows.Close()
	messages := make([]orchestration.Message, 0)
	for rows.Next() {
		message, scanErr := scanMailboxMessage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mailbox messages: %w", err)
	}
	return messages, nil
}

func (store *OrchestrationStore) MarkDelivered(ctx context.Context, messageID orchestration.MessageID) (orchestration.Message, error) {
	return store.transitionMessage(ctx, messageID, orchestration.DeliveryDelivered)
}

func (store *OrchestrationStore) MarkAcknowledged(ctx context.Context, messageID orchestration.MessageID) (orchestration.Message, error) {
	return store.transitionMessage(ctx, messageID, orchestration.DeliveryAcknowledged)
}

func (store *OrchestrationStore) transitionMessage(ctx context.Context, messageID orchestration.MessageID, target orchestration.DeliveryState) (orchestration.Message, error) {
	if err := messageID.Validate(); err != nil {
		return orchestration.Message{}, fmt.Errorf("message ID: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return orchestration.Message{}, fmt.Errorf("begin mailbox transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	message, err := mailboxMessageByID(ctx, tx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.Message{}, fmt.Errorf("%w: message %s", orchestration.ErrNotFound, messageID)
	}
	if err != nil {
		return orchestration.Message{}, err
	}
	if message.DeliveryState == target {
		if err := tx.Commit(); err != nil {
			return orchestration.Message{}, fmt.Errorf("commit idempotent mailbox transition: %w", err)
		}
		return message, nil
	}
	if message.DeliveryState == orchestration.DeliveryAcknowledged {
		return orchestration.Message{}, fmt.Errorf("%w: cannot transition acknowledged message to %q", orchestration.ErrInvalidEvent, target)
	}
	if target == orchestration.DeliveryDelivered && message.DeliveryState != orchestration.DeliveryEnqueued {
		return orchestration.Message{}, fmt.Errorf("%w: cannot deliver message in state %q", orchestration.ErrInvalidEvent, message.DeliveryState)
	}
	if target == orchestration.DeliveryAcknowledged && message.DeliveryState != orchestration.DeliveryDelivered {
		return orchestration.Message{}, fmt.Errorf("%w: cannot acknowledge message in state %q", orchestration.ErrInvalidEvent, message.DeliveryState)
	}
	now := time.Now().UTC()
	column := "delivered_at"
	eventType := orchestration.EventMailboxDelivered
	if target == orchestration.DeliveryAcknowledged {
		column = "acknowledged_at"
		eventType = orchestration.EventMailboxAcknowledged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_messages SET delivery_state = ?, `+column+` = ? WHERE message_id = ?`, string(target), now.Format(time.RFC3339Nano), string(messageID)); err != nil {
		return orchestration.Message{}, fmt.Errorf("update mailbox message: %w", err)
	}
	if _, err := appendOrchestrationEvents(ctx, tx, orchestration.Stream{Type: orchestration.StreamMailbox, ID: string(message.RecipientAgentID), TeamID: message.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: eventType, ActorAgentID: message.RecipientAgentID, CorrelationID: message.CorrelationID, Payload: orchestration.MustJSONPayload(map[string]any{"message_id": message.ID, "from": message.DeliveryState, "to": target})},
	); err != nil {
		return orchestration.Message{}, fmt.Errorf("record mailbox transition: %w", err)
	}
	updated, err := mailboxMessageByID(ctx, tx, messageID)
	if err != nil {
		return orchestration.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.Message{}, fmt.Errorf("commit mailbox transition: %w", err)
	}
	return updated, nil
}

func mailboxMessageByID(ctx context.Context, query rowQuery, messageID orchestration.MessageID) (orchestration.Message, error) {
	return scanMailboxMessage(query.QueryRowContext(ctx, `SELECT `+mailboxColumns+` FROM mailbox_messages WHERE message_id = ?`, string(messageID)))
}

func scanMailboxMessage(scanner rowScanner) (orchestration.Message, error) {
	var message orchestration.Message
	var messageID, teamID, senderID, recipientID, messageType, body, correlationID, idempotencyKey, deliveryState, createdAt string
	var deliveredAt, acknowledgedAt, boundAt sql.NullString
	if err := scanner.Scan(&messageID, &teamID, &senderID, &recipientID, &messageType, &body, &correlationID, &idempotencyKey, &message.Sequence, &deliveryState, &createdAt, &deliveredAt, &acknowledgedAt, &message.ConsumerTurnID, &message.ConsumerRequest, &boundAt); err != nil {
		return orchestration.Message{}, err
	}
	var err error
	message.ID, message.TeamID = orchestration.MessageID(messageID), orchestration.TeamID(teamID)
	message.SenderAgentID, message.RecipientAgentID = orchestration.AgentID(senderID), orchestration.AgentID(recipientID)
	message.Type, message.DeliveryState = orchestration.MessageType(messageType), orchestration.DeliveryState(deliveryState)
	message.Body, message.CorrelationID, message.IdempotencyKey = []byte(body), correlationID, idempotencyKey
	message.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return orchestration.Message{}, fmt.Errorf("parse mailbox creation time: %w", err)
	}
	message.DeliveredAt, err = nullableTime(deliveredAt)
	if err != nil {
		return orchestration.Message{}, fmt.Errorf("parse mailbox delivery time: %w", err)
	}
	message.AcknowledgedAt, err = nullableTime(acknowledgedAt)
	if err != nil {
		return orchestration.Message{}, fmt.Errorf("parse mailbox acknowledgement time: %w", err)
	}
	message.BoundAt, err = nullableTime(boundAt)
	if err != nil {
		return orchestration.Message{}, fmt.Errorf("parse mailbox binding time: %w", err)
	}
	return message, nil
}

func nullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
