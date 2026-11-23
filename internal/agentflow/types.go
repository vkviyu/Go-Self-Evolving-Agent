// Package agentflow provides composable, streaming multi-Agent coordination.
// Control operators are host-owned; every external or orchestration side
// effect is compiled into a call through tool.Pipeline.
package agentflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,95}$`)
var revisionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

var (
	ErrNoResults           = errors.New("Flow produced no valid results")
	ErrReturned            = errors.New("Flow returned")
	ErrHumanDeclined       = errors.New("human intervention declined")
	ErrStepConflict        = errors.New("Flow step input conflicts with its durable checkpoint")
	ErrDefinitionMismatch  = errors.New("Flow definition revision or fingerprint conflicts with its durable run")
	ErrRecursiveInvocation = errors.New("recursive Flow invocation uses the active idempotency key")
)

type State string

const (
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCanceled  State = "canceled"
)

type StepState string

const (
	StepRunning   StepState = "running"
	StepCompleted StepState = "completed"
	StepFailed    StepState = "failed"
)

// Value is the immutable item passed through a Flow. Vars carry named control
// data such as budget snapshots without replacing the branch payload.
type Value struct {
	Key  string                     `json:"key,omitempty"`
	Data json.RawMessage            `json:"data"`
	Vars map[string]json.RawMessage `json:"vars,omitempty"`

	// identity is an execution-local lineage token. It intentionally remains
	// private and is omitted from durable payloads; it only prevents equal
	// values from sharing a child Agent session within one Flow run.
	identity string
}

func NewValue(value any) (Value, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return Value{}, err
	}
	return Value{Data: encoded}, nil
}

func (value Value) Clone() Value {
	clone := Value{Key: value.Key, Data: append(json.RawMessage(nil), value.Data...), identity: value.identity}
	if len(value.Vars) > 0 {
		clone.Vars = make(map[string]json.RawMessage, len(value.Vars))
		for key, item := range value.Vars {
			clone.Vars[key] = append(json.RawMessage(nil), item...)
		}
	}
	return clone
}

func (value Value) Validate() error {
	if len(value.Data) == 0 || !json.Valid(value.Data) {
		return errors.New("Flow value data must be valid JSON")
	}
	for key, item := range value.Vars {
		if !namePattern.MatchString(key) || len(item) == 0 || !json.Valid(item) {
			return fmt.Errorf("invalid Flow variable %q", key)
		}
	}
	return nil
}

func (value Value) Empty() bool {
	trimmed := strings.TrimSpace(string(value.Data))
	return trimmed == "" || trimmed == "null" || trimmed == `""` || trimmed == "[]" || trimmed == "{}"
}

type Item struct {
	Value Value
	Err   error
	Final bool

	// identity is an execution-local lineage token. It is intentionally not
	// exported or serialized: it exists solely to keep durable step IDs
	// unique when a stream contains equal values more than once.
	identity string
}

type Stream <-chan Item

type Run struct {
	ID                    string
	IdempotencyKey        string
	Definition            string
	DefinitionRevision    string
	DefinitionFingerprint string
	SessionID             session.ID
	TeamID                orchestration.TeamID
	AgentID               orchestration.AgentID
	State                 State
	Input                 Value
	Outputs               []Value
	Error                 string
	RegistryGeneration    uint64
	RuntimeSnapshotID     string
	Execution             orchestration.ExecutionBinding
	Version               uint64
	StartedAt             time.Time
	UpdatedAt             time.Time
	CompletedAt           *time.Time
}

type RunRequest struct {
	ID                    string
	IdempotencyKey        string
	Definition            string
	DefinitionRevision    string
	DefinitionFingerprint string
	SessionID             session.ID
	TeamID                orchestration.TeamID
	AgentID               orchestration.AgentID
	Input                 Value
	RegistryGeneration    uint64
	RuntimeSnapshotID     string
	Execution             orchestration.ExecutionBinding
}

func (request RunRequest) Validate() error {
	if request.ID == "" || !namePattern.MatchString(request.Definition) || request.SessionID == "" || request.IdempotencyKey == "" {
		return errors.New("Flow run requires ID, definition, session and idempotency key")
	}
	if err := validRevision(request.DefinitionRevision); err != nil {
		return err
	}
	if len(request.DefinitionFingerprint) != sha256.Size*2 {
		return errors.New("Flow definition fingerprint must be a SHA-256 hexadecimal digest")
	}
	if _, err := hex.DecodeString(request.DefinitionFingerprint); err != nil {
		return errors.New("Flow definition fingerprint must be hexadecimal")
	}
	if err := request.TeamID.Validate(); err != nil {
		return err
	}
	if err := request.AgentID.Validate(); err != nil {
		return err
	}
	if err := request.Execution.Validate(); err != nil {
		return fmt.Errorf("Flow execution binding: %w", err)
	}
	return request.Input.Validate()
}

type Step struct {
	RunID       string
	ID          string
	InputHash   string
	State       StepState
	Outputs     []Value
	Error       string
	Attempt     int
	StartedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

type SessionAction string

const (
	SessionSpawn    SessionAction = "spawn"
	SessionContinue SessionAction = "continue"
)

type SessionStage struct {
	RunID       string
	SessionName string
	StepID      string
	Action      SessionAction
}

type Store interface {
	CreateRun(context.Context, RunRequest) (Run, error)
	GetRun(context.Context, string) (Run, error)
	StartStep(context.Context, string, string, string) (Step, error)
	FinishStep(context.Context, string, string, []Value) (Step, error)
	FailStep(context.Context, string, string, string) error
	FinishRun(context.Context, string, State, []Value, string) (Run, error)
	BeginSessionStage(context.Context, string, string, string) (SessionStage, error)
	SessionAgent(context.Context, string, string) (orchestration.AgentID, bool, error)
	BindSessionAgent(context.Context, string, string, orchestration.AgentID) error
}

func NewID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "flow-" + hex.EncodeToString(value[:]), nil
}

func valueHash(value Value) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%s name %q must match %s", kind, name, namePattern)
	}
	return nil
}

func validRevision(revision string) error {
	if !revisionPattern.MatchString(revision) {
		return fmt.Errorf("Flow revision %q must match %s", revision, revisionPattern)
	}
	return nil
}
