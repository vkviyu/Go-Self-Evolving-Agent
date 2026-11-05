package orchestration

import (
	"strings"
	"testing"
)

func TestTypedIDsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		new      func() (string, error)
		validate func(string) error
	}{
		{"team", func() (string, error) { value, err := NewTeamID(); return string(value), err }, func(value string) error { return TeamID(value).Validate() }},
		{"agent", func() (string, error) { value, err := NewAgentID(); return string(value), err }, func(value string) error { return AgentID(value).Validate() }},
		{"thread", func() (string, error) { value, err := NewThreadID(); return string(value), err }, func(value string) error { return ThreadID(value).Validate() }},
		{"task", func() (string, error) { value, err := NewTaskID(); return string(value), err }, func(value string) error { return TaskID(value).Validate() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value, err := test.new()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(value, test.name+"_") {
				t.Fatalf("ID %q has wrong prefix", value)
			}
			if err := test.validate(value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTypedIDRejectsWrongType(t *testing.T) {
	t.Parallel()
	value, err := NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := TeamID(value).Validate(); err == nil {
		t.Fatal("agent ID must not validate as a team ID")
	}
}
