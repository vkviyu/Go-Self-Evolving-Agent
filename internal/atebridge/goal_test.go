package atebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vkviyu/agent-temporal-engine/agentprofile"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateports"
)

func goalSource(t *testing.T, f *fixture, key model.KnowledgeKey) memory.Source {
	t.Helper()
	var source memory.Source
	if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		receipt, err := f.b.lookup(t.Context(), c, "value/goal/"+string(key.Execution))
		if err != nil {
			return err
		}
		value, err := c.GetState(t.Context(), api.ReadStateRequest{Space: f.b.OperationalSpace(), Key: f.b.valueKey("goal/" + string(key.Execution))})
		if err != nil {
			return err
		}
		id, err := valueID(f.b.OperationalSpace(), value.Value.Data)
		source = memory.Source{SourceID: id, Anchor: model.SnapshotAnchor{Space: f.b.OperationalSpace(), Fence: receipt.Result.Fence}}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return source
}

func originalTask(t *testing.T, f *fixture, key model.KnowledgeKey) agentprofile.Task {
	t.Helper()
	var task agentprofile.Task
	if err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		var err error
		task, err = f.b.originalTask(t.Context(), c, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestLongUnicodeGoalPublishedBeforeTaskAndStableAcrossRestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	goal := strings.Repeat("完整目标🙂\n", 300) + "preserve the final instruction"
	key, _ := f.b.execution("session", "turn", goal)
	var taskFence model.CommitFence
	f.p.check = func(request api.CommitRequest) error {
		if request.Method != api.MethodApplyAgent {
			return nil
		}
		command, err := agentprofile.DecodeCommand(request.Body)
		if err != nil {
			return err
		}
		if command.CreateTask == nil {
			return nil
		}
		value, err := f.p.store.GetState(ctx, api.ReadStateRequest{
			Space: f.b.OperationalSpace(),
			Key:   f.b.valueKey("goal/" + string(key.Execution)),
		})
		if err != nil {
			return err
		}
		if !value.Found || !bytes.Equal(value.Value.Data, []byte(goal)) {
			return errors.New("full immutable goal was not published before task creation")
		}
		taskFence = value.Fence
		return nil
	}
	got, err := f.b.EnsureExecution(ctx, "session", "turn", goal)
	if err != nil || got != key {
		t.Fatalf("ensure: %+v %v", got, err)
	}
	source := goalSource(t, f, key)
	if got := readSource(t, f.b, source); string(got) != goal {
		t.Fatal("full goal bytes changed")
	}
	task := originalTask(t, f, key)
	anchor, _ := json.Marshal(source.Anchor)
	if len(task.Goal) > 1024 || !utf8.ValidString(task.Goal) ||
		!strings.Contains(task.Goal, "full goal ate.value digest "+string(source.SourceID)) ||
		!strings.Contains(task.Goal, string(anchor)) {
		t.Fatalf("task must contain bounded explicit source reference: %s", task.Goal)
	}
	if taskFence != source.Anchor.Fence {
		t.Fatal("goal anchor changed")
	}
	before := len(f.p.commits)
	f.restart(t)
	again, err := f.b.EnsureExecution(ctx, "session", "turn", goal)
	if err != nil || again != key || len(f.p.commits) != before {
		t.Fatalf("idempotent resume: %v", err)
	}
	if goalSource(t, f, key) != source {
		t.Fatal("resume moved original goal anchor")
	}
	if _, err := f.b.EnsureExecution(ctx, "session", "turn", goal+" changed suffix"); !errors.Is(err, ateports.ErrConflict) {
		t.Fatalf("changed full goal accepted: %v", err)
	}
	if len(f.p.commits) != before {
		t.Fatal("goal collision published a mutation")
	}
	if err := f.b.CompleteExecution(ctx, "session", "turn", true); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if err := f.b.CompleteExecution(ctx, "session", "turn", true); err != nil {
		t.Fatal(err)
	}
}

func TestLongGoalUnknownWriteRetainsOriginalPinAndAnchor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	goal := strings.Repeat("目标", 1000)
	key, _ := f.b.execution("session", "turn", goal)
	goalStateKey := string(f.b.valueKey("goal/" + string(key.Execution)))
	var original api.CommitRequest
	f.p.check = func(request api.CommitRequest) error {
		if request.Method != api.MethodMutateState {
			return nil
		}
		batch, err := api.DecodeStateBatch(request.Body)
		if err != nil {
			return err
		}
		if string(batch.Operations[0].Key) == goalStateKey {
			original = request
			f.p.drop = "after"
			f.p.check = nil
		}
		return nil
	}
	_, err := f.b.EnsureExecution(ctx, "session", "turn", goal)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("want unknown goal write: %v", err)
	}
	lookup, err := f.p.store.LookupRequest(ctx, api.LookupRequestRequest{
		Space: original.Space, Principal: original.Principal, RequestID: original.RequestID,
	})
	if err != nil || lookup.Code != api.CodeDurableAck {
		t.Fatalf("original goal receipt: %v", err)
	}
	if ready, err := f.b.HasExecution(ctx, "session", "turn"); err != nil || ready {
		t.Fatalf("partial preparation: %v %v", ready, err)
	}
	before := len(f.p.commits)
	if err := f.b.CompleteExecution(ctx, "session", "turn", false); err == nil {
		t.Fatal("completed before preparation")
	}
	if len(f.p.commits) != before {
		t.Fatal("completion synthesized records for failed preparation")
	}
	f.restart(t)
	if _, err := f.b.EnsureExecution(ctx, "session", "turn", goal+"wrong"); !errors.Is(err, ateports.ErrConflict) {
		t.Fatalf("unknown goal write allowed changed intent: %v", err)
	}
	if _, err := f.b.EnsureExecution(ctx, "session", "turn", goal); err != nil {
		t.Fatal(err)
	}
	if goalSource(t, f, key).Anchor.Fence != lookup.Result.Fence {
		t.Fatal("goal retry replaced original publication")
	}
	if ready, err := f.b.HasExecution(ctx, "session", "turn"); err != nil || !ready {
		t.Fatalf("prepared: %v %v", ready, err)
	}
}

func TestGoalByteLimitsAndShortRepresentation(t *testing.T) {
	for _, size := range []int{1024, 1025, MaxGoalBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newFixture(t)
			goal := strings.Repeat("界", size/3) + strings.Repeat("x", size%3)
			key, err := f.b.EnsureExecution(context.Background(), "session", "turn", goal)
			if err != nil {
				t.Fatal(err)
			}
			if string(readSource(t, f.b, goalSource(t, f, key))) != goal {
				t.Fatal("goal bytes lost")
			}
			task := originalTask(t, f, key)
			if size <= 1024 && task.Goal != goal {
				t.Fatal("short goal representation changed")
			}
			if size > 1024 && len(task.Goal) > 1024 {
				t.Fatal("long goal still exceeds native task limit")
			}
		})
	}
	f := newFixture(t)
	for _, goal := range []string{"", string([]byte{0xff}), strings.Repeat("a", MaxGoalBytes+1)} {
		if _, err := f.b.EnsureExecution(context.Background(), "session", "turn", goal); err == nil {
			t.Fatal("invalid goal accepted")
		}
	}
	if len(f.p.commits) != 0 || f.p.calls != 0 {
		t.Fatal("invalid goal reached ATE")
	}
}
