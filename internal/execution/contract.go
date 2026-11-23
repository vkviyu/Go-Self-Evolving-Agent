// Package execution owns the host-independent execution model.
//
// The package deliberately has no dependency on Agent runtimes, tool
// registries, ATE adapters, or persistence implementations. Those systems
// attach to this model through bindings and lifecycle ports.
package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type VerificationPolicy string

const (
	VerificationNone        VerificationPolicy = "none"
	VerificationIndependent VerificationPolicy = "independent"
	VerificationProfile     VerificationPolicy = "profile"
)

// Contract is the immutable authorization and delivery contract for durable
// work. Once an execution starts, changing a field means creating a new
// contract and therefore a new execution scope.
type Contract struct {
	Objective       string             `json:"objective"`
	Scope           string             `json:"scope,omitempty"`
	Criteria        []string           `json:"criteria,omitempty"`
	Constraints     []string           `json:"constraints,omitempty"`
	Verification    VerificationPolicy `json:"verification"`
	Deadline        *time.Time         `json:"deadline,omitempty"`
	MaxAttempts     int                `json:"max_attempts,omitempty"`
	MaxInputTokens  int                `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int                `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int                `json:"max_total_tokens,omitempty"`
	MaxCostMicros   int64              `json:"max_cost_micros,omitempty"`
}

func (contract Contract) IsZero() bool {
	return strings.TrimSpace(contract.Objective) == "" &&
		strings.TrimSpace(contract.Scope) == "" &&
		len(contract.Criteria) == 0 &&
		len(contract.Constraints) == 0 &&
		contract.Verification == "" &&
		contract.Deadline == nil &&
		contract.MaxAttempts == 0 &&
		contract.MaxInputTokens == 0 &&
		contract.MaxOutputTokens == 0 &&
		contract.MaxTotalTokens == 0 &&
		contract.MaxCostMicros == 0
}

func (contract Contract) Validate() error {
	if strings.TrimSpace(contract.Objective) == "" {
		return errors.New("execution objective is required")
	}
	if len(contract.Objective) > 1<<20 || len(contract.Scope) > 1<<20 {
		return errors.New("execution contract text exceeds 1 MiB")
	}
	if len(contract.Criteria) > 128 || len(contract.Constraints) > 128 {
		return errors.New("execution contract contains too many criteria or constraints")
	}
	for _, value := range append(append([]string(nil), contract.Criteria...), contract.Constraints...) {
		if strings.TrimSpace(value) == "" || len(value) > 64<<10 {
			return errors.New("execution contract entries must be non-empty and bounded")
		}
	}
	switch contract.Verification {
	case "", VerificationNone, VerificationIndependent, VerificationProfile:
	default:
		return fmt.Errorf("unsupported verification policy %q", contract.Verification)
	}
	if contract.MaxAttempts < 0 ||
		contract.MaxInputTokens < 0 ||
		contract.MaxOutputTokens < 0 ||
		contract.MaxTotalTokens < 0 ||
		contract.MaxCostMicros < 0 {
		return errors.New("execution contract budgets must be non-negative")
	}
	if contract.Deadline != nil && contract.Deadline.IsZero() {
		return errors.New("execution contract deadline must be non-zero")
	}
	return nil
}

// CanonicalBytes returns the byte representation used for durable identity.
// Text fields are trimmed and an omitted verification policy is normalized to
// "none", so semantically equivalent input cannot create different scopes.
func (contract Contract) CanonicalBytes() ([]byte, error) {
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	canonical := contract
	canonical.Objective = strings.TrimSpace(canonical.Objective)
	canonical.Scope = strings.TrimSpace(canonical.Scope)
	canonical.Criteria = normalizeEntries(canonical.Criteria)
	canonical.Constraints = normalizeEntries(canonical.Constraints)
	if canonical.Verification == "" {
		canonical.Verification = VerificationNone
	}
	if canonical.Deadline != nil {
		deadline := canonical.Deadline.UTC()
		canonical.Deadline = &deadline
	}
	return json.Marshal(canonical)
}

// Hash returns the SHA-256 identity of the canonical contract.
func (contract Contract) Hash() (string, error) {
	canonical, err := contract.CanonicalBytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeEntries(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	normalized := make([]string, len(entries))
	for index, entry := range entries {
		normalized[index] = strings.TrimSpace(entry)
	}
	return normalized
}
