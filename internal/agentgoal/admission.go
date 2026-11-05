package agentgoal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

// AdmissionState is the durable handshake between Goal admission and the
// Turn/round journal. It prevents a Goal-mode turn from being mistaken for an
// ordinary chat turn after a crash between two persistence boundaries.
type AdmissionState string

const (
	AdmissionReserved     AdmissionState = "reserved"
	AdmissionTurnStarted  AdmissionState = "turn_started"
	AdmissionRoundStarted AdmissionState = "round_started"
	AdmissionConsumed     AdmissionState = "consumed"
	AdmissionUnavailable  AdmissionState = "unavailable"
)

func (state AdmissionState) Validate() error {
	switch state {
	case AdmissionReserved, AdmissionTurnStarted, AdmissionRoundStarted, AdmissionConsumed, AdmissionUnavailable:
		return nil
	default:
		return fmt.Errorf("%w: invalid Goal admission state %q", orchestration.ErrInvalidEvent, state)
	}
}

// Admission is an opaque, host-issued durable admission record. Digest fields
// bind the approval to the exact input and Goal contract/configuration.
type Admission struct {
	Token          string
	GoalID         string
	SessionID      string
	TeamID         orchestration.TeamID
	AgentID        orchestration.AgentID
	InputDigest    string
	ContractDigest string
	ConfigDigest   string
	TurnID         string
	Round          int
	State          AdmissionState
	Version        uint64
	Reason         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type AdmissionRequest struct {
	Token          string
	GoalID         string
	SessionID      string
	TeamID         orchestration.TeamID
	AgentID        orchestration.AgentID
	InputDigest    string
	ContractDigest string
	ConfigDigest   string
}

func (request AdmissionRequest) Validate() error {
	for name, value := range map[string]string{"admission token": request.Token, "Goal ID": request.GoalID, "session ID": request.SessionID, "input digest": request.InputDigest, "contract digest": request.ContractDigest, "config digest": request.ConfigDigest} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", orchestration.ErrInvalidEvent, name)
		}
	}
	if err := request.TeamID.Validate(); err != nil {
		return err
	}
	if err := request.AgentID.Validate(); err != nil {
		return err
	}
	return nil
}

var ErrAdmissionTransition = errors.New("invalid Goal admission transition")

func ValidAdmissionTransition(from, to AdmissionState) bool {
	switch from {
	case AdmissionReserved:
		return to == AdmissionTurnStarted || to == AdmissionUnavailable
	case AdmissionTurnStarted:
		return to == AdmissionRoundStarted || to == AdmissionUnavailable
	case AdmissionRoundStarted:
		return to == AdmissionConsumed || to == AdmissionUnavailable
	default:
		return false
	}
}

// AdmissionRepository is implemented by the durable session store. Each
// mutation is a CAS boundary and is safe to retry with the same identity.
type AdmissionRepository interface {
	Prepare(context.Context, AdmissionRequest) (Admission, error)
	BindTurn(context.Context, string, string, string, uint64) (Admission, error)
	BindRound(context.Context, string, string, int, uint64) (Admission, error)
	Consume(context.Context, string, uint64) (Admission, error)
	MarkUnavailable(context.Context, string, uint64, string) (Admission, error)
	Get(context.Context, string) (Admission, error)
	List(context.Context, AdmissionState) ([]Admission, error)
}
