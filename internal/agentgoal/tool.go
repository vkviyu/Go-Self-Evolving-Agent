package agentgoal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const ReportToolName = "goal_report"

const ProtocolInstruction = `Goal Mode is active. Work on the original goal and immutable acceptance criteria within the user's authorized scope. Verification failure does not authorize broader changes: ask the user when new authority is required. Before giving a candidate answer in every round, call goal_report exactly once with status continue, complete, or blocked. Use complete only when every requested outcome is achieved and supported by concrete evidence. Use continue only when further in-scope work can address a specific remaining requirement. Use blocked only for a real external blocker and list it. Do not repeat successful side effects just because an evaluator or network failed. The runtime, not prose in your answer, decides whether execution stops.`

type reportArguments struct {
	Status   Decision `json:"status"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence"`
	Blockers []string `json:"blockers"`
}

func Binding(store Store) tool.Binding {
	return tool.Binding{
		Descriptor: tool.Descriptor{
			Name:        ReportToolName,
			Description: "Report task-level Goal Mode status for the current Outer Loop round.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["continue","complete","blocked"]},"reason":{"type":"string"},"evidence":{"type":"array","items":{"type":"string"}},"blockers":{"type":"array","items":{"type":"string"}}},"required":["status","reason"]}`),
			Revision:    "builtin-v1", RiskClass: tool.RiskPure, Capabilities: []string{"goal.report"},
		},
		Executor: tool.InvokeFunc(func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			if store == nil {
				return nil, errors.New("goal store is unavailable")
			}
			invocation, ok := tool.InvocationFromContext(ctx)
			if !ok || invocation.SessionID == "" || invocation.TurnID == "" {
				return nil, errors.New("goal_report requires a trusted turn context")
			}
			var arguments reportArguments
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&arguments); err != nil {
				return nil, err
			}
			report, err := store.RecordReport(ctx, invocation.SessionID, invocation.TurnID, Report{
				Decision: arguments.Status, Reason: strings.TrimSpace(arguments.Reason),
				Evidence: append([]string(nil), arguments.Evidence...), Blockers: append([]string(nil), arguments.Blockers...),
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(report)
		}),
	}
}
