package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/sandbox"
)

func TestChatOutputAllowanceDefersAutomaticResolution(t *testing.T) {
	for _, test := range []struct {
		name               string
		window, configured int
		arguments          []string
		want               int
	}{
		{name: "unknown capacity", want: 0},
		{name: "small configured capacity", window: 8192, want: 0},
		{name: "large configured capacity", window: 1000000, want: 0},
		{name: "explicit configured allowance", configured: 4096, want: 4096},
		{name: "flag resets configured allowance", configured: 4096, arguments: []string{"--max-output-tokens", "0"}, want: 0},
		{name: "flag allowance", arguments: []string{"--max-output-tokens", "1024"}, want: 1024},
	} {
		t.Run(test.name, func(t *testing.T) {
			defaults := config.Config{
				Model:  config.ModelConfig{Timeout: time.Second},
				Agent:  config.AgentConfig{MaxIterations: 20, ContextWindowTokens: test.window, MaxOutputTokens: test.configured},
				Agents: config.AgentsConfig{MaxRunning: 4},
			}
			options, err := parseChatOptions(test.arguments, defaults, &bytes.Buffer{})
			if err != nil || options.maxOutputTokens != test.want {
				t.Fatalf("output allowance=%d, want=%d err=%v", options.maxOutputTokens, test.want, err)
			}
		})
	}
	defaults := config.Config{Model: config.ModelConfig{Timeout: time.Second}, Agent: config.AgentConfig{MaxIterations: 20}, Agents: config.AgentsConfig{MaxRunning: 4}}
	if _, err := parseChatOptions([]string{"--max-output-tokens", "-1"}, defaults, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "max-output-tokens") {
		t.Fatalf("negative allowance error=%v", err)
	}
	if _, err := parseChatOptions([]string{"--max-running-agents", "0"}, defaults, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "max-running-agents") {
		t.Fatalf("zero running limit error=%v", err)
	}
	options, err := parseChatOptions([]string{"--max-running-agents", "2"}, defaults, &bytes.Buffer{})
	if err != nil || options.maxRunningAgents != 2 {
		t.Fatalf("running limit=%d err=%v", options.maxRunningAgents, err)
	}
}

func TestParseChatOptionsKeepsProviderConfigTogether(t *testing.T) {
	defaults := config.Config{
		DataDirectory: "/tmp/local-agent",
		Model:         config.ModelConfig{Provider: "ollama", WireAPI: "chat-completions", Timeout: 30},
		Agent:         config.AgentConfig{Instruction: "configured instruction", MaxIterations: 20, ContextWindowTokens: 32768, CompactionTriggerTokens: 24576},
		Agents:        config.AgentsConfig{MaxRunning: 4},
	}
	options, err := parseChatOptions([]string{
		"--provider", "openai",
		"--wire-api", "responses",
		"--model", "gpt-test",
		"--base-url", "https://example.test/v1",
		"--api-key-env", "XAPI_API_KEY",
		"--reasoning-effort", "high",
		"--session", "session-1",
	}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !options.usesResponses() {
		t.Fatal("expected Responses mode")
	}
	if options.model.Provider != modelgateway.ProviderOpenAI || options.model.Model != "gpt-test" || options.model.APIKeyEnvironment != "XAPI_API_KEY" {
		t.Fatalf("model options = %+v", options.model)
	}
	if !options.modelExplicit || !options.providerExplicit {
		t.Fatalf("explicit model/provider flags were not tracked: %+v", options)
	}
	if options.sessionID != "session-1" || options.maxIterations != 20 || options.instruction != "configured instruction" {
		t.Fatalf("runtime options = %+v", options)
	}
}

func TestParseChatOptionsTracksModelProviderExplicitnessIndependently(t *testing.T) {
	defaults := config.Config{
		Model:  config.ModelConfig{Provider: "ollama", Name: "default-model", Timeout: 30},
		Agent:  config.AgentConfig{MaxIterations: 20, ContextWindowTokens: 32768, CompactionTriggerTokens: 24576},
		Agents: config.AgentsConfig{MaxRunning: 4},
	}
	options, err := parseChatOptions([]string{"--model", "cli-model"}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !options.modelExplicit || options.providerExplicit {
		t.Fatalf("explicitness = model:%v provider:%v", options.modelExplicit, options.providerExplicit)
	}
	options, err = parseChatOptions([]string{"--provider", "openai"}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.modelExplicit || !options.providerExplicit {
		t.Fatalf("explicitness = model:%v provider:%v", options.modelExplicit, options.providerExplicit)
	}
}

func TestParseChatOptionsRejectsInvalidSessionID(t *testing.T) {
	defaults := config.Config{DataDirectory: "/tmp/local-agent", Model: config.ModelConfig{Timeout: 30}, Agent: config.AgentConfig{MaxIterations: 20}, Agents: config.AgentsConfig{MaxRunning: 4}}
	if _, err := parseChatOptions([]string{"--session", "../unsafe"}, defaults, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("expected invalid session ID error, got %v", err)
	}
}

func TestParseChatOptionsValidatesHeadlessFormat(t *testing.T) {
	defaults := config.Config{
		DataDirectory: "/tmp/local-agent",
		Model:         config.ModelConfig{Timeout: 30},
		Agent: config.AgentConfig{
			MaxIterations:           20,
			ContextWindowTokens:     32768,
			CompactionTriggerTokens: 24576,
		},
		Agents: config.AgentsConfig{MaxRunning: 4},
	}
	options, err := parseChatOptions([]string{"--headless-format", "JSON"}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.headlessFormat != headlessFormatJSON {
		t.Fatalf("headless format = %q", options.headlessFormat)
	}
	if _, err := parseChatOptions([]string{"--headless-format", "xml"}, defaults, &bytes.Buffer{}); err == nil {
		t.Fatal("expected unsupported headless format error")
	}
}

func TestParseChatOptionsEvaluationModeRequiresExplicitWorkspace(t *testing.T) {
	defaults := config.Config{
		DataDirectory:      "/tmp/local-agent",
		WorkspaceDirectory: "/configured/workspace",
		Model:              config.ModelConfig{Timeout: 30},
		Agent: config.AgentConfig{
			MaxIterations:           20,
			ContextWindowTokens:     32768,
			CompactionTriggerTokens: 24576,
		},
		Agents: config.AgentsConfig{MaxRunning: 4},
	}
	for _, arguments := range [][]string{
		{"--evaluation-mode"},
		{"--evaluation-mode", "--workspace="},
		{"--evaluation-mode", "--workspace", "   "},
	} {
		if _, err := parseChatOptions(arguments, defaults, &bytes.Buffer{}); err == nil {
			t.Fatalf("expected evaluation arguments %v to fail", arguments)
		}
	}
	options, err := parseChatOptions([]string{"--evaluation-mode", "--workspace", "/isolated/workspace"}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !options.evaluationMode || !options.workspaceExplicit || options.workspaceDirectory != "/isolated/workspace" {
		t.Fatalf("evaluation options = %+v", options)
	}
}

func TestParseChatOptionsRestrictsContainerSandboxToEvaluation(t *testing.T) {
	defaults := config.Config{
		DataDirectory: "/tmp/local-agent",
		Model:         config.ModelConfig{Timeout: 30},
		Agent: config.AgentConfig{
			MaxIterations:           20,
			ContextWindowTokens:     32768,
			CompactionTriggerTokens: 24576,
		},
		Agents: config.AgentsConfig{MaxRunning: 4},
	}
	if _, err := parseChatOptions([]string{"--shell-sandbox", "container"}, defaults, &bytes.Buffer{}); err == nil {
		t.Fatal("container sandbox was accepted outside evaluation mode")
	}
	options, err := parseChatOptions([]string{
		"--evaluation-mode", "--workspace", "/isolated/workspace", "--shell-sandbox", "container",
	}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.shellSandbox != sandbox.ModeContainer {
		t.Fatalf("shell sandbox = %q", options.shellSandbox)
	}
	if _, err := parseChatOptions([]string{"--shell-sandbox", "unknown"}, defaults, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown shell sandbox was accepted")
	}
}

func TestParseChatOptionsRestrictsToolBridgeToEvaluation(t *testing.T) {
	defaults := config.Config{
		DataDirectory: "/tmp/local-agent",
		Model:         config.ModelConfig{Timeout: 30},
		Agent: config.AgentConfig{
			MaxIterations:           20,
			ContextWindowTokens:     32768,
			CompactionTriggerTokens: 24576,
		},
		Agents: config.AgentsConfig{MaxRunning: 4},
	}
	if _, err := parseChatOptions([]string{"--tool-bridge-manifest", "tools.json"}, defaults, &bytes.Buffer{}); err == nil {
		t.Fatal("incomplete tool bridge was accepted")
	}
	if _, err := parseChatOptions([]string{
		"--tool-bridge-manifest", "tools.json", "--tool-bridge-socket", "/tmp/tools.sock",
	}, defaults, &bytes.Buffer{}); err == nil {
		t.Fatal("tool bridge was accepted outside evaluation mode")
	}
	options, err := parseChatOptions([]string{
		"--evaluation-mode", "--workspace", "/isolated/workspace",
		"--tool-bridge-manifest", "tools.json", "--tool-bridge-socket", "/tmp/tools.sock",
	}, defaults, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.toolBridgeManifest != "tools.json" || options.toolBridgeSocket != "/tmp/tools.sock" {
		t.Fatalf("tool bridge options = %+v", options)
	}
}

func TestProfileArgumentSupportsLongAndShortForms(t *testing.T) {
	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{arguments: []string{"--profile", "work"}, want: "work"},
		{arguments: []string{"-p", "work"}, want: "work"},
		{arguments: []string{"--profile=work"}, want: "work"},
		{arguments: []string{"-p=work"}, want: "work"},
	} {
		got, set, err := profileArgument(test.arguments)
		if err != nil || !set || got != test.want {
			t.Fatalf("arguments %v: profile=%q set=%v err=%v", test.arguments, got, set, err)
		}
	}
}

func TestProfileArgumentRejectsMissingOrEmptyValue(t *testing.T) {
	for _, arguments := range [][]string{{"--profile"}, {"-p", ""}, {"--profile="}} {
		if _, _, err := profileArgument(arguments); err == nil {
			t.Fatalf("arguments %v: expected error", arguments)
		}
	}
}
