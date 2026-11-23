package execution

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type VerificationKind string

const (
	VerificationKindModelReport VerificationKind = "model_report"
	VerificationKindMechanical  VerificationKind = "mechanical"
	VerificationKindIndependent VerificationKind = "independent"
	VerificationKindProfile     VerificationKind = "profile"
	VerificationKindWorkspace   VerificationKind = "workspace"
)

type VerificationReceipt struct {
	ID           string           `json:"verification_id"`
	ExecutionID  string           `json:"execution_id"`
	StepID       string           `json:"step_id,omitempty"`
	ContractHash string           `json:"contract_hash"`
	Kind         VerificationKind `json:"kind"`
	VerifierID   string           `json:"verifier_id,omitempty"`
	Evidence     string           `json:"evidence_digest"`
	Passed       bool             `json:"passed"`
	CreatedAt    time.Time        `json:"created_at"`
}

func (receipt VerificationReceipt) Validate() error {
	if strings.TrimSpace(receipt.ID) == "" || strings.TrimSpace(receipt.ExecutionID) == "" {
		return errors.New("verification receipt requires an ID and execution ID")
	}
	if len(receipt.ContractHash) != 64 {
		return errors.New("verification receipt requires a contract hash")
	}
	switch receipt.Kind {
	case VerificationKindModelReport, VerificationKindMechanical, VerificationKindIndependent, VerificationKindProfile, VerificationKindWorkspace:
	default:
		return fmt.Errorf("unsupported verification kind %q", receipt.Kind)
	}
	if strings.TrimSpace(receipt.Evidence) == "" {
		return errors.New("verification receipt requires evidence")
	}
	if receipt.CreatedAt.IsZero() {
		return errors.New("verification receipt requires creation time")
	}
	return nil
}
