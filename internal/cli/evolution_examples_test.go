package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func TestStrategyExamplesMatchThePublicSchemas(t *testing.T) {
	var spec strategy.Spec
	if err := readStrategyJSON("../../examples/evolution/strategy.json", maxStrategySpecBytes, &spec); err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	var suite strategy.Suite
	if err := readStrategyJSON("../../examples/evolution/shadow-suite.json", maxStrategySuiteBytes, &suite); err != nil {
		t.Fatal(err)
	}
	if err := suite.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStrategyJSONFileBoundsAndSchema(t *testing.T) {
	dir := t.TempDir()
	var spec strategy.Spec
	if err := readStrategyJSON(dir, maxStrategySpecBytes, &spec); err == nil {
		t.Fatal("accepted a directory")
	}
	const valid = `{"version":1}`
	atLimit := valid + strings.Repeat(" ", maxStrategySpecBytes-len(valid))
	for _, tc := range []struct {
		name, data string
		wantErr    bool
	}{
		{"exact limit", atLimit, false},
		{"over limit", atLimit + " ", true},
		{"unknown field", `{"unexpected":true}`, true},
		{"second value", valid + ` {}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "spec.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			var input strategy.Spec
			if err := readStrategyJSON(path, maxStrategySpecBytes, &input); (err != nil) != tc.wantErr {
				t.Fatalf("readStrategyJSON: %v", err)
			}
		})
	}
}
