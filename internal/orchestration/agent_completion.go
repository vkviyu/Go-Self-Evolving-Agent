package orchestration

import (
	"context"
	"encoding/json"
	"strings"
)

// AgentCompletion freezes a returned run before its Task, delivery and Agent
// terminal transitions. Reconciliation finishes these effects without running
// the worker again. RunID fences a late completion from a newer assignment.
type AgentCompletion struct {
	AgentID AgentID
	RunID   RunID
	Result  AgentResult
}

// AgentCompletionRepository is an optional durable extension. Production
// SQLite stores implement it; small in-memory runner adapters need not.
type AgentCompletionRepository interface {
	// PrepareAgentCompletion records the completion facts needed to finish delivery after
	// an interruption.
	PrepareAgentCompletion(context.Context, AgentCompletion) (AgentCompletion, error)
	// PendingAgentCompletions returns unsettled completion records for the given team.
	PendingAgentCompletions(context.Context, TeamID) ([]AgentCompletion, error)
	// SettleAgentCompletion marks completion delivery for an agent and run as settled.
	SettleAgentCompletion(context.Context, AgentID, RunID) error
}

// ResultMessage is a deterministic notification of a durable Agent result.
// Large results stay intact in the Agent projection; the mailbox carries a
// bounded pointer-like summary so notification cannot block terminal commit.
func ResultMessage(agent AgentSnapshot) (MessageSpec, bool, error) {
	if agent.ParentAgentID == "" || agent.Result == nil {
		return MessageSpec{}, false, nil
	}
	result := *agent.Result
	body, err := json.Marshal(result)
	if err != nil {
		return MessageSpec{}, false, err
	}
	if len(body) > 1024*1024 {
		result.Payload, result.Blockers, result.Risks, result.Delivery = nil, nil, nil, nil
		result.Summary = boundedResultText(result.Summary) + "\n[Notification shortened; inspect the persisted Agent result for full details.]"
		result.Error = boundedResultText(result.Error)
		body, err = json.Marshal(result)
		if err != nil {
			return MessageSpec{}, false, err
		}
	}
	key := string(agent.CurrentRunID)
	if key == "" {
		key = string(agent.ID)
	}
	return MessageSpec{TeamID: agent.TeamID, SenderAgentID: agent.ID, RecipientAgentID: agent.ParentAgentID,
		Type: MessageResult, Body: body, CorrelationID: key, IdempotencyKey: "result:" + key}, true, nil
}

func boundedResultText(value string) string {
	const limit = 16 * 1024
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "")
}
