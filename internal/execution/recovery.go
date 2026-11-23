package execution

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type RecoveryState string

const (
	RecoveryRequired   RecoveryState = "required"
	RecoveryReconciled RecoveryState = "reconciled"
	RecoveryAbandoned  RecoveryState = "abandoned"
)

// RecoveryRecord is the durable fence for an effect whose outcome is not
// known. Reconciliation must use this identity and may not create a new
// execution or replay the side effect automatically.
type RecoveryRecord struct {
	ID           string        `json:"recovery_id"`
	ExecutionID  string        `json:"execution_id"`
	StepID       string        `json:"step_id,omitempty"`
	Attempt      int           `json:"attempt"`
	ContractHash string        `json:"contract_hash"`
	RequestKey   string        `json:"request_key"`
	Reason       string        `json:"reason"`
	State        RecoveryState `json:"state"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

func (record RecoveryRecord) Validate() error {
	if strings.TrimSpace(record.ID) == "" ||
		strings.TrimSpace(record.ExecutionID) == "" ||
		strings.TrimSpace(record.RequestKey) == "" {
		return errors.New("recovery record requires identity and request key")
	}
	if record.Attempt < 1 {
		return errors.New("recovery record requires a positive attempt")
	}
	if len(record.ContractHash) != 64 {
		return errors.New("recovery record requires a contract hash")
	}
	if strings.TrimSpace(record.Reason) == "" {
		return errors.New("recovery record requires a reason")
	}
	switch record.State {
	case RecoveryRequired, RecoveryReconciled, RecoveryAbandoned:
	default:
		return fmt.Errorf("unsupported recovery state %q", record.State)
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return errors.New("recovery record requires timestamps")
	}
	return nil
}
