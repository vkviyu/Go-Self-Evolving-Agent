package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	sessionate "github.com/vkviyu/go-self-evolving-agent/internal/session/atestore"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type (
	toolReceiptFault struct {
		durable bool
		fired   bool
		key     api.RequestKey
	}
	lostToolReceipt struct {
		*client.Client
		fault *toolReceiptFault
		lost  bool
	}
)

// Commit loses only the outcome receipt, after the executor has already run.
func (c *lostToolReceipt) Commit(ctx context.Context, request api.CommitRequest) (api.CommitResult, error) {
	command, _ := agentprofile.DecodeCommand(request.Body)
	target := false
	if command.AppendSessionEvents != nil {
		for _, event := range command.AppendSessionEvents.Events {
			var envelope session.Event
			if json.Unmarshal(event.Payload, &envelope) == nil && envelope.Type == session.EventToolSucceeded {
				target = true
			}
		}
	}
	if !target || c.fault.fired {
		return c.Client.Commit(ctx, request)
	}
	c.fault.fired, c.fault.key = true, request.Key()
	if c.fault.durable {
		result, err := c.Client.Commit(ctx, request)
		if err != nil || result.Code != api.CodeDurableAck {
			return result, err
		}
	}
	c.lost = true
	return api.CommitResult{}, context.DeadlineExceeded
}

// LookupRequest models a lease that expired before the immediate lookup.
func (c *lostToolReceipt) LookupRequest(ctx context.Context, request api.LookupRequestRequest) (api.LookupRequestResult, error) {
	if c.lost {
		return api.LookupRequestResult{}, context.DeadlineExceeded
	}
	return c.Client.LookupRequest(ctx, request)
}

func TestToolNativeOutcomeReconcilesWithoutExecutionReplay(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, risk := range []tool.RiskClass{tool.RiskRead, tool.RiskWrite} {
			name := string(risk) + map[bool]string{false: "/absent", true: "/acknowledged"}[durable]
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				cfg, err := (config.Config{
					HomeDirectory: t.TempDir(), DataDirectory: t.TempDir(), WorkspaceDirectory: t.TempDir(),
					ATE: config.ATEConfig{Enabled: true, Binary: testATEBinary(t), Directory: filepath.Join(t.TempDir(), "ate")},
				}).Resolve()
				if err != nil {
					t.Fatal(err)
				}
				db, err := runtimestore.OpenRuntime(ctx, cfg.RuntimeDatabasePath())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				authority, err := openAuthority(ctx, cfg, db.ATERequests())
				if err != nil {
					t.Fatal(err)
				}
				defer authority.process.Close()
				fault := &toolReceiptFault{durable: durable}
				events, err := sessionate.New(sessionate.Options{
					Process: authority.bound, Pending: db.ATERequests(), Space: authoritySpace, StoreID: authority.storeID,
					ReconciliationTimeout: cfg.ATE.Timeout + time.Second,
					NewExecutor: func(c *client.Client, id model.StoreID) (sessionate.Executor, error) {
						return aterequest.NewExecutor(&lostToolReceipt{Client: c, fault: fault}, db.ATERequests(), id)
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				registry := tool.NewRegistry()
				invocations := 0
				_, err = registry.Activate(tool.Binding{
					Descriptor: tool.Descriptor{Name: "outcome_probe", Revision: "v1", Description: "Test outcome persistence",
						RiskClass: risk, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)},
					Executor: tool.InvokeFunc(func(context.Context, json.RawMessage) (json.RawMessage, error) {
						invocations++
						return json.RawMessage(`{"count":0}`), nil
					}),
				})
				if err != nil {
					t.Fatal(err)
				}
				engine, err := policy.DefaultDeny(policy.Rule{ID: "allow-probe", ToolPattern: "outcome_probe", CapabilityPattern: "tool.invoke", Effect: policy.EffectAllow})
				if err != nil {
					t.Fatal(err)
				}
				pipeline, err := tool.NewPipeline(tool.PipelineConfig{Registry: registry, Policy: engine, Events: events})
				if err != nil {
					t.Fatal(err)
				}
				call := tool.Call{SessionID: "native-outcome", TurnID: "turn", CallID: "call", ToolName: "outcome_probe", Arguments: json.RawMessage(`{}`)}
				result, err := pipeline.Execute(ctx, call)
				if !fault.fired || invocations != 1 {
					t.Fatalf("fault missed actual outcome boundary: fired=%v executions=%d err=%v", fault.fired, invocations, err)
				}
				if durable {
					if err != nil || string(result.Data) != `{"count":0}` {
						t.Fatalf("durable result lost: %s %v", result.Data, err)
					}
					if _, err := pipeline.Execute(ctx, call); err != nil || invocations != 1 {
						t.Fatalf("executor replayed after recovery: %d %v", invocations, err)
					}
				} else {
					var unknown *tool.OutcomeUnknownError
					if !errors.As(err, &unknown) {
						t.Fatalf("absent receipt became success: %v", err)
					}
					_, appendErr := events.Append(ctx, call.SessionID, session.AnySequence, session.PendingEvent{
						Type: session.EventUserMessage, Payload: json.RawMessage(`{"content":"different work"}`),
					})
					if !errors.Is(appendErr, sessionate.ErrPending) {
						t.Fatalf("unresolved identity was replaced: %v", appendErr)
					}
				}
				record, found, err := db.ATERequests().Get(ctx, fault.key)
				if err != nil || found == durable || (found && !record.Attempted) {
					t.Fatalf("pending receipt state: found=%v attempted=%v err=%v", found, record.Attempted, err)
				}
				history, err := events.LoadByCorrelation(ctx, call.SessionID, call.CallID)
				if err != nil {
					t.Fatal(err)
				}
				successes := 0
				for _, event := range history {
					if event.Type == session.EventToolSucceeded {
						successes++
					}
				}
				if successes != map[bool]int{false: 0, true: 1}[durable] || invocations != 1 {
					t.Fatalf("duplicate/lost outcomes: successes=%d executions=%d", successes, invocations)
				}
			})
		}
	}
}
