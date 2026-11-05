package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func TestOpenAIRequiresCredentialWithoutExposingValue(t *testing.T) {
	t.Parallel()

	_, err := newWithLookup(context.Background(), Config{
		Provider:          ProviderOpenAI,
		Model:             "test-model",
		APIKeyEnvironment: "TEST_SECRET",
	}, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "TEST_SECRET") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCacheUsageHTTPClientDoesNotMutateCallerClient(t *testing.T) {
	t.Parallel()

	transport := &inertRoundTripper{}
	original := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	wrapped := cacheUsageHTTPClient(original, time.Second)

	if wrapped == original {
		t.Fatal("cache usage client reused the caller's client pointer")
	}
	if original.Transport != transport || original.Timeout != 3*time.Second {
		t.Fatal("cache usage client mutated the caller's client")
	}
	normalizing, ok := wrapped.Transport.(cacheUsageTransport)
	if !ok || normalizing.inner != transport {
		t.Fatalf("wrapped transport = %#v", wrapped.Transport)
	}
}

func TestOpenAICompatibleWireRequest(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer test-secret" {
			t.Errorf("authorization = %q", authorization)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"gateway works"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens":2,"total_tokens":6}}`,
			)),
			Request: request,
		}, nil
	})}

	chatModel, err := newWithLookup(context.Background(), Config{
		Provider:          ProviderOpenAI,
		Model:             "test-model",
		BaseURL:           "https://model.test/v1",
		APIKeyEnvironment: "TEST_SECRET",
		Timeout:           time.Second,
		HTTPClient:        client,
	}, func(name string) (string, bool) { return "test-secret", name == "TEST_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	response, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "gateway works" || response.ResponseMeta == nil || response.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 3 {
		t.Fatalf("response = %#v", response)
	}
}

func TestDeepSeekChatCacheUsageIsNormalized(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"deepseek-test","object":"chat.completion","created":1,"model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"cached"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3,"completion_tokens":2,"total_tokens":12}}`,
			)),
			Request: request,
		}, nil
	})}

	chatModel, err := newWithLookup(context.Background(), Config{
		Provider: ProviderOpenAI, Model: "deepseek-chat", BaseURL: "https://model.test/v1",
		APIKeyEnvironment: "TEST_SECRET", Timeout: time.Second, HTTPClient: client,
	}, func(name string) (string, bool) { return "test-secret", name == "TEST_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	response, err := chatModel.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	if response.ResponseMeta == nil || response.ResponseMeta.Usage == nil || response.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 7 {
		t.Fatalf("response usage = %#v", response.ResponseMeta)
	}
}

func TestDeepSeekStreamingCacheUsageIsNormalized(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		stream := strings.Join([]string{
			`data: {"id":"deepseek-stream","object":"chat.completion.chunk","created":1,"model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"cached"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"deepseek-stream","object":"chat.completion.chunk","created":1,"model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"prompt_cache_hit_tokens":15,"prompt_cache_miss_tokens":5,"completion_tokens":2,"total_tokens":22}}` + "\n\n",
			"data: [DONE]\n\n",
		}, "")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(stream)),
			Request:    request,
		}, nil
	})}

	chatModel, err := newWithLookup(context.Background(), Config{
		Provider: ProviderOpenAI, Model: "deepseek-chat", BaseURL: "https://model.test/v1",
		APIKeyEnvironment: "TEST_SECRET", Timeout: time.Second, HTTPClient: client,
	}, func(name string) (string, bool) { return "test-secret", name == "TEST_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	stream, err := chatModel.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	message, err := schema.ConcatMessageStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "cached" || message.ResponseMeta == nil || message.ResponseMeta.Usage == nil || message.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 15 {
		t.Fatalf("streamed message = %#v", message)
	}
}

func TestResponsesWireRequestAndToolRoundTrip(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if authorization := request.Header.Get("Authorization"); authorization != "Bearer test-secret" {
			t.Errorf("authorization = %q", authorization)
		}
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort  string `json:"effort"`
				Summary string `json:"summary"`
			} `json:"reasoning"`
			Input []struct {
				Type   string `json:"type"`
				Role   string `json:"role"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			} `json:"input"`
			Tools []struct {
				Type       string         `json:"type"`
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		} else {
			if body.Model != "test-model" {
				t.Errorf("model = %q", body.Model)
			}
			if body.Reasoning.Effort != "medium" || body.Reasoning.Summary != "auto" {
				t.Errorf("reasoning = %#v", body.Reasoning)
			}
			if len(body.Input) != 3 {
				t.Errorf("input item count = %d, want 3", len(body.Input))
			}
			if len(body.Input) >= 3 {
				if body.Input[1].Type != "function_call" || body.Input[1].CallID != "call-1" {
					t.Errorf("function call item = %#v", body.Input[1])
				}
				if body.Input[2].Type != "function_call_output" || body.Input[2].CallID != "call-1" || body.Input[2].Output != `{"value":"ok"}` {
					t.Errorf("function output item = %#v", body.Input[2])
				}
			}
			if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Name != "runtime_diagnostics" {
				t.Errorf("tools = %#v", body.Tools)
			}
			if body.Tools[0].Parameters["type"] != "object" {
				t.Errorf("tool parameters = %#v", body.Tools[0].Parameters)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp-test","status":"completed","output_text":"responses works","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses works"}]}],"usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":2},"output_tokens":2,"total_tokens":5}}`)),
			Request:    request,
		}, nil
	})}

	chatModel, err := newWithLookup(context.Background(), Config{
		Provider:          ProviderOpenAI,
		WireAPI:           WireAPI("RESPONSES"),
		Model:             "test-model",
		BaseURL:           "https://model.test/v1",
		APIKeyEnvironment: "TEST_SECRET",
		Timeout:           time.Second,
		HTTPClient:        client,
	}, func(name string) (string, bool) { return "test-secret", name == "TEST_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	withTools, err := chatModel.WithTools([]*schema.ToolInfo{{Name: "runtime_diagnostics", Desc: "diagnostics"}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := withTools.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("run it"),
		schema.AssistantMessage("", []schema.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: schema.FunctionCall{
				Name:      "runtime_diagnostics",
				Arguments: `{}`,
			},
		}}),
		schema.ToolMessage(`{"value":"ok"}`, "call-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "responses works" || response.ResponseMeta == nil || response.ResponseMeta.Usage.TotalTokens != 5 || response.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 2 {
		t.Fatalf("response = %#v", response)
	}
}

func TestDecodeResponsesFunctionCall(t *testing.T) {
	message, err := decodeResponsesMessage([]byte(`{"id":"resp-tool","status":"completed","output":[{"type":"function_call","call_id":"call-7","name":"runtime_diagnostics","arguments":"{}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call-7" || message.ToolCalls[0].Function.Name != "runtime_diagnostics" || message.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("message = %#v", message)
	}
}

func TestResponsesStreamingTextAndUsage(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("accept = %q", request.Header.Get("Accept"))
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		} else if !body.Stream {
			t.Error("stream flag is false")
		}
		stream := strings.Join([]string{
			"event: response.reasoning_summary_text.delta\n",
			"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"plan\"}\n\n",
			"event: response.output_text.delta\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n",
			"event: response.output_text.delta\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n",
			"event: response.completed\n",
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens\":2,\"total_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\n\n",
		}, "")
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: request}, nil
	})}
	chatModel, err := newWithLookup(context.Background(), Config{
		Provider: ProviderOpenAI, WireAPI: WireAPIResponses, Model: "test-model",
		BaseURL: "https://model.test/v1", APIKeyEnvironment: "TEST_SECRET", Timeout: time.Second, HTTPClient: client,
	}, func(name string) (string, bool) { return "test-secret", name == "TEST_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	stream, err := chatModel.Stream(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	message, err := schema.ConcatMessageStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "hello" || message.ReasoningContent != "plan" || message.ResponseMeta == nil || message.ResponseMeta.Usage.TotalTokens != 5 || message.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 2 || message.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 4 {
		t.Fatalf("streamed message = %#v", message)
	}
}

func TestDecodeResponsesReasoningSummary(t *testing.T) {
	message, err := decodeResponsesMessage([]byte(`{"id":"resp-reasoning","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":3,"total_tokens":4,"output_tokens_details":{"reasoning_tokens":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "done" || message.ReasoningContent != "plan" || message.ResponseMeta == nil || message.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("message = %#v", message)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type inertRoundTripper struct{}

func (*inertRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected request")
}

func TestConstructsPinnedProviderAdaptersWithoutNetworkCall(t *testing.T) {
	t.Parallel()

	openAIModel, err := newWithLookup(context.Background(), Config{
		Provider:          ProviderOpenAI,
		Model:             "test-model",
		BaseURL:           "http://127.0.0.1:9999/v1",
		APIKeyEnvironment: "TEST_SECRET",
		Timeout:           time.Second,
	}, func(name string) (string, bool) { return "not-a-real-secret", name == "TEST_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	if openAIModel == nil {
		t.Fatal("OpenAI model is nil")
	}

	ollamaModel, err := newWithLookup(context.Background(), Config{
		Provider: ProviderOllama,
		Model:    "test-model",
		Timeout:  time.Second,
	}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if ollamaModel == nil {
		t.Fatal("Ollama model is nil")
	}
}

func TestRejectsCredentialInBaseURL(t *testing.T) {
	t.Parallel()

	_, err := newWithLookup(context.Background(), Config{
		Provider: ProviderOllama,
		Model:    "test-model",
		BaseURL:  "http://secret@127.0.0.1:11434",
	}, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestModelClientsServeConcurrentAgents checks that one model client can be
// shared by parallel child Agents: every wire API answers concurrent Generate
// and Stream calls through a real HTTP server, and no caller receives another
// caller's answer. Run it with -race.
func TestModelClientsServeConcurrentAgents(t *testing.T) {
	marker := regexp.MustCompile(`agent-\d+`)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		answer := marker.FindString(string(body))
		streaming := strings.Contains(string(body), `"stream":true`)
		switch request.URL.Path {
		case "/v1/chat/completions":
			if !streaming {
				writer.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(writer, `{"id":"chat","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`, answer)
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(writer, "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", answer)
			fmt.Fprint(writer, "data: {\"id\":\"chat\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		case "/v1/responses":
			if !streaming {
				writer.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(writer, `{"id":"resp","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`, answer)
				return
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", answer)
			fmt.Fprint(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n")
		case "/api/chat":
			writer.Header().Set("Content-Type", "application/x-ndjson")
			if streaming {
				fmt.Fprintf(writer, "{\"model\":\"test-model\",\"created_at\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":%q},\"done\":false}\n", answer)
				answer = ""
			}
			fmt.Fprintf(writer, "{\"model\":\"test-model\",\"created_at\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":%q},\"done\":true,\"done_reason\":\"stop\",\"prompt_eval_count\":3,\"eval_count\":1}\n", answer)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	lookup := func(name string) (string, bool) { return "test-secret", name == "TEST_SECRET" }
	for _, config := range []Config{
		{Provider: ProviderOpenAI, WireAPI: WireAPIChatCompletions, Model: "test-model", BaseURL: server.URL + "/v1", APIKeyEnvironment: "TEST_SECRET", Timeout: 5 * time.Second},
		{Provider: ProviderOpenAI, WireAPI: WireAPIResponses, Model: "test-model", BaseURL: server.URL + "/v1", APIKeyEnvironment: "TEST_SECRET", Timeout: 5 * time.Second},
		{Provider: ProviderOllama, Model: "test-model", BaseURL: server.URL, Timeout: 5 * time.Second},
	} {
		t.Run(string(config.Provider)+"-"+string(config.WireAPI), func(t *testing.T) {
			chatModel, err := newWithLookup(context.Background(), config, lookup)
			if err != nil {
				t.Fatal(err)
			}
			shared, err := chatModel.WithTools([]*schema.ToolInfo{{Name: "runtime_diagnostics", Desc: "diagnostics"}})
			if err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			for agent := range 8 {
				group.Go(func() {
					want := fmt.Sprintf("agent-%d", agent)
					input := []*schema.Message{schema.UserMessage("answer with " + want)}
					for range 4 {
						generated, err := shared.Generate(context.Background(), input)
						if err != nil || generated.Content != want {
							t.Errorf("%s generated %v, %v", want, generated, err)
							return
						}
						stream, err := shared.Stream(context.Background(), input)
						if err != nil {
							t.Errorf("%s stream: %v", want, err)
							return
						}
						streamed, err := schema.ConcatMessageStream(stream)
						if err != nil || streamed.Content != want {
							t.Errorf("%s streamed %v, %v", want, streamed, err)
							return
						}
					}
				})
			}
			group.Wait()
		})
	}
}
