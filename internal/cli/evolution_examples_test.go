package cli

import (
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
)

func TestEvolutionExamplesMatchThePublicSchemas(t *testing.T) {
	var request pluginbuild.Request
	if err := readStrictPluginJSON("../../examples/evolution/echo.json", pluginbuild.MaxRequestBytes, &request); err != nil {
		t.Fatal(err)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	var spec strategy.Spec
	if err := readStrictPluginJSON("../../examples/evolution/strategy.json", 16<<10, &spec); err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	var suite strategy.Suite
	if err := readStrictPluginJSON("../../examples/evolution/shadow-suite.json", 128<<10, &suite); err != nil {
		t.Fatal(err)
	}
	if err := suite.Validate(); err != nil {
		t.Fatal(err)
	}
}
