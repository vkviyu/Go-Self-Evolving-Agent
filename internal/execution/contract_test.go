package execution

import (
	"testing"
	"time"
)

func TestContractHashCanonicalizesEquivalentInput(t *testing.T) {
	deadline := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.FixedZone("test", 8*60*60))
	first := Contract{
		Objective:    "  ship the change ",
		Scope:        " repository ",
		Criteria:     []string{" tests pass "},
		Verification: "",
		Deadline:     &deadline,
	}
	secondDeadline := deadline.UTC()
	second := Contract{
		Objective:    "ship the change",
		Scope:        "repository",
		Criteria:     []string{"tests pass"},
		Verification: VerificationNone,
		Deadline:     &secondDeadline,
	}
	firstHash, err := first.Hash()
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := second.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Fatalf("equivalent contracts have different hashes: %s != %s", firstHash, secondHash)
	}
}

func TestBindingRequiresImmutableDurableScope(t *testing.T) {
	if err := (Binding{ExecutionID: "run", Attempt: 1}).Validate(); err == nil {
		t.Fatal("expected contract hash requirement")
	}
	if err := (Binding{ExecutionID: "run", Attempt: 1, ContractHash: "not-a-hash"}).Validate(); err == nil {
		t.Fatal("expected contract hash format requirement")
	}
	if err := (Binding{ExecutionID: "run", Attempt: 1, ContractHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleRejectsUnknownOutcomeAsCompletion(t *testing.T) {
	if err := Transition(EntityExecution, ExecutionRunning, ExecutionSucceeded); err == nil {
		t.Fatal("execution must verify and deliver before completion")
	}
	if err := Transition(EntityExecution, ExecutionRunning, ExecutionPaused); err != nil {
		t.Fatal(err)
	}
	if err := Transition(EntityExecution, ExecutionPaused, ExecutionAdmitted); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptsRequireExecutionEvidence(t *testing.T) {
	now := time.Now().UTC()
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	verification := VerificationReceipt{
		ID: "verification-1", ExecutionID: "execution-1", ContractHash: hash,
		Kind: VerificationKindMechanical, Evidence: "sha256:evidence", Passed: true, CreatedAt: now,
	}
	if err := verification.Validate(); err != nil {
		t.Fatal(err)
	}
	recovery := RecoveryRecord{
		ID: "recovery-1", ExecutionID: "execution-1", Attempt: 1,
		ContractHash: hash, RequestKey: "request-1", Reason: "outcome was not durable",
		State: RecoveryRequired, CreatedAt: now, UpdatedAt: now,
	}
	if err := recovery.Validate(); err != nil {
		t.Fatal(err)
	}
}
