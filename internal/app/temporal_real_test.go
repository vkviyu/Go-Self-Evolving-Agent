package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const realE2EModelTimeout = 5 * time.Minute

func realE2EConfig(base config.Config) config.Config {
	if base.Model.Timeout < realE2EModelTimeout {
		base.Model.Timeout = realE2EModelTimeout
	}
	if base.Agent.MaxIterations < 20 {
		base.Agent.MaxIterations = 20
	}
	return base
}

// Run explicitly with GSEA_TEST_REAL_ATE=1. Load the user's normal provider
// configuration before isolating writable application paths. Never replace
// HOME/GSEA_HOME or credentials, and never log config, prompts or responses.
func TestTemporalRealAPICrossProjectLearning(t *testing.T) {
	if os.Getenv("GSEA_TEST_REAL_ATE") != "1" {
		t.Skip("opt in with GSEA_TEST_REAL_ATE=1 to call the configured real provider")
	}
	start := time.Now()
	defer func() { t.Logf("real_e2e_total=%s", time.Since(start).Round(time.Millisecond)) }()
	base, err := config.Load(config.LoadOptions{WorkingDirectory: temporalRepo(t)})
	temporalRequire(t, "load normal provider configuration", err)
	base = realE2EConfig(base)
	if strings.EqualFold(base.Model.Provider, "openai") {
		keyEnv := base.Model.APIKeyEnvironment
		if strings.TrimSpace(keyEnv) == "" {
			keyEnv = "OPENAI_API_KEY"
		}
		if strings.TrimSpace(os.Getenv(keyEnv)) == "" {
			t.Fatal("real E2E blocked: configured provider credential is absent (presence checked; value not logged)")
		}
	}
	if strings.TrimSpace(base.Model.Name) == "" {
		t.Fatal("real E2E blocked: normal configuration has no model name")
	}
	t.Logf("normal configuration loaded; model present; maintenance_budget=%s", min(5*time.Minute, 2*base.Model.Timeout))
	binary, shared := temporalBinary(t), t.TempDir()
	firstCfg := temporalConfig(t, base, binary, shared)
	secondCfg := temporalConfig(t, base, binary, shared)
	temporalPythonFixture(t, firstCfg.WorkspaceDirectory, false)
	temporalPythonFixture(t, secondCfg.WorkspaceDirectory, true)
	modelConfig := modelgateway.Config{
		Provider: modelgateway.Provider(base.Model.Provider), WireAPI: modelgateway.WireAPI(base.Model.WireAPI),
		Model: base.Model.Name, TokenizerEncoding: base.Model.TokenizerEncoding,
		BaseURL: base.Model.BaseURL, APIKeyEnvironment: base.Model.APIKeyEnvironment,
		ReasoningEffort: base.Model.ReasoningEffort, Timeout: base.Model.Timeout,
	}
	ctx, cancel := context.WithTimeout(t.Context(), base.Model.Timeout)
	chat, err := modelgateway.New(ctx, modelConfig)
	cancel()
	temporalRequire(t, "construct configured real provider", err)
	first := temporalOpen(t, firstCfg)
	firstResult, selected := temporalRun(t, first, chat, "wire-reader",
		"Fix the binary frame decoder in parser.py. Inspect capture.txt and the existing tests to infer the wire format, run python3 -B -m unittest -v, fix the bug, and rerun tests. Keep the existing tests intact; add edge cases in a separate test file if useful. My standing preference across my binary parser projects is to check truncated headers and payloads before slicing, and to include a payload longer than 255 bytes when testing a two-byte length field. Keep this change local to this workspace and use only Python's standard library.")
	if len(selected.Memories) != 0 {
		t.Fatal("first project unexpectedly inherited experience")
	}
	temporalRealTools(t, first, "wire-reader", firstResult.TurnID)
	temporalPythonOracle(t, firstCfg.WorkspaceDirectory, false)
	promoted := temporalPromoted(t, first)
	t.Logf("first_project_promoted_memories=%d", len(promoted))
	temporalRequire(t, "close first real App", first.Close())
	temporalReplay(t, firstCfg, selected)

	// No conversation history, shared workspace, database or explicit memory
	// request crosses this boundary. Only the temporary ATE directory is shared.
	second := temporalOpen(t, secondCfg)
	secondResult, adopted := temporalRun(t, second, chat, "telemetry-reader",
		"Implement decode_many in parser.py for this independent telemetry project. The input is a concatenation of frames; infer the framing from capture.txt and preserve payload bytes. Inspect the workspace, implement the function, add boundary tests in a separate file without changing test_parser.py, and run python3 -B -m unittest -v. Use the testing conventions I prefer for binary parsers. Keep all changes in this workspace and use only Python's standard library.")
	temporalAdopted(t, second, adopted, promoted)
	temporalRealTools(t, second, "telemetry-reader", secondResult.TurnID)
	temporalPythonOracle(t, secondCfg.WorkspaceDirectory, true)
	temporalRequire(t, "close second real App", second.Close())
	temporalReplay(t, secondCfg, adopted)
}

// Isolate real curation and reuse from a multi-step edit/test loop.
func TestTemporalRealAPIPreferenceCrossProject(t *testing.T) {
	if os.Getenv("GSEA_TEST_REAL_ATE") != "1" {
		t.Skip("opt in with GSEA_TEST_REAL_ATE=1 to call the configured real provider")
	}
	start := time.Now()
	defer func() { t.Logf("real_preference_total=%s", time.Since(start).Round(time.Millisecond)) }()
	base, err := config.Load(config.LoadOptions{WorkingDirectory: temporalRepo(t)})
	temporalRequire(t, "load normal provider configuration", err)
	base = realE2EConfig(base)
	t.Logf("normal configuration loaded; maintenance_budget=%s", min(5*time.Minute, 2*base.Model.Timeout))
	binary, shared := temporalBinary(t), t.TempDir()
	firstCfg := temporalConfig(t, base, binary, shared)
	secondCfg := temporalConfig(t, base, binary, shared)
	temporalPythonFixture(t, firstCfg.WorkspaceDirectory, false)
	temporalPythonFixture(t, secondCfg.WorkspaceDirectory, true)
	ctx, cancel := context.WithTimeout(t.Context(), base.Model.Timeout)
	chat, err := modelgateway.New(ctx, modelgateway.Config{
		Provider: modelgateway.Provider(base.Model.Provider), WireAPI: modelgateway.WireAPI(base.Model.WireAPI),
		Model: base.Model.Name, TokenizerEncoding: base.Model.TokenizerEncoding,
		BaseURL: base.Model.BaseURL, APIKeyEnvironment: base.Model.APIKeyEnvironment,
		ReasoningEffort: base.Model.ReasoningEffort, Timeout: base.Model.Timeout,
	})
	cancel()
	temporalRequire(t, "construct configured real provider", err)
	first := temporalOpen(t, firstCfg)
	_, firstSelection := temporalRun(t, first, chat, "parser-preference",
		"Read capture.txt and parser.py, then briefly explain the likely framing bug. Do not edit or run commands yet. My standing preference for binary parser work across projects is to test truncated headers and payloads plus a payload longer than 255 bytes whenever a two-byte length field is involved. Treat that as my testing preference, not as a claim that tests have passed.")
	if len(firstSelection.Memories) != 0 {
		t.Fatal("fresh project inherited memory")
	}
	promoted := temporalPromoted(t, first)
	preferences := 0
	for _, m := range promoted {
		if string(m.Kind) == "preference" {
			preferences++
		}
	}
	if preferences == 0 {
		t.Fatal("real curation did not promote the explicit user preference")
	}
	t.Logf("real_promoted_memories=%d user_preferences=%d", len(promoted), preferences)
	temporalRequire(t, "close first preference App", first.Close())
	temporalReplay(t, firstCfg, firstSelection)
	second := temporalOpen(t, secondCfg)
	_, adopted := temporalRun(t, second, chat, "independent-parser-review",
		"Review this independent telemetry parser project. Read parser.py and capture.txt, then propose how to implement decode_many and which boundary tests to add using my usual binary-parser testing preferences. Do not edit or run commands yet.")
	temporalAdopted(t, second, adopted, promoted)
	temporalRequire(t, "close second preference App", second.Close())
	temporalReplay(t, secondCfg, adopted)
}

func temporalRealTools(t *testing.T, a *App, id, turnID string) {
	t.Helper()
	reads, writes, testRuns := 0, 0, 0
	err := session.VisitEvents(t.Context(), a.events, session.ID(id), func(e session.Event) error {
		if e.Type != session.EventToolSucceeded {
			return nil
		}
		var p struct {
			TurnID string `json:"turn_id"`
			Tool   string `json:"tool"`
			Result struct {
				Command  string `json:"command"`
				ExitCode *int   `json:"exit_code"`
			} `json:"result"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return err
		}
		if p.TurnID != turnID {
			return nil
		}
		switch p.Tool {
		case "workspace_read":
			reads++
		case "workspace_write", "workspace_edit":
			writes++
		case "shell_exec":
			if p.Result.ExitCode != nil && *p.Result.ExitCode == 0 && strings.Contains(p.Result.Command, "unittest") {
				testRuns++
			}
		}
		return nil
	})
	temporalRequire(t, "inspect actual development tool evidence", err)
	if reads == 0 || writes == 0 || testRuns == 0 {
		t.Fatalf("missing actual development evidence: reads=%d writes=%d successful_test_commands=%d", reads, writes, testRuns)
	}
	t.Logf("session=%s actual_reads=%d actual_writes=%d successful_test_commands=%d", id, reads, writes, testRuns)
}

const temporalSingleTests = `import unittest
from parser import decode

class ParserTests(unittest.TestCase):
    def test_capture(self):
        self.assertEqual(decode(bytes.fromhex("0300616263")), b"abc")
    def test_empty(self):
        self.assertEqual(decode(bytes.fromhex("0000")), b"")
    def test_long(self):
        self.assertEqual(decode(bytes.fromhex("0001") + b"x" * 256), b"x" * 256)
    def test_truncated(self):
        for data in (b"", b"\x01", b"\x03\x00ab"):
            with self.assertRaises(ValueError):
                decode(data)
`

const temporalManyTests = `import unittest
from parser import decode_many

class ParserTests(unittest.TestCase):
    def test_capture(self):
        self.assertEqual(decode_many(bytes.fromhex("030061626302006465")), [b"abc", b"de"])
    def test_empty_stream(self):
        self.assertEqual(decode_many(b""), [])
`

func temporalPythonFixture(t *testing.T, root string, many bool) {
	t.Helper()
	parser := `def decode(data):
    length = int.from_bytes(data[:2], "big")
    return data[2:2 + length]
`
	tests := temporalSingleTests
	if many {
		parser = "def decode_many(data):\n    raise NotImplementedError\n"
		tests = temporalManyTests
	}
	for name, content := range map[string]string{
		"parser.py": parser, "test_parser.py": tests,
		"capture.txt": "Frame hex -> payload hex\n0300616263 -> 616263\n02006465 -> 6465\n0000 -> (empty)\n0001 followed by 256 bytes of 78 -> 256 payload bytes of 78\n",
	} {
		temporalRequire(t, "write isolated parser fixture", os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}
}

func temporalPythonOracle(t *testing.T, root string, many bool) {
	t.Helper()
	wantTests := temporalSingleTests
	oracle := `from parser import decode
assert decode(b"\x03\x00abc") == b"abc"
assert decode(b"\x00\x01" + b"x"*256) == b"x"*256
for v in (b"", b"\x01", b"\x03\x00ab"):
    try:
        decode(v)
    except ValueError:
        pass
    else:
        raise AssertionError("truncation accepted")
`
	if many {
		wantTests = temporalManyTests
		oracle = `from parser import decode_many
assert decode_many(b"") == []
assert decode_many(b"\x00\x00") == [b""]
assert decode_many(b"\x03\x00abc\x00\x01" + b"x"*256) == [b"abc", b"x"*256]
for v in (b"\x01", b"\x03\x00ab", b"\x00\x00\x02\x00x"):
    try:
        decode_many(v)
    except ValueError:
        pass
    else:
        raise AssertionError("truncation accepted")
`
	}
	actual, err := os.ReadFile(filepath.Join(root, "test_parser.py"))
	temporalRequire(t, "read original acceptance tests", err)
	if string(actual) != wantTests {
		t.Fatal("model changed original acceptance tests")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for _, args := range [][]string{{"-B", "-m", "unittest", "-v"}, {"-B", "-c", oracle}} {
		cmd := exec.CommandContext(ctx, "python3", args...)
		cmd.Dir = root
		// Model-generated code receives no provider credentials in this verifier.
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
		temporalRequire(t, "independent parser acceptance oracle", cmd.Run())
	}
	t.Log("independent parser tests and boundary oracle passed")
}
