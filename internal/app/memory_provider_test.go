package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
	"github.com/vkviyu/go-self-evolving-agent/internal/approval"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// TestMemoryRealProviderDecision exercises primary-model choice with synthetic
// prompts and isolated storage. Provider credentials and responses are not logged.
func TestMemoryRealProviderDecision(t *testing.T) {
	if os.Getenv("GSEA_TEST_REAL_MEMORY") != "1" {
		t.Skip("opt in with GSEA_TEST_REAL_MEMORY=1 to call the configured provider")
	}
	base, err := config.Load(config.LoadOptions{WorkingDirectory: temporalRepo(t)})
	if err != nil {
		t.Fatal("cannot load configured provider")
	}
	if strings.EqualFold(base.Model.Provider, "openai") {
		key := base.Model.APIKeyEnvironment
		if key == "" {
			key = "OPENAI_API_KEY"
		}
		if strings.TrimSpace(os.Getenv(key)) == "" {
			t.Fatal("configured provider credential is absent; value not logged")
		}
	}
	providerTimeout := 45 * time.Second
	if value := os.Getenv("GSEA_TEST_REAL_MEMORY_TIMEOUT"); value != "" {
		providerTimeout, err = time.ParseDuration(value)
		if err != nil || providerTimeout <= 0 || providerTimeout > 5*time.Minute {
			t.Fatal("GSEA_TEST_REAL_MEMORY_TIMEOUT must be positive and at most 5m")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*providerTimeout+time.Minute)
	defer cancel()
	chat, err := modelgateway.New(ctx, modelgateway.Config{
		Provider: modelgateway.Provider(base.Model.Provider), WireAPI: modelgateway.WireAPI(base.Model.WireAPI),
		Model: base.Model.Name, TokenizerEncoding: base.Model.TokenizerEncoding,
		BaseURL: base.Model.BaseURL, APIKeyEnvironment: base.Model.APIKeyEnvironment,
		ReasoningEffort: base.Model.ReasoningEffort, Timeout: providerTimeout,
	})
	if err != nil {
		t.Fatalf("provider initialization failed (%s)", temporalErrorClass(err))
	}
	a := temporalOpen(t, temporalConfig(t, base, temporalBinary(t), t.TempDir()))
	for _, test := range []struct {
		name, prompt string
		count        bool
	}{
		{"direct", "你好，请简单打个招呼。", false},
		{"memory", "你当前有几条可复用记忆？", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := session.ID("real-memory-" + test.name)
			root, _, err := a.EnsureRootThread(ctx, id)
			if err != nil {
				t.Fatal("cannot create isolated test session")
			}
			factory, err := a.NewAgentFactory(chat, agentruntime.Config{
				ActorScope: root.ActorScope(), Instruction: "Answer the user's request concisely.",
				MaxIterations: 4, MaxOutputTokens: 1024,
			}, approval.NewToolAllowlistApprover())
			if err != nil {
				t.Fatal("cannot construct test factory")
			}
			turn, err := factory.NewTurn(ctx, id)
			if err != nil {
				t.Fatal("cannot create test turn")
			}
			start := time.Now()
			result, runErr := turn.Run(ctx, []*schema.Message{schema.UserMessage(test.prompt)}, func(_ context.Context, event agentruntime.Event) error {
				switch event.Type {
				case agentruntime.EventModelRequestStarted, agentruntime.EventModelResponseStarted,
					agentruntime.EventToolExecutionStarted, agentruntime.EventToolExecutionSucceeded,
					agentruntime.EventToolExecutionFailed, agentruntime.EventToolOutcomeUnknown:
					t.Logf("phase=%s elapsed=%s", event.Type, time.Since(start).Round(time.Millisecond))
				}
				return nil
			})
			requests, countQueries, tools := 0, 0, 0
			err = session.VisitEvents(ctx, a.events, id, func(event session.Event) error {
				if event.Type == session.EventAssistantMessage || event.Type == session.EventModelFailed {
					var payload struct {
						RequestID string          `json:"request_id"`
						Transport json.RawMessage `json:"transport"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						return err
					}
					if len(payload.Transport) != 0 {
						t.Logf("request=%s event=%s transport=%s", payload.RequestID, event.Type, payload.Transport)
					}
				}
				if event.Type == session.EventModelRequested {
					var payload struct {
						Purpose string `json:"purpose"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						return err
					}
					if payload.Purpose != "" {
						t.Error("unexpected auxiliary model request")
					}
					requests++
				}
				if event.Type == session.EventToolSucceeded {
					tools++
					var payload struct {
						Tool   string `json:"tool"`
						Result struct {
							Operation string `json:"operation"`
							Count     *int   `json:"count"`
						} `json:"result"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						return err
					}
					if payload.Tool == "memory_query" && payload.Result.Operation == "count" &&
						payload.Result.Count != nil && *payload.Result.Count == 0 {
						countQueries++
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal("cannot inspect durable decision evidence")
			}
			if runErr != nil {
				t.Fatalf("primary turn failed (%s); requests=%d successful_tools=%d count_queries=%d", temporalErrorClass(runErr), requests, tools, countQueries)
			}
			if result.Final == nil || strings.TrimSpace(result.Final.Content) == "" || result.ExperienceMaintenance != nil {
				t.Fatal("missing final answer or unexpected automatic maintenance")
			}
			if test.count && countQueries == 0 {
				t.Fatal("real model did not query the authoritative memory count")
			}
			if !test.count && (requests != 1 || tools != 0) {
				t.Fatalf("simple greeting used unnecessary work: requests=%d tools=%d", requests, tools)
			}
			t.Logf("primary_requests=%d successful_tools=%d count_queries=%d elapsed=%s",
				requests, tools, countQueries, time.Since(start).Round(time.Millisecond))
		})
	}
}
