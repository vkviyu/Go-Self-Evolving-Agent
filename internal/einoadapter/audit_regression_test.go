package einoadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/session/memorystore"
	appTool "github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

func TestAdapterRequiresDurableToolCallIdentityAndReturnsPolicyDenial(t *testing.T) {
	registry := appTool.NewRegistry()
	if _, err := registry.Activate(binding("v1")); err != nil {
		t.Fatal(err)
	}
	engine, err := policy.DefaultDeny()
	if err != nil {
		t.Fatal(err)
	}
	events := memorystore.New()
	pipeline, err := appTool.NewPipeline(appTool.PipelineConfig{Registry: registry, Policy: engine, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewToolsWithExecution(pipeline, registry.Snapshot(), "session", "runtime", "turn", orchestration.ActorScope{}, orchestration.ExecutionBinding{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].(einotool.InvokableTool).InvokableRun(context.Background(), `{}`); err == nil {
		t.Fatal("missing model call ID accepted")
	}
	result, err := invokeThroughEino(context.Background(), tools[0], `{}`)
	if err != nil {
		t.Fatalf("policy denial aborted the model loop: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(result), &payload); err != nil || payload["error"] != appTool.ErrPolicyDenied.Error() {
		t.Fatalf("unexpected policy denial: %s %v", result, err)
	}
}

func TestRecoverableDenialCannotHideDurabilityFailure(t *testing.T) {
	for _, denial := range []error{appTool.ErrPolicyDenied, appTool.ErrApprovalDenied} {
		if !onlyDenial(errors.Join(fmt.Errorf("decision: %w", denial)), denial) {
			t.Fatal("wrapped denial was not recognized")
		}
		if onlyDenial(errors.Join(denial, errors.New("audit write failed")), denial) {
			t.Fatal("durability failure hidden by denial")
		}
	}
}
