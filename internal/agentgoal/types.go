// Package agentgoal implements task-level Outer Loop and Goal Mode semantics.
// It deliberately does not own model/tool iteration; one Eino turn remains one
// bounded Inner Loop round.
package agentgoal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
)

type Decision string

const (
	DecisionContinue Decision = "continue"
	DecisionComplete Decision = "complete"
	DecisionBlocked  Decision = "blocked"
)

func (decision Decision) Validate() error {
	switch decision {
	case DecisionContinue, DecisionComplete, DecisionBlocked:
		return nil
	default:
		return fmt.Errorf("unsupported goal decision %q", decision)
	}
}

type AssessmentStrategy string

const (
	AssessmentAdaptive    AssessmentStrategy = "adaptive"
	AssessmentSelf        AssessmentStrategy = "self"
	AssessmentIndependent AssessmentStrategy = "independent"
	AssessmentHybrid      AssessmentStrategy = "hybrid"
)

// Validate accepts only the assessment strategies the evaluator implements.
func (strategy AssessmentStrategy) Validate() error {
	switch strategy {
	case AssessmentAdaptive, AssessmentSelf, AssessmentIndependent, AssessmentHybrid:
		return nil
	default:
		return fmt.Errorf("unsupported goal assessment strategy %q", strategy)
	}
}

type State string

const (
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateBlocked   State = "blocked"
	StateExhausted State = "exhausted"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

type StopReason string

const (
	StopSemanticComplete StopReason = "semantic_complete"
	StopSemanticBlocked  StopReason = "semantic_blocked"
	StopMaxRounds        StopReason = "max_rounds"
	StopMaxWallTime      StopReason = "max_wall_time"
	StopBudgetExhausted  StopReason = "budget_exhausted"
	StopCanceled         StopReason = "canceled"
	StopAbandoned        StopReason = "abandoned"
	StopFailure          StopReason = "failure"
)

type Config struct {
	Enabled           bool
	Activation        Activation
	Assessment        AssessmentStrategy
	MaxRounds         int
	MaxWallTime       time.Duration
	EvaluationTimeout time.Duration
}

func NewID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate goal ID: %w", err)
	}
	return "goal-" + hex.EncodeToString(value[:]), nil
}

func (config Config) WithDefaults() Config {
	if config.Activation == "" {
		config.Activation = ActivationAuto
	}
	if config.Assessment == "" {
		config.Assessment = AssessmentAdaptive
	}
	if config.MaxRounds <= 0 {
		config.MaxRounds = 4
	}
	if config.MaxWallTime <= 0 {
		config.MaxWallTime = 30 * time.Minute
	}
	if config.EvaluationTimeout <= 0 {
		config.EvaluationTimeout = 30 * time.Second
	}
	return config
}

func (config Config) Validate() error {
	if !config.Enabled {
		return nil
	}
	if config.MaxRounds < 0 || config.MaxWallTime < 0 || config.EvaluationTimeout < 0 {
		return errors.New("goal limits cannot be negative")
	}
	config = config.WithDefaults()
	if config.Activation != ActivationAuto && config.Activation != ActivationAlways {
		return fmt.Errorf("unsupported goal activation %q (supported: auto, always)", config.Activation)
	}
	if err := config.Assessment.Validate(); err != nil {
		return err
	}
	if config.MaxRounds <= 0 || config.MaxWallTime <= 0 {
		return errors.New("goal mode requires positive round and wall-time limits")
	}
	return nil
}

type Goal struct {
	Contract    *Contract
	ID          string
	SessionID   session.ID
	TeamID      orchestration.TeamID
	AgentID     orchestration.AgentID
	Text        string
	State       State
	Assessment  AssessmentStrategy
	Round       int
	MaxRounds   int
	MaxWallTime time.Duration
	Decision    Decision
	StopReason  StopReason
	Reason      string
	Result      string
	Version     uint64
	StartedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
	// AdmissionToken is an in-memory handoff from Conversation to the first
	// Goal round. It is persisted separately by AdmissionRepository.
	AdmissionToken string `json:"-"`
}

type CreateRequest struct {
	Contract    *Contract
	StartedAt   time.Time
	ID          string
	SessionID   session.ID
	TeamID      orchestration.TeamID
	AgentID     orchestration.AgentID
	Text        string
	Assessment  AssessmentStrategy
	MaxRounds   int
	MaxWallTime time.Duration
}

func (request CreateRequest) Validate() error {
	if request.Contract != nil {
		if err := request.Contract.Validate(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(request.ID) == "" || request.SessionID == "" || strings.TrimSpace(request.Text) == "" {
		return errors.New("goal requires ID, session ID and text")
	}
	if request.TeamID != "" {
		if err := request.TeamID.Validate(); err != nil {
			return err
		}
	}
	if request.AgentID != "" {
		if err := request.AgentID.Validate(); err != nil {
			return err
		}
	}
	if request.MaxRounds <= 0 || request.MaxWallTime <= 0 {
		return errors.New("goal requires positive hard limits")
	}
	return request.Assessment.Validate()
}

type Round struct {
	GoalID      string
	Number      int
	TurnID      string
	Input       string
	Candidate   string
	Decision    Decision
	Reason      string
	Feedback    string
	StartedAt   time.Time
	CompletedAt *time.Time
}

type Report struct {
	GoalID    string    `json:"goal_id"`
	Round     int       `json:"round"`
	TurnID    string    `json:"turn_id"`
	Decision  Decision  `json:"status"`
	Reason    string    `json:"reason"`
	Evidence  []string  `json:"evidence,omitempty"`
	Blockers  []string  `json:"blockers,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (report Report) Validate() error {
	if err := report.Decision.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(report.Reason) == "" {
		return errors.New("goal report reason is required")
	}
	if report.Decision == DecisionBlocked && len(report.Blockers) == 0 {
		return errors.New("blocked goal report requires at least one blocker")
	}
	return nil
}

type Finish struct {
	State      State
	Decision   Decision
	StopReason StopReason
	Reason     string
	Result     string
}

// Store is the durable Goal Mode projection. Round registration happens before
// the Inner Loop starts so goal_report can resolve its goal from trusted tool
// invocation metadata.
type Store interface {
	// AdmitGoal atomically creates a goal and adopts an already-running turn as
	// its first round. Retry is idempotent; contracts cannot be replaced.
	AdmitGoal(context.Context, CreateRequest, string) (Goal, Round, error)
	CreateGoal(context.Context, CreateRequest) (Goal, error)
	GetGoal(context.Context, string) (Goal, error)
	GoalForTurn(context.Context, session.ID, string) (Goal, Round, bool, error)
	StartRound(context.Context, string, int, string, string) (Round, error)
	FinishRound(context.Context, string, int, Verdict, string) (Round, error)
	RecordReport(context.Context, session.ID, string, Report) (Report, error)
	ReportForTurn(context.Context, session.ID, string) (Report, bool, error)
	FinishGoal(context.Context, string, Finish) (Goal, error)
	CountGoals(context.Context, session.ID) (int, error)
}

type BudgetReader interface {
	Budget(context.Context, orchestration.TeamID) (tokenusage.BudgetSnapshot, error)
}

// PendingRoundReader includes unstarted rounds so recovery discovery does not
// hide the crash window between Goal creation and turn.started.
type PendingRoundReader interface {
	PendingRounds(context.Context, session.ID) ([]Round, error)
}

type CapGuard struct {
	Budget BudgetReader
	Now    func() time.Time
}

func (guard CapGuard) Reached(ctx context.Context, goal Goal, nextRound int) (StopReason, bool, error) {
	if nextRound > goal.MaxRounds {
		return StopMaxRounds, true, nil
	}
	now := time.Now().UTC()
	if guard.Now != nil {
		now = guard.Now().UTC()
	}
	if !goal.StartedAt.IsZero() && now.Sub(goal.StartedAt) >= goal.MaxWallTime {
		return StopMaxWallTime, true, nil
	}
	if guard.Budget != nil && goal.TeamID != "" {
		budget, err := guard.Budget.Budget(ctx, goal.TeamID)
		if err != nil {
			return "", false, err
		}
		if budget.Exhausted {
			return StopBudgetExhausted, true, nil
		}
	}
	return "", false, nil
}
