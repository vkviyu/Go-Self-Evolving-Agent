package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/codingtools"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestDoctorCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := run(context.Background(), []string{"doctor", "--data-dir", t.TempDir()}, bytes.NewBuffer(nil), &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	var report struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q", report.Status)
	}
}

func TestSubcommandHelpReturnsSuccess(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if exitCode := runWithDependencies(context.Background(), []string{"eval", "run", "--help"}, bytes.NewBuffer(nil), &stdout, &stderr, dependencies{}); exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Usage of eval run:")) {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestChatCommandRunsMultipleTurns(t *testing.T) {
	cliATEEnvironment(t)

	profile := nativeCLIProfile(t)
	var stdout, stderr bytes.Buffer
	fakeModel := &chatTestModel{}
	dataDirectory := t.TempDir()
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"chat", "--profile", profile, "--model", "test-model", "--data-dir", dataDirectory, "--session", "test-session"},
		bytes.NewBufferString("hello\nagain\n/exit\n"),
		&stdout,
		&stderr,
		dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) { return fakeModel, nil }},
	)
	if exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	if got := stdout.String(); got != "reply: hello\nreply: again\n" {
		t.Fatalf("stdout = %q", got)
	}
	// One system instruction precedes the three conversational messages.
	// Ordinary turns do not inject an automatically selected experience message.
	if fakeModel.lastInputCount != 4 {
		t.Fatalf("second turn model input count = %d, want 4", fakeModel.lastInputCount)
	}

	stdout.Reset()
	stderr.Reset()
	resumedModel := &chatTestModel{}
	exitCode = runWithDependencies(
		context.Background(),
		[]string{"chat", "--profile", profile, "--model", "test-model", "--data-dir", dataDirectory, "--session", "test-session"},
		bytes.NewBufferString("resumed\n/exit\n"),
		&stdout,
		&stderr,
		dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
			return resumedModel, nil
		}},
	)
	if exitCode != 0 {
		t.Fatalf("resume exit code %d, stderr: %s", exitCode, stderr.String())
	}
	if resumedModel.lastInputCount != 6 {
		t.Fatalf("resumed model input count = %d, want 6", resumedModel.lastInputCount)
	}
}

func TestChatCommandRendersReasoningAndUsage(t *testing.T) {
	cliATEEnvironment(t)

	var stdout, stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"chat", "--profile", nativeCLIProfile(t), "--model", "test-model", "--data-dir", t.TempDir(), "--session", "reasoning-session"},
		bytes.NewBufferString("hello\n/exit\n"),
		&stdout,
		&stderr,
		dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
			return &reasoningChatModel{}, nil
		}},
	)
	if exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	want := "plan\nanswer\nusage: input=1 cached=1 cache_hit=100.0% output=2 reasoning=1 total=3\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestChatCommandEmitsMachineReadableFinalResult(t *testing.T) {
	cliATEEnvironment(t)

	var stdout, stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"chat", "--profile", nativeCLIProfile(t), "--model", "test-model", "--data-dir", t.TempDir(), "--session", "json-session", "--headless-format", "json"},
		bytes.NewBufferString("hello\n/exit\n"),
		&stdout,
		&stderr,
		dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
			return &reasoningChatModel{}, nil
		}},
	)
	if exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	var result headlessJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout.String())
	}
	if result.Schema != "gsea.chat.result.v1" || result.Content != "answer" {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.Total != 3 || result.Usage.Reasoning != 1 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestChatCompletesWhenObserverCannotOpen(t *testing.T) {
	data, workspace := t.TempDir(), t.TempDir()
	cliCommandConfig(t, data, workspace)
	if err := os.WriteFile(filepath.Join(data, "diagnostics"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exit := runWithDependencies(t.Context(), []string{
		"chat", "--model", "test-model", "--workspace", workspace, "--data-dir", data, "--prompt-file", "-",
	}, strings.NewReader("hello"), &stdout, &stderr, dependencies{
		newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
			return &chatTestModel{}, nil
		},
	})
	if exit != 0 || stdout.String() != "reply: hello\n" || !strings.Contains(stderr.String(), "execution diagnostics") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

func TestChatCommandReadsMultilinePromptFromStdin(t *testing.T) {
	cliATEEnvironment(t)

	var stdout, stderr bytes.Buffer
	fakeModel := &chatTestModel{}
	exitCode := runWithDependencies(
		context.Background(),
		[]string{"chat", "--profile", nativeCLIProfile(t), "--model", "test-model", "--data-dir", t.TempDir(), "--session", "prompt-file-session", "--prompt-file", "-"},
		bytes.NewBufferString("first line\nsecond line\n"),
		&stdout,
		&stderr,
		dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
			return fakeModel, nil
		}},
	)
	if exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	if got := stdout.String(); got != "reply: first line\nsecond line\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestChatEvaluationModeApprovesWorkspaceWriteThroughPipeline(t *testing.T) {
	workspace := t.TempDir()
	dataDirectory := t.TempDir()
	// The fixture and the CLI process must resolve the same home and data
	// directory, otherwise they open different native authorities.
	cfg := cliCommandConfig(t, dataDirectory, workspace)

	var stdout, stderr bytes.Buffer
	exitCode := runWithDependencies(
		context.Background(),
		[]string{
			"chat", "--model", "test-model", "--data-dir", dataDirectory,
			"--workspace", workspace, "--evaluation-mode", "--session", "evaluation-session",
		},
		bytes.NewBufferString("create the requested file\n/exit\n"),
		&stdout,
		&stderr,
		dependencies{newModel: func(context.Context, modelgateway.Config) (model.ToolCallingChatModel, error) {
			return &workspaceWriteChatModel{}, nil
		}},
	)
	if exitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", exitCode, stderr.String())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "answer.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "package answer\n" {
		t.Fatalf("answer.go = %q", content)
	}

	database := openCLIApp(t, cfg)
	defer database.Close()
	events, err := database.Events().Load(context.Background(), session.ID("evaluation-session"), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	foundRequested, foundApproved, foundSucceeded := false, false, false
	for _, event := range events {
		switch event.Type {
		case session.EventApprovalRequested:
			foundRequested = true
		case session.EventApprovalResolved:
			var payload struct {
				Approved bool `json:"approved"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			foundApproved = payload.Approved
		case session.EventToolSucceeded:
			foundSucceeded = true
		}
	}
	if !foundRequested || !foundApproved || !foundSucceeded {
		t.Fatalf("approval requested=%t approved=%t tool succeeded=%t", foundRequested, foundApproved, foundSucceeded)
	}
}

// cliAuxiliaryReply answers the internal experience curator's staged requests
// with valid empty decisions. Chat tests exercise the Agent Loop, not curation;
// without this, auxiliary maintenance would call the fake model as ordinary chat.
func cliAuxiliaryReply(messages []*schema.Message) *schema.Message {
	if len(messages) == 0 || !strings.Contains(messages[0].Content, "Stage: ") {
		return nil
	}
	stage := strings.Split(strings.Split(messages[0].Content, "Stage: ")[1], "\n")[0]
	switch stage {
	case "discover":
		return schema.AssistantMessage(`{"space_ids":[],"new_space":null,"search_terms":[],"full_history":false}`, nil)
	case "select":
		return schema.AssistantMessage(`{"memory_ids":[]}`, nil)
	case "history-map", "history-reduce":
		return schema.AssistantMessage(`{"notes":[]}`, nil)
	case "extract":
		return schema.AssistantMessage(`{"lessons":[]}`, nil)
	case "organize":
		return schema.AssistantMessage(`{"actions":[]}`, nil)
	case "verify":
		return schema.AssistantMessage(`{"passed":false,"reason":"no verified claim","event_ids":[],"claims_tool_success":false,"tool_success_event_ids":[]}`, nil)
	}
	return nil
}

type chatTestModel struct {
	lastInputCount int
}

var _ model.ToolCallingChatModel = (*chatTestModel)(nil)

func (m *chatTestModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if reply := cliAuxiliaryReply(messages); reply != nil {
		return reply, nil
	}
	m.lastInputCount = len(messages)
	return schema.AssistantMessage("reply: "+messages[len(messages)-1].Content, nil), nil
}

func (m *chatTestModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (m *chatTestModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type reasoningChatModel struct{}

var _ model.ToolCallingChatModel = (*reasoningChatModel)(nil)

func (*reasoningChatModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if reply := cliAuxiliaryReply(messages); reply != nil {
		return reply, nil
	}
	message := schema.AssistantMessage("answer", nil)
	message.ReasoningContent = "plan"
	message.ResponseMeta = &schema.ResponseMeta{
		Usage: &schema.TokenUsage{
			PromptTokens: 1,
			PromptTokenDetails: schema.PromptTokenDetails{
				CachedTokens: 1,
			},
			CompletionTokens: 2,
			TotalTokens:      3,
			CompletionTokensDetails: schema.CompletionTokensDetails{
				ReasoningTokens: 1,
			},
		},
	}
	return message, nil
}

func (m *reasoningChatModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (m *reasoningChatModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type workspaceWriteChatModel struct {
	calls           int
	lastUserContent string
}

var _ model.ToolCallingChatModel = (*workspaceWriteChatModel)(nil)

func (m *workspaceWriteChatModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if reply := cliAuxiliaryReply(messages); reply != nil {
		return reply, nil
	}
	for _, message := range messages {
		if message != nil && message.Role == schema.User {
			m.lastUserContent = message.Content
		}
	}
	m.calls++
	if m.calls == 1 {
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID:   "evaluation-write-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      codingtools.WriteToolName,
				Arguments: `{"path":"answer.go","content":"package answer\n"}`,
			},
		}}), nil
	}
	return schema.AssistantMessage("created answer.go", nil), nil
}

func (m *workspaceWriteChatModel) Stream(ctx context.Context, messages []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, options...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(message, nil)
	}()
	return reader, nil
}

func (m *workspaceWriteChatModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}
