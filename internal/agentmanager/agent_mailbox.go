package agentmanager

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// ErrMailboxLedgerUnavailable reports a mailbox store without consumption
// records.
var ErrMailboxLedgerUnavailable = errors.New("Agent mailbox ledger is unavailable")

// AgentMailbox binds one Agent's mailbox to the turns of its conversation.
// Each message is recorded against the turn that received it, so a turn that
// committed before a crash never receives the message again.
type AgentMailbox struct {
	supervisor *Supervisor
	ledger     orchestration.MailboxLedger
	teamID     orchestration.TeamID
	agentID    orchestration.AgentID
}

// AgentMailbox returns the turn-scoped mailbox of agentID.
func (supervisor *Supervisor) AgentMailbox(ctx context.Context, agentID orchestration.AgentID) (*AgentMailbox, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return nil, err
	}
	ledger, ok := supervisor.mailbox.(orchestration.MailboxLedger)
	if !ok {
		return nil, ErrMailboxLedgerUnavailable
	}
	agent, err := supervisor.agents.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return &AgentMailbox{supervisor: supervisor, ledger: ledger, teamID: agent.TeamID, agentID: agent.ID}, nil
}

// Claim binds the available messages to turnID for its request.
func (mailbox *AgentMailbox) Claim(ctx context.Context, turnID string, request uint64) ([]orchestration.Message, error) {
	return mailbox.ledger.ClaimMessages(ctx, orchestration.MessageClaim{TeamID: mailbox.teamID, RecipientAgentID: mailbox.agentID, TurnID: turnID, Request: request})
}

// Acknowledge completes delivery of the messages a committed turn consumed.
func (mailbox *AgentMailbox) Acknowledge(ctx context.Context, turnID string) error {
	_, err := mailbox.ledger.AcknowledgeTurn(ctx, mailbox.teamID, mailbox.agentID, turnID)
	return err
}

// Release makes the messages of an uncommitted turn available again.
func (mailbox *AgentMailbox) Release(ctx context.Context, turnID string) error {
	count, err := mailbox.ledger.ReleaseTurn(ctx, mailbox.teamID, mailbox.agentID, turnID)
	if err == nil && count > 0 {
		mailbox.supervisor.signal()
	}
	return err
}

// BoundTurns lists the turns that hold messages awaiting settlement.
func (mailbox *AgentMailbox) BoundTurns(ctx context.Context) ([]string, error) {
	messages, err := mailbox.list(ctx, orchestration.DeliveryDelivered)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var turns []string
	for _, message := range messages {
		if message.ConsumerTurnID != "" && !seen[message.ConsumerTurnID] {
			seen[message.ConsumerTurnID] = true
			turns = append(turns, message.ConsumerTurnID)
		}
	}
	return turns, nil
}

// Available counts the messages a new turn could claim.
func (mailbox *AgentMailbox) Available(ctx context.Context) (int, error) {
	messages, err := mailbox.list(ctx, orchestration.DeliveryEnqueued, orchestration.DeliveryDelivered)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, message := range messages {
		if message.Available() {
			count++
		}
	}
	return count, nil
}

// list returns up to 1000 messages of the Agent in the given states.
func (mailbox *AgentMailbox) list(ctx context.Context, states ...orchestration.DeliveryState) ([]orchestration.Message, error) {
	return mailbox.supervisor.mailbox.ListMessages(ctx, orchestration.MessageQuery{
		TeamID: mailbox.teamID, RecipientAgentID: mailbox.agentID, States: states, Limit: 1000,
	})
}
