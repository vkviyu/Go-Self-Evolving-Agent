package agentmanager

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// Send durably enqueues a message and returns once it is safe to retry.
func (supervisor *Supervisor) Send(ctx context.Context, spec orchestration.MessageSpec) (orchestration.Message, error) {
	if err := supervisor.ensureOpen(); err != nil {
		return orchestration.Message{}, err
	}
	if supervisor.mailbox == nil {
		return orchestration.Message{}, errors.New("Agent mailbox is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sender, err := supervisor.agents.GetAgent(ctx, spec.SenderAgentID)
	if err != nil {
		return orchestration.Message{}, err
	}
	if profile, ok := supervisor.agentProfile(sender); ok && !profile.CanSendMessages {
		return orchestration.Message{}, errors.New("Agent profile does not allow sending messages")
	}
	message, err := supervisor.mailbox.Enqueue(ctx, spec)
	if err == nil {
		supervisor.signal()
	}
	return message, err
}

// loadMessages marks pending messages as delivered and returns them in
// recipient sequence order. Delivery is at-least-once: the run acknowledges
// them only after the consuming turn has been durably completed.
func (supervisor *Supervisor) loadMessages(ctx context.Context, agentID orchestration.AgentID, teamID orchestration.TeamID) ([]orchestration.Message, error) {
	if supervisor.mailbox == nil {
		return nil, nil
	}
	messages, err := supervisor.mailbox.ListMessages(ctx, orchestration.MessageQuery{
		TeamID: teamID, RecipientAgentID: agentID, States: []orchestration.DeliveryState{orchestration.DeliveryEnqueued, orchestration.DeliveryDelivered},
	})
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if _, err := supervisor.mailbox.MarkDelivered(ctx, message.ID); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

// acknowledgeMessages completes delivery. A message may be acknowledged more
// than once, which makes retrying a completed turn safe after an interruption.
func (supervisor *Supervisor) acknowledgeMessages(ctx context.Context, messages []orchestration.Message) error {
	if supervisor.mailbox == nil {
		return nil
	}
	for _, message := range messages {
		if _, err := supervisor.mailbox.MarkAcknowledged(ctx, message.ID); err != nil {
			return err
		}
	}
	return nil
}
