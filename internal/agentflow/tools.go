package agentflow

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const (
	BudgetToolName = "runtime_budget"
	HumanToolName  = "human_confirm"
)

type BudgetReader interface {
	Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error)
}

func ControlBindings(reader BudgetReader) []tool.Binding {
	return []tool.Binding{
		{
			Descriptor: tool.Descriptor{
				Name: BudgetToolName, Description: "Read the current Team execution budget and remaining capacity.",
				InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
				Revision:    "builtin-v1", RiskClass: tool.RiskRead, Capabilities: []string{"flow.budget"},
			},
			Executor: tool.InvokeFunc(func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				if reader == nil {
					return nil, errors.New("budget reader is unavailable")
				}
				actor, ok := orchestration.ActorScopeFromContext(ctx)
				if !ok || actor.IsZero() {
					return nil, errors.New("runtime_budget requires an actor scope")
				}
				snapshot, err := reader.Budget(ctx, actor.TeamID)
				if err != nil {
					return nil, err
				}
				return json.Marshal(budgetView(snapshot))
			}),
		},
		{
			Descriptor: tool.Descriptor{
				Name: HumanToolName, Description: "Request an explicit human decision at a Flow fallback boundary.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`),
				Revision:    "builtin-v1", RiskClass: tool.RiskRead, Capabilities: []string{"flow.human"},
			},
			Executor: tool.InvokeFunc(func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
				var request struct {
					Question string `json:"question"`
				}
				if err := json.Unmarshal(raw, &request); err != nil || request.Question == "" {
					return nil, errors.New("human confirmation question is required")
				}
				return json.Marshal(map[string]any{"approved": true, "question": request.Question})
			}),
		},
	}
}

type BudgetView struct {
	Snapshot  tokenusage.BudgetSnapshot `json:"snapshot"`
	Remaining tokenusage.BudgetLimits   `json:"remaining"`
}

func budgetView(snapshot tokenusage.BudgetSnapshot) BudgetView {
	remaining := tokenusage.BudgetLimits{
		MaxInputTokens:  remainingInt(snapshot.Limits.MaxInputTokens, snapshot.InputTokens+snapshot.ReservedInputTokens),
		MaxOutputTokens: remainingInt(snapshot.Limits.MaxOutputTokens, snapshot.OutputTokens+snapshot.ReservedOutputTokens),
		MaxTotalTokens:  remainingInt(snapshot.Limits.MaxTotalTokens, snapshot.TotalTokens+snapshot.ReservedTotalTokens),
		MaxCostMicros:   remainingInt64(snapshot.Limits.MaxCostMicros, snapshot.CostMicros+snapshot.ReservedCostMicros),
	}
	return BudgetView{Snapshot: snapshot, Remaining: remaining}
}

func remainingInt(limit, used int) int {
	if limit <= 0 {
		return 0
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

func remainingInt64(limit, used int64) int64 {
	if limit <= 0 || used >= limit {
		return 0
	}
	return limit - used
}
