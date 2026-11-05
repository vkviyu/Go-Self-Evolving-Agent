// Package pluginbuild builds bounded pure-computation candidates in an OS
// sandbox. It never accepts a go.mod, shell command, build flag or SDK override
// from a model, and never publishes a revision.
package pluginbuild

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const MaxRequestBytes = 256 << 10

// Evidence uncertainty is not an ordinary repairable compilation error. The
// tool adapter must stop the Turn instead of spending resources again.
var ErrEvidence error = evidenceError{}

type evidenceError struct{}

func (evidenceError) Error() string            { return "plugin build evidence could not be committed" }
func (evidenceError) ToolExecutionFatal() bool { return true }

type Request struct {
	OwnerSession string            `json:"owner_session,omitempty"`
	Manifest     plugin.Manifest   `json:"manifest"`
	Source       string            `json:"source"`
	Cases        []plugin.TestCase `json:"cases"`
}

func (request Request) Validate() error {
	if err := request.Manifest.Tool.Validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(request.Manifest.Tool.Name, "extension_") || request.Manifest.PluginID == "" || len(request.Manifest.PluginID) > 128 || len(request.Manifest.Tool.Revision) > 128 {
		return errors.New("generated tools require an extension_ name and bounded plugin/revision identity")
	}
	if request.Manifest.Tool.RiskClass != tool.RiskPure || len(request.Manifest.Tool.Capabilities) != 0 {
		return errors.New("generated plugins are pure WASM only; capabilities cannot be self-granted")
	}
	if request.Manifest.Timeout < 0 || request.Manifest.Timeout > 5e9 {
		return errors.New("candidate timeout must be at most 5 seconds")
	}
	if err := ValidateSource(request.Source); err != nil {
		return err
	}
	if err := plugin.ValidateCases(request.Cases); err != nil {
		return err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(encoded) > MaxRequestBytes {
		return errors.New("candidate request exceeds 256 KiB")
	}
	return nil
}

type BuildEvidence struct {
	Toolchain       string `json:"toolchain"`
	ToolchainSHA256 string `json:"toolchain_sha256"`
	// Older evidence only hashed bin/go. This scope explicitly covers the
	// configured host compiler inputs; it is not a bundled-toolchain claim.
	ToolchainHashScope string `json:"toolchain_hash_scope,omitempty"`
	MemoryLimitMode    string `json:"memory_limit_mode,omitempty"`
	SDKSHA256          string `json:"sdk_sha256"`
	SourceSHA256       string `json:"source_sha256"`
	ArtifactSHA256     string `json:"artifact_sha256,omitempty"`
	Sandbox            string `json:"sandbox"`
	Log                string `json:"log"`
	Success            bool   `json:"success"`
}

type Result struct {
	BuildID      string             `json:"build_id"`
	Build        BuildEvidence      `json:"build"`
	State        string             `json:"state"`
	Error        string             `json:"error,omitempty"`
	TestReportID string             `json:"test_report_id,omitempty"`
	TestReport   *plugin.TestReport `json:"test_report,omitempty"`
}
