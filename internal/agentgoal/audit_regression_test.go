package agentgoal

import (
	"context"
	"sync"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestAdmissionGuardBindingIsRaceSafe(t *testing.T) {
	guard := NewAdmissionGuard(nil, "session", "turn", false)
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 1000 {
			guard.BindTurn("turn")
		}
	})
	workers.Go(func() {
		for range 1000 {
			if err := guard.BeforeCall(context.Background(), tool.Call{SessionID: "session", TurnID: "turn"}, tool.Descriptor{RiskClass: tool.RiskRead}); err != nil {
				t.Error(err)
			}
		}
	})
	workers.Wait()
}
