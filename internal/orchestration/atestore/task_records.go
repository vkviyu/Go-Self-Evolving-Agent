package atestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const taskClaimTTL = 30 * time.Minute

// Each edge and command receipt is an individual authoritative JSON record.
// Grouping through TaskSnapshot.ParentTaskID never creates an edge.
type taskEdge struct {
	TaskID       orchestration.TaskID `json:"task_id"`
	DependencyID orchestration.TaskID `json:"dependency_task_id"`
	CreatedAt    time.Time            `json:"created_at"`
}

type taskReceipt struct {
	Hash     string                          `json:"hash"`
	TaskID   orchestration.TaskID            `json:"task_id,omitempty"`
	RunID    orchestration.RunID             `json:"run_id,omitempty"`
	Children map[string]orchestration.TaskID `json:"children,omitempty"`
}

// taskContext supplies a background context for task repository calls whose caller omitted
// one.
func taskContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// taskPair encodes two IDs as an ordered pair for compound task record keys.
func taskPair(a, b string) string {
	data, _ := json.Marshal([2]string{a, b})
	return string(data)
}

// taskHash hashes the JSON representation used to compare task requests for idempotency.
func taskHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// taskEvent appends events to this task's stream within the transaction that changes the
// task.
func taskEvent(tx *ateauthority.Tx, task orchestration.TaskSnapshot, pending ...orchestration.PendingEvent) error {
	_, err := appendEvents(tx, orchestration.Stream{Type: orchestration.StreamTask, ID: string(task.ID), TeamID: task.TeamID}, orchestration.AnySequence, pending...)
	return err
}

func taskValidateState(state orchestration.TaskState) error {
	switch state {
	case orchestration.TaskDraft, orchestration.TaskBlocked, orchestration.TaskReady, orchestration.TaskClaimed, orchestration.TaskInProgress, orchestration.TaskVerification, orchestration.TaskAwaitingMerge, orchestration.TaskCompleted, orchestration.TaskFailed, orchestration.TaskCanceled:
		return nil
	}
	return fmt.Errorf("%w: unsupported task state %q", orchestration.ErrInvalidEvent, state)
}

func taskTerminal(state orchestration.TaskState) bool {
	return state == orchestration.TaskCompleted || state == orchestration.TaskFailed || state == orchestration.TaskCanceled
}

func taskAgentTerminal(state orchestration.AgentState) bool {
	return state == orchestration.AgentCompleted || state == orchestration.AgentFailed || state == orchestration.AgentCanceled
}

func taskAgentLive(state orchestration.AgentState) bool {
	switch state {
	case orchestration.AgentCreated, orchestration.AgentQueued, orchestration.AgentRunning, orchestration.AgentWaiting, orchestration.AgentCanceling:
		return true
	}
	return false
}

// taskAgentBelongs verifies inside the transaction that the agent belongs to the task's
// team.
func taskAgentBelongs(tx *ateauthority.Tx, id orchestration.AgentID, team orchestration.TeamID) error {
	agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(id))
	if err != nil {
		return err
	}
	if agent.TeamID != team {
		return fmt.Errorf("%w: Agent %s belongs to another team", orchestration.ErrInvalidEvent, id)
	}
	return nil
}

func taskActiveTeam(tx *ateauthority.Tx, id orchestration.TeamID) error {
	team, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(id))
	if err != nil {
		return err
	}
	if team.State != orchestration.TeamActive {
		return orchestration.ErrTeamNotActive
	}
	return nil
}

func taskAuthorizePlan(tx *ateauthority.Tx, task orchestration.TaskSnapshot, actorID orchestration.AgentID) error {
	actor, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(actorID))
	if err != nil {
		return err
	}
	if actor.TeamID != task.TeamID || taskAgentTerminal(actor.State) || actor.State == orchestration.AgentCanceling {
		return fmt.Errorf("%w: plan actor is not active in this team", orchestration.ErrInvalidEvent)
	}
	team, err := getRecord[orchestration.TeamSnapshot](tx, "team", string(task.TeamID))
	if err != nil {
		return err
	}
	if team.RootAgentID == actor.ID {
		return nil
	}
	seenTasks := map[orchestration.TaskID]bool{}
	seenAgents := map[orchestration.AgentID]bool{}
	for task.ID != "" && !seenTasks[task.ID] && task.TeamID == team.ID {
		seenTasks[task.ID] = true
		for owner := task.AssigneeAgentID; owner != "" && !seenAgents[owner]; {
			seenAgents[owner] = true
			agent, err := getRecord[orchestration.AgentSnapshot](tx, "agent", string(owner))
			if err != nil {
				return err
			}
			if agent.TeamID != team.ID {
				break
			}
			if agent.ID == actor.ID {
				return nil
			}
			owner = agent.ParentAgentID
		}
		if task.ParentTaskID == "" {
			break
		}
		task, err = getRecord[orchestration.TaskSnapshot](tx, "task", string(task.ParentTaskID))
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: only the root, task assignee or an owning ancestor may edit this plan", orchestration.ErrInvalidEvent)
}

// These projections decode only the fields used by task guards. The workspace
// repositories own their records and their full schemas.
type taskWorkspaceRecord struct {
	AgentID orchestration.AgentID `json:"agent_id"`
	TaskID  orchestration.TaskID  `json:"task_id"`
	State   string                `json:"state"`
}

func taskOpenMerges(tx *ateauthority.Tx, id orchestration.TaskID) (int, error) {
	merges, err := listRecords[taskWorkspaceRecord](tx, "merge")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, merge := range merges {
		if merge.TaskID == id && (merge.State == "pending" || merge.State == "running" || merge.State == "conflicted") {
			n++
		}
	}
	return n, nil
}

// taskDependenciesComplete checks that every prerequisite of the requested edge kind has
// reached TaskCompleted.
func taskDependenciesComplete(tx *ateauthority.Tx, id orchestration.TaskID, kind string) (bool, error) {
	edges, err := listRecords[taskEdge](tx, kind)
	if err != nil {
		return false, err
	}
	for _, edge := range edges {
		if edge.TaskID != id {
			continue
		}
		dependency, err := getRecord[orchestration.TaskSnapshot](tx, "task", string(edge.DependencyID))
		if err != nil {
			return false, err
		}
		if dependency.State != orchestration.TaskCompleted {
			return false, nil
		}
	}
	return true, nil
}

func taskWouldCycle(edges []taskEdge, from, dependency orchestration.TaskID) bool {
	adj := make(map[orchestration.TaskID][]orchestration.TaskID)
	for _, edge := range edges {
		adj[edge.TaskID] = append(adj[edge.TaskID], edge.DependencyID)
	}
	seen := map[orchestration.TaskID]bool{}
	queue := []orchestration.TaskID{dependency}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id == from {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, adj[id]...)
	}
	return false
}

func taskCheckCycle(tx *ateauthority.Tx, from, dependency orchestration.TaskID) error {
	edges, err := listRecords[taskEdge](tx, "task-dependency")
	if err != nil {
		return err
	}
	members, err := listRecords[taskEdge](tx, "task-refinement")
	if err != nil {
		return err
	}
	if taskWouldCycle(append(edges, members...), from, dependency) {
		return fmt.Errorf("%w: %s -> %s", orchestration.ErrTaskDependencyCycle, from, dependency)
	}
	return nil
}

func taskSort(tasks []orchestration.TaskSnapshot) {
	slices.SortFunc(tasks, func(a, b orchestration.TaskSnapshot) int {
		if a.Priority != b.Priority {
			if a.Priority > b.Priority {
				return -1
			}
			return 1
		}
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}
