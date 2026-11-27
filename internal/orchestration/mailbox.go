package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type MessageType string

const (
	MessageDirective MessageType = "directive"
	MessageQuestion  MessageType = "question"
	MessageAnswer    MessageType = "answer"
	MessageContext   MessageType = "context"
	MessageStatus    MessageType = "status"
	MessageResult    MessageType = "result"
	MessageControl   MessageType = "control"
)

type DeliveryState string

const (
	DeliveryEnqueued     DeliveryState = "enqueued"
	DeliveryDelivered    DeliveryState = "delivered"
	DeliveryAcknowledged DeliveryState = "acknowledged"
)

type Message struct {
	ID               MessageID
	TeamID           TeamID
	SenderAgentID    AgentID
	RecipientAgentID AgentID
	Type             MessageType
	Body             json.RawMessage
	CorrelationID    string
	IdempotencyKey   string
	Sequence         uint64
	DeliveryState    DeliveryState
	CreatedAt        time.Time
	DeliveredAt      *time.Time
	AcknowledgedAt   *time.Time
	// ConsumerTurnID names the turn that received the message, and
	// ConsumerRequest the model request of that turn. Both are empty while
	// the message is available.
	ConsumerTurnID  string
	ConsumerRequest uint64
	BoundAt         *time.Time
}

// Available reports whether a turn may still claim the message.
func (message Message) Available() bool {
	return message.DeliveryState == DeliveryEnqueued || (message.DeliveryState == DeliveryDelivered && message.ConsumerTurnID == "")
}

type MessageSpec struct {
	TeamID           TeamID
	SenderAgentID    AgentID
	RecipientAgentID AgentID
	Type             MessageType
	Body             json.RawMessage
	CorrelationID    string
	IdempotencyKey   string
}

func (spec MessageSpec) Validate() error {
	if err := spec.TeamID.Validate(); err != nil {
		return fmt.Errorf("message team: %w", err)
	}
	if err := spec.SenderAgentID.Validate(); err != nil {
		return fmt.Errorf("message sender: %w", err)
	}
	if err := spec.RecipientAgentID.Validate(); err != nil {
		return fmt.Errorf("message recipient: %w", err)
	}
	if spec.SenderAgentID == spec.RecipientAgentID {
		return errors.New("message sender and recipient must differ")
	}
	switch spec.Type {
	case MessageDirective, MessageQuestion, MessageAnswer, MessageContext, MessageStatus, MessageResult, MessageControl:
	default:
		return fmt.Errorf("unsupported message type %q", spec.Type)
	}
	if len(spec.Body) == 0 || !json.Valid(spec.Body) {
		return errors.New("message body must be valid JSON")
	}
	if len(spec.Body) > 1024*1024 {
		return errors.New("message body exceeds the 1 MiB limit")
	}
	return nil
}

type MessageQuery struct {
	TeamID           TeamID
	RecipientAgentID AgentID
	States           []DeliveryState
	Limit            int
}

type MailboxRepository interface {
	// Enqueue records a message for later delivery to the recipient's mailbox.
	Enqueue(ctx context.Context, spec MessageSpec) (Message, error)
	// ListMessages returns mailbox entries matching the supplied query.
	ListMessages(ctx context.Context, query MessageQuery) ([]Message, error)
	// MarkDelivered records that the message has been delivered to its recipient.
	MarkDelivered(ctx context.Context, messageID MessageID) (Message, error)
	// MarkAcknowledged records that the recipient has acknowledged the message.
	MarkAcknowledged(ctx context.Context, messageID MessageID) (Message, error)
}

// MessageClaim binds the available messages of one recipient to the turn
// that injects them into a model request.
type MessageClaim struct {
	TeamID           TeamID
	RecipientAgentID AgentID
	TurnID           string
	Request          uint64
	Limit            int
}

// Validate checks the claim identity.
func (claim MessageClaim) Validate() error {
	if err := claim.TeamID.Validate(); err != nil {
		return fmt.Errorf("message claim team: %w", err)
	}
	if err := claim.RecipientAgentID.Validate(); err != nil {
		return fmt.Errorf("message claim recipient: %w", err)
	}
	if claim.TurnID == "" {
		return errors.New("message claim requires a turn ID")
	}
	return nil
}

// MailboxLedger records which turn consumed each message. A message bound to
// a turn is acknowledged when that turn commits, or released for another
// turn when it cannot commit. A crash after the commit therefore never makes
// the message available again.
type MailboxLedger interface {
	// ClaimMessages binds every available message to the claim's turn and
	// returns them together with messages already bound to that turn.
	ClaimMessages(ctx context.Context, claim MessageClaim) ([]Message, error)
	// AcknowledgeTurn acknowledges the messages bound to turnID.
	AcknowledgeTurn(ctx context.Context, teamID TeamID, recipient AgentID, turnID string) (int, error)
	// ReleaseTurn makes the messages bound to turnID available again.
	ReleaseTurn(ctx context.Context, teamID TeamID, recipient AgentID, turnID string) (int, error)
}

// MessageRequestFinder resolves an idempotent mailbox write without scanning a
// recipient queue. It is used to recover compound orchestration operations.
type MessageRequestFinder interface {
	// FindMessage resolves a team-scoped message idempotency key and reports whether it
	// exists.
	FindMessage(context.Context, TeamID, string) (Message, bool, error)
}
