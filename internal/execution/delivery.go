package execution

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type DeliveryState string

const (
	DeliveryPending  DeliveryState = "pending"
	DeliveryAccepted DeliveryState = "accepted"
	DeliveryMerged   DeliveryState = "merged"
	DeliveryRejected DeliveryState = "rejected"
)

type DeliveryReceipt struct {
	ID              string        `json:"delivery_id"`
	ExecutionID     string        `json:"execution_id"`
	TaskID          string        `json:"task_id,omitempty"`
	ContractHash    string        `json:"contract_hash"`
	CandidateDigest string        `json:"candidate_digest"`
	VerificationID  string        `json:"verification_id,omitempty"`
	State           DeliveryState `json:"state"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

func (receipt DeliveryReceipt) Validate() error {
	if strings.TrimSpace(receipt.ID) == "" || strings.TrimSpace(receipt.ExecutionID) == "" {
		return errors.New("delivery receipt requires an ID and execution ID")
	}
	if len(receipt.ContractHash) != 64 {
		return errors.New("delivery receipt requires a contract hash")
	}
	if strings.TrimSpace(receipt.CandidateDigest) == "" {
		return errors.New("delivery receipt requires a candidate digest")
	}
	switch receipt.State {
	case DeliveryPending, DeliveryAccepted, DeliveryMerged, DeliveryRejected:
	default:
		return fmt.Errorf("unsupported delivery state %q", receipt.State)
	}
	if receipt.CreatedAt.IsZero() || receipt.UpdatedAt.IsZero() {
		return errors.New("delivery receipt requires timestamps")
	}
	return nil
}
