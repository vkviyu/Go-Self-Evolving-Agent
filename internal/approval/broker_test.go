package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/policy"
)

func TestBrokerKeepsParallelRequestsWithSharedCallIDApart(t *testing.T) {
	parent, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchestration.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	broker := NewBroker(2, WithSourceDescriber(func(_ context.Context, actor orchestration.ActorScope) Source {
		if actor.ParentAgentID == "" {
			return Source{}
		}
		return Source{AgentID: actor.AgentID, Profile: "worker", Task: "Inspect logs"}
	}))
	results := map[orchestration.AgentID]chan bool{"": make(chan bool, 1), child: make(chan bool, 1)}
	for agentID, result := range results {
		actor := orchestration.ActorScope{}
		if agentID != "" {
			actor = orchestration.ActorScope{AgentID: agentID, ParentAgentID: parent}
		}
		go func() {
			approved, err := broker.Approve(context.Background(), policy.Request{Actor: actor, CallID: "call_0", ToolName: "workspace_write"}, policy.Decision{})
			if err != nil {
				t.Errorf("approve: %v", err)
			}
			result <- approved
		}()
	}
	requests := make(map[orchestration.AgentID]Request)
	for range 2 {
		select {
		case request := <-broker.Requests():
			requests[request.Source.AgentID] = request
		case <-time.After(time.Second):
			t.Fatal("approval request was not published")
		}
	}
	if requests[""].ID == requests[child].ID {
		t.Fatalf("requests sharing a call ID got one approval ID: %q", requests[child].ID)
	}
	if source := requests[child].Source; source.Profile != "worker" || source.Task != "Inspect logs" {
		t.Fatalf("child source = %#v", source)
	}
	if err := broker.Resolve(requests[child].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := broker.Resolve(requests[""].ID, false); err != nil {
		t.Fatal(err)
	}
	for agentID, want := range map[orchestration.AgentID]bool{"": false, child: true} {
		select {
		case approved := <-results[agentID]:
			if approved != want {
				t.Fatalf("agent %q approval = %v, want %v", agentID, approved, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("agent %q approval did not return", agentID)
		}
	}
}

func TestBrokerRoundTripAndCancellation(t *testing.T) {
	broker := NewBroker(1)
	result := make(chan bool, 1)
	go func() {
		approved, err := broker.Approve(context.Background(), policy.Request{CallID: "call-1", ToolName: "shell_exec", NormalizedArg: []byte(`{"command":"true"}`)}, policy.Decision{Effect: policy.EffectRequireApproval})
		if err != nil {
			t.Errorf("approve: %v", err)
			return
		}
		result <- approved
	}()
	var request Request
	select {
	case request = <-broker.Requests():
	case <-time.After(time.Second):
		t.Fatal("approval request was not published")
	}
	if request.ID == "" || request.Policy.CallID != "call-1" || request.Policy.ToolName != "shell_exec" {
		t.Fatalf("request = %#v", request)
	}
	if err := broker.Resolve(request.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case approved := <-result:
		if !approved {
			t.Fatal("approval was rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("approval did not return")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := broker.Approve(ctx, policy.Request{CallID: "cancelled"}, policy.Decision{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if err := broker.Resolve("missing", true); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("unknown error = %v", err)
	}
}

func TestBrokerCloseFailsPendingApproval(t *testing.T) {
	broker := NewBroker(1)
	result := make(chan error, 1)
	go func() {
		_, err := broker.Approve(context.Background(), policy.Request{CallID: "pending"}, policy.Decision{})
		result <- err
	}()
	select {
	case <-broker.Requests():
	case <-time.After(time.Second):
		t.Fatal("request was not published")
	}
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("pending approval error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending approval did not resolve")
	}
}
