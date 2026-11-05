package agentgoal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestContractValidation(t *testing.T) {
	content := "hello"
	for _, path := range []string{"../escape", "/absolute", ".", "", "a/../../escape", "a\\b"} {
		contract := Contract{Objective: "write", Scope: "workspace only", Criteria: []Criterion{{Description: "exact text", Kind: "file_content", Path: path, Content: &content}}}
		if contract.Validate() == nil {
			t.Errorf("unsafe path %q accepted", path)
		}
	}
	contract := Contract{Objective: "write", Scope: "only answer.txt", Criteria: []Criterion{{Description: "exact text", Kind: "file_content", Path: "answer.txt", Content: &content}}}
	if err := contract.Validate(); err != nil {
		t.Fatal(err)
	}
	contract.Criteria = nil
	if contract.Validate() == nil {
		t.Fatal("task without acceptance criteria accepted")
	}
}

func TestSemanticCriterionAllowsFileContextWithoutBecomingExactAssertion(t *testing.T) {
	content := "`.gitignore` contains a new ignore entry for the `testdata` folder."
	contract := Contract{Objective: "Modify .gitignore", Scope: "Only .gitignore; preserve existing entries", Criteria: []Criterion{
		{Description: "Add one new ignore line", Kind: "semantic", Path: ".gitignore", Content: &content},
	}}
	if err := contract.Validate(); err != nil {
		t.Fatal(err)
	}
	if contract.Criteria[0].Kind != "semantic" || *contract.Criteria[0].Content != content {
		t.Fatal("semantic context was rewritten as an exact file assertion")
	}
	for _, path := range []string{"../escape", "/absolute", ".", "a\\b"} {
		contract.Criteria[0].Path = path
		if contract.Validate() == nil {
			t.Errorf("unsafe semantic path accepted: %q", path)
		}
	}
	contract.Criteria[0].Path = ""
	content = strings.Repeat("x", 64*1024+1)
	if contract.Validate() == nil {
		t.Fatal("oversized semantic context accepted")
	}
}

func TestAdmissionCannotRunWithoutTrustedConversationAndInvocation(t *testing.T) {
	if _, err := StartBinding().Executor.Invoke(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("untrusted admission accepted")
	}
}
