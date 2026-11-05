package conversation

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentgoal"
)

func goalContextText(goal agentgoal.Goal) string {
	if goal.Contract == nil {
		return goal.Text
	}
	contract, _ := json.Marshal(goal.Contract)
	return goal.Text + "\nImmutable acceptance contract: " + string(contract)
}

func continuationPrompt(goal agentgoal.Goal, verdict agentgoal.Verdict) string {
	feedback := strings.TrimSpace(verdict.Feedback)
	if feedback == "" {
		feedback = strings.TrimSpace(verdict.Reason)
	}
	contract, _ := json.Marshal(goal.Contract)
	return fmt.Sprintf("[Goal Mode continuation: round %d was not accepted]\nOriginal user request: %s\nImmutable task contract: %s\nEvaluator feedback: %s\nAddress specific unmet criteria within the original user authorization. Do not broaden scope or repeat successful side effects. If new authority is required, report blocked and ask the user. Gather evidence and submit goal_report before the next candidate answer.", goal.Round, goal.Text, contract, feedback)
}
