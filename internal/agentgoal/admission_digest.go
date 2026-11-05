package agentgoal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// NewAdmissionToken creates an opaque host-issued identifier. It carries no
// authority by itself; the durable admission row binds it to the Goal,
// session, actor, input and immutable Goal configuration.
func NewAdmissionToken() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate Goal admission token: %w", err)
	}
	return "adm-" + hex.EncodeToString(value[:]), nil
}

func AdmissionInputDigest(input string) string {
	digest := sha256.Sum256([]byte(input))
	return hex.EncodeToString(digest[:])
}

func AdmissionContractDigest(contract *Contract) (string, error) {
	return admissionJSONDigest(contract)
}

func AdmissionConfigDigest(config Config) (string, error) {
	// Keep only immutable Goal execution configuration. Runtime/model identity
	// is already fenced by the Turn snapshot and must not change this contract.
	return admissionJSONDigest(struct {
		Assessment        AssessmentStrategy `json:"assessment"`
		MaxRounds         int                `json:"max_rounds"`
		MaxWallTimeNanos  int64              `json:"max_wall_time_ns"`
		EvaluationTimeout int64              `json:"evaluation_timeout_ns"`
	}{config.Assessment, config.MaxRounds, config.MaxWallTime.Nanoseconds(), config.EvaluationTimeout.Nanoseconds()})
}

func admissionJSONDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode Goal admission digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// AdmissionClock is kept as a small seam for fault-injection tests that need
// deterministic reservation timestamps.
var AdmissionClock = func() time.Time { return time.Now().UTC() }
