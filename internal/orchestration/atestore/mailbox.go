package atestore

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

func (store *Store) Enqueue(ctx context.Context, spec orchestration.MessageSpec) (result orchestration.Message, err error) {
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error { var e error; result, e = enqueueTx(tx, spec, now); return e })
	return
}
func findMessageTx(tx *ateauthority.Tx, team orchestration.TeamID, key string) (orchestration.Message, bool, error) {
	var id orchestration.MessageID
	found, err := tx.Get(recordKey("message-receipt", scopedKey(team, key)), &id)
	if err != nil || !found {
		return orchestration.Message{}, false, err
	}
	message, err := getRecord[orchestration.Message](tx, "message", string(id))
	return message, err == nil, err
}
func (store *Store) FindMessage(ctx context.Context, team orchestration.TeamID, key string) (result orchestration.Message, found bool, err error) {
	if err = team.Validate(); err != nil {
		return
	}
	if key == "" {
		return
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, found, e = findMessageTx(tx, team, key)
		return e
	})
	return
}
func enqueueTx(tx *ateauthority.Tx, spec orchestration.MessageSpec, now time.Time) (orchestration.Message, error) {
	if err := spec.Validate(); err != nil {
		return orchestration.Message{}, err
	}
	if spec.IdempotencyKey != "" {
		message, found, err := findMessageTx(tx, spec.TeamID, spec.IdempotencyKey)
		if err != nil {
			return message, err
		}
		if found {
			if message.TeamID != spec.TeamID || message.SenderAgentID != spec.SenderAgentID || message.RecipientAgentID != spec.RecipientAgentID || message.Type != spec.Type || !bytes.Equal(message.Body, spec.Body) || message.CorrelationID != spec.CorrelationID {
				return orchestration.Message{}, fmt.Errorf("%w: mailbox key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
			}
			return message, nil
		}
	}
	sender, err := agentTx(tx, spec.SenderAgentID)
	if err != nil {
		return orchestration.Message{}, err
	}
	recipient, err := agentTx(tx, spec.RecipientAgentID)
	if err != nil {
		return orchestration.Message{}, err
	}
	if sender.TeamID != spec.TeamID || recipient.TeamID != spec.TeamID {
		return orchestration.Message{}, fmt.Errorf("%w: mailbox Agent belongs to another team", orchestration.ErrInvalidEvent)
	}
	if sender.ParentAgentID != recipient.ID && recipient.ParentAgentID != sender.ID {
		return orchestration.Message{}, fmt.Errorf("%w: mailbox only supports parent-child delivery", orchestration.ErrInvalidEvent)
	}
	if spec.Type != orchestration.MessageResult && spec.Type != orchestration.MessageControl && (isTerminalAgentState(sender.State) || isTerminalAgentState(recipient.State)) {
		return orchestration.Message{}, fmt.Errorf("%w: terminal Agent cannot send or receive ordinary messages", orchestration.ErrInvalidEvent)
	}
	id, err := orchestration.NewMessageID()
	if err != nil {
		return orchestration.Message{}, err
	}
	var sequence uint64
	if _, err := tx.Get(recordKey("mailbox-counter", string(recipient.ID)), &sequence); err != nil {
		return orchestration.Message{}, err
	}
	sequence++
	if sequence == 0 {
		return orchestration.Message{}, fmt.Errorf("%w: mailbox sequence overflow", orchestration.ErrInvalidEvent)
	}
	message := orchestration.Message{ID: id, TeamID: spec.TeamID, SenderAgentID: sender.ID, RecipientAgentID: recipient.ID, Type: spec.Type, Body: append([]byte(nil), spec.Body...), CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Sequence: sequence, DeliveryState: orchestration.DeliveryEnqueued, CreatedAt: now}
	if err := createRecord(tx, "message", string(id), message); err != nil {
		return message, err
	}
	if err := putRecord(tx, "mailbox-counter", string(recipient.ID), sequence); err != nil {
		return message, err
	}
	if spec.IdempotencyKey != "" {
		if err := createRecord(tx, "message-receipt", scopedKey(spec.TeamID, spec.IdempotencyKey), id); err != nil {
			return message, err
		}
	}
	_, err = appendEvents(tx, orchestration.Stream{Type: orchestration.StreamMailbox, ID: string(recipient.ID), TeamID: spec.TeamID}, orchestration.AnySequence,
		orchestration.PendingEvent{Type: orchestration.EventMailboxEnqueued, ActorAgentID: sender.ID, CorrelationID: spec.CorrelationID, IdempotencyKey: spec.IdempotencyKey, Payload: orchestration.MustJSONPayload(map[string]any{
			"message_id": id, "sender_agent_id": sender.ID, "recipient_agent_id": recipient.ID, "type": spec.Type, "body": spec.Body, "sequence": sequence})})
	return message, err
}
func mailboxLimit(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 100
	}
	return limit
}
func messagesTx(tx *ateauthority.Tx, match func(orchestration.Message) bool) ([]orchestration.Message, error) {
	all, err := listRecords[orchestration.Message](tx, "message")
	if err != nil {
		return nil, err
	}
	result := make([]orchestration.Message, 0)
	for _, message := range all {
		if match(message) {
			result = append(result, message)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result, nil
}
func (store *Store) ListMessages(ctx context.Context, query orchestration.MessageQuery) (result []orchestration.Message, err error) {
	if err = query.TeamID.Validate(); err != nil {
		return
	}
	if err = query.RecipientAgentID.Validate(); err != nil {
		return
	}
	for _, state := range query.States {
		if state != orchestration.DeliveryEnqueued && state != orchestration.DeliveryDelivered && state != orchestration.DeliveryAcknowledged {
			return nil, fmt.Errorf("%w: unsupported delivery state %q", orchestration.ErrInvalidEvent, state)
		}
	}
	err = store.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var e error
		result, e = messagesTx(tx, func(m orchestration.Message) bool {
			return m.TeamID == query.TeamID && m.RecipientAgentID == query.RecipientAgentID && (len(query.States) == 0 || slices.Contains(query.States, m.DeliveryState))
		})
		if len(result) > mailboxLimit(query.Limit) {
			result = result[:mailboxLimit(query.Limit)]
		}
		return e
	})
	return
}
func mailboxEvent(tx *ateauthority.Tx, message orchestration.Message, kind orchestration.EventType, payload map[string]any) error {
	_, err := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamMailbox, ID: string(message.RecipientAgentID), TeamID: message.TeamID}, orchestration.AnySequence, orchestration.PendingEvent{Type: kind, ActorAgentID: message.RecipientAgentID, CorrelationID: message.CorrelationID, Payload: orchestration.MustJSONPayload(payload)})
	return err
}
func (store *Store) MarkDelivered(ctx context.Context, id orchestration.MessageID) (orchestration.Message, error) {
	return store.transitionMessage(ctx, id, orchestration.DeliveryDelivered)
}
func (store *Store) MarkAcknowledged(ctx context.Context, id orchestration.MessageID) (orchestration.Message, error) {
	return store.transitionMessage(ctx, id, orchestration.DeliveryAcknowledged)
}
func (store *Store) transitionMessage(ctx context.Context, id orchestration.MessageID, target orchestration.DeliveryState) (result orchestration.Message, err error) {
	if err = id.Validate(); err != nil {
		return
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		message, e := getRecord[orchestration.Message](tx, "message", string(id))
		if e != nil {
			return e
		}
		if message.DeliveryState == target {
			result = message
			return nil
		}
		if (target == orchestration.DeliveryDelivered && message.DeliveryState != orchestration.DeliveryEnqueued) || (target == orchestration.DeliveryAcknowledged && message.DeliveryState != orchestration.DeliveryDelivered) {
			return fmt.Errorf("%w: cannot transition message from %q to %q", orchestration.ErrInvalidEvent, message.DeliveryState, target)
		}
		kind := orchestration.EventMailboxDelivered
		if target == orchestration.DeliveryAcknowledged {
			kind = orchestration.EventMailboxAcknowledged
			message.AcknowledgedAt = &now
		} else {
			message.DeliveredAt = &now
		}
		if e := mailboxEvent(tx, message, kind, map[string]any{"message_id": id, "from": message.DeliveryState, "to": target}); e != nil {
			return e
		}
		message.DeliveryState = target
		if e := putRecord(tx, "message", string(id), message); e != nil {
			return e
		}
		result = message
		return nil
	})
	return
}
func (store *Store) ClaimMessages(ctx context.Context, claim orchestration.MessageClaim) (result []orchestration.Message, err error) {
	if err = claim.Validate(); err != nil {
		return
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		messages, e := messagesTx(tx, func(m orchestration.Message) bool {
			return m.TeamID == claim.TeamID && m.RecipientAgentID == claim.RecipientAgentID && (m.DeliveryState == orchestration.DeliveryEnqueued || (m.DeliveryState == orchestration.DeliveryDelivered && (m.ConsumerTurnID == "" || m.ConsumerTurnID == claim.TurnID)))
		})
		if e != nil {
			return e
		}
		if len(messages) > mailboxLimit(claim.Limit) {
			messages = messages[:mailboxLimit(claim.Limit)]
		}
		for i := range messages {
			m := &messages[i]
			if m.ConsumerTurnID == claim.TurnID {
				continue
			}
			if e := mailboxEvent(tx, *m, orchestration.EventMailboxBound, map[string]any{"message_id": m.ID, "from": m.DeliveryState, "turn_id": claim.TurnID, "request": claim.Request}); e != nil {
				return e
			}
			if m.DeliveredAt == nil {
				m.DeliveredAt = &now
			}
			m.DeliveryState, m.ConsumerTurnID, m.ConsumerRequest, m.BoundAt = orchestration.DeliveryDelivered, claim.TurnID, claim.Request, &now
			if e := putRecord(tx, "message", string(m.ID), m); e != nil {
				return e
			}
		}
		result = messages
		return nil
	})
	return
}
func (store *Store) AcknowledgeTurn(ctx context.Context, team orchestration.TeamID, recipient orchestration.AgentID, turn string) (int, error) {
	return store.settleTurn(ctx, team, recipient, turn, true)
}
func (store *Store) ReleaseTurn(ctx context.Context, team orchestration.TeamID, recipient orchestration.AgentID, turn string) (int, error) {
	return store.settleTurn(ctx, team, recipient, turn, false)
}
func (store *Store) settleTurn(ctx context.Context, team orchestration.TeamID, recipient orchestration.AgentID, turn string, ack bool) (count int, err error) {
	if err = (orchestration.MessageClaim{TeamID: team, RecipientAgentID: recipient, TurnID: turn}).Validate(); err != nil {
		return
	}
	now := time.Now().UTC()
	err = store.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		messages, e := messagesTx(tx, func(m orchestration.Message) bool {
			return m.TeamID == team && m.RecipientAgentID == recipient && m.ConsumerTurnID == turn && m.DeliveryState == orchestration.DeliveryDelivered
		})
		if e != nil {
			return e
		}
		for _, m := range messages {
			kind := orchestration.EventMailboxReleased
			if ack {
				kind = orchestration.EventMailboxAcknowledged
				m.DeliveryState, m.AcknowledgedAt = orchestration.DeliveryAcknowledged, &now
			} else {
				m.ConsumerTurnID, m.ConsumerRequest, m.BoundAt = "", 0, nil
			}
			if e := putRecord(tx, "message", string(m.ID), m); e != nil {
				return e
			}
			if e := mailboxEvent(tx, m, kind, map[string]any{"message_id": m.ID, "turn_id": turn}); e != nil {
				return e
			}
		}
		count = len(messages)
		return nil
	})
	return
}
