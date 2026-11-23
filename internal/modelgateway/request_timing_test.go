package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
)

type modelTimingWire struct {
	provider    Provider
	wireAPI     WireAPI
	contentType string
	delta       string
	done        string
}

var modelTimingWires = map[string]modelTimingWire{
	"Responses": {
		provider: ProviderOpenAI, wireAPI: WireAPIResponses, contentType: "text/event-stream",
		delta: `data: {"type":"response.output_text.delta","delta":"x"}` + "\n\n",
		done:  `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n",
	},
	"ChatCompletions": {
		provider: ProviderOpenAI, wireAPI: WireAPIChatCompletions, contentType: "text/event-stream",
		delta: `data: {"id":"chat","choices":[{"index":0,"delta":{"role":"assistant","content":"x"},"finish_reason":null}]}` + "\n\n",
		done:  `data: {"id":"chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
	},
	"Ollama": {
		provider: ProviderOllama, contentType: "application/x-ndjson",
		delta: `{"model":"test","message":{"role":"assistant","content":"x"},"done":false}` + "\n",
		done:  `{"model":"test","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}` + "\n",
	},
}

func TestHealthyModelStreamsCanOutlastRequestTimeout(t *testing.T) {
	for name, wire := range modelTimingWires {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const timeout = 240 * time.Millisecond
			const frames = 8
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", wire.contentType)
				for range frames {
					writeModelFrame(writer, wire.delta)
					select {
					case <-time.After(timeout / 4):
					case <-request.Context().Done():
						return
					}
				}
				writeModelFrame(writer, wire.done)
			}))
			t.Cleanup(server.Close)
			chat := timingTestModel(t, wire, server.URL, timeout, nil)
			bound, err := chat.WithTools([]*schema.ToolInfo{{Name: "test_tool", Desc: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx = modeltelemetry.CaptureStream(ctx)
			result, err := drainModelStream(ctx, bound)
			if err != nil || result != strings.Repeat("x", frames) {
				t.Fatalf("healthy long stream: result=%q error=%v", result, err)
			}
			if name == "Responses" {
				observation := modeltelemetry.ReadStream(ctx)
				if observation == nil || !observation.Completed || observation.Outcome != "completed" ||
					observation.LastEvent != "response.completed" || observation.BodyBytes != uint64(frames*len(wire.delta)+len(wire.done)) {
					t.Fatalf("completed stream observation: %+v", observation)
				}
			}
		})
	}
}

func TestModelTimeoutDistinguishesHeadersStreamReadAndUnaryRequest(t *testing.T) {
	for name, wire := range modelTimingWires {
		for _, phase := range []string{"response headers", "stream read", "request"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				t.Parallel()
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					_, _ = io.Copy(io.Discard, request.Body)
					switch phase {
					case "stream read":
						writer.Header().Set("Content-Type", wire.contentType+"; fixture=ignored")
						writer.Header().Set("X-Request-Id", "test-key")
						writer.Header().Set("OpenAI-Request-Id", "req_fixture-123")
						writeModelFrame(writer, wire.delta)
					case "request":
						writer.Header().Set("Content-Type", "application/json")
						writeModelFrame(writer, "{")
					}
					select {
					case <-request.Context().Done():
					case <-t.Context().Done():
					}
				}))
				t.Cleanup(server.Close)
				chat := timingTestModel(t, wire, server.URL, 150*time.Millisecond, nil)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = modeltelemetry.CaptureStream(ctx)
				var result string
				var err error
				if phase == "request" {
					_, err = chat.Generate(ctx, timingTestPrompt())
				} else {
					result, err = drainModelStream(ctx, chat)
				}
				var deadline *modelDeadline
				if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &deadline) || deadline.phase != phase {
					t.Fatalf("phase=%q error=%v", phase, err)
				}
				if phase == "stream read" && result != "x" {
					t.Fatalf("partial text was not received before the timeout: %q", result)
				}
				if name != "Responses" || phase == "request" {
					return
				}
				observation := modeltelemetry.ReadStream(ctx)
				if observation == nil || observation.Completed || observation.StartedAt.IsZero() || observation.EndedAt.Before(observation.StartedAt) {
					t.Fatalf("timed out stream observation: %+v", observation)
				}
				if phase == "response headers" {
					if observation.Outcome != "response_headers_timeout" || !observation.HeadersAt.IsZero() || observation.BodyBytes != 0 {
						t.Fatalf("header timeout observation: %+v", observation)
					}
				} else if observation.Outcome != "stream_read_timeout" || observation.Status != http.StatusOK ||
					observation.ContentType != wire.contentType || observation.RequestID != "req_fixture-123" ||
					observation.LastEvent != "response.output_text.delta" || observation.BodyBytes != uint64(len(wire.delta)) ||
					observation.HeadersAt.Before(observation.StartedAt) || observation.FirstByteAt.Before(observation.HeadersAt) ||
					observation.LastByteAt.Before(observation.FirstByteAt) || observation.EndedAt.Before(observation.LastByteAt) {
					t.Fatalf("body timeout observation: %+v", observation)
				}
			})
		}
	}
}

func TestStreamingRespectsCallerCancellationAndClientTimeout(t *testing.T) {
	for name, wire := range modelTimingWires {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", wire.contentType)
				writeModelFrame(writer, wire.delta)
				select {
				case <-request.Context().Done():
				case <-t.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			t.Run("parent cancellation", func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				chat := timingTestModel(t, wire, server.URL, 2*time.Second, nil)
				stream, err := chat.Stream(ctx, timingTestPrompt())
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if _, err := stream.Recv(); err != nil {
					t.Fatal(err)
				}
				cancel()
				if _, err := stream.Recv(); !errors.Is(err, context.Canceled) {
					t.Fatalf("stream ignored caller cancellation: %v", err)
				}
			})
			t.Run("parent deadline", func(t *testing.T) {
				chat := timingTestModel(t, wire, server.URL, 2*time.Second, nil)
				ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
				defer cancel()
				_, err := drainModelStream(ctx, chat)
				var ownDeadline *modelDeadline
				if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ownDeadline) {
					t.Fatalf("caller deadline was overridden: %v", err)
				}
			})
			t.Run("injected client deadline", func(t *testing.T) {
				client := *server.Client()
				client.Timeout = 150 * time.Millisecond
				transport := client.Transport
				chat := timingTestModel(t, wire, server.URL, 2*time.Second, &client)
				_, err := drainModelStream(t.Context(), chat)
				var ownDeadline *modelDeadline
				if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ownDeadline) {
					t.Fatalf("injected HTTP client deadline was overridden: %v", err)
				}
				if client.Timeout != 150*time.Millisecond || client.Transport != transport {
					t.Fatal("injected HTTP client was modified")
				}
			})
		})
	}
}

func timingTestModel(t *testing.T, wire modelTimingWire, baseURL string, timeout time.Duration, client *http.Client) model.ToolCallingChatModel {
	t.Helper()
	if wire.provider == ProviderOpenAI {
		baseURL += "/v1"
	}
	chat, err := newWithLookup(t.Context(), Config{
		Provider: wire.provider, WireAPI: wire.wireAPI, Model: "test", BaseURL: baseURL, Timeout: timeout, HTTPClient: client,
	}, func(string) (string, bool) { return "test-key", true })
	if err != nil {
		t.Fatal(err)
	}
	return chat
}

func timingTestPrompt() []*schema.Message { return []*schema.Message{schema.UserMessage("hello")} }

func writeModelFrame(writer http.ResponseWriter, frame string) {
	_, _ = fmt.Fprint(writer, frame)
	writer.(http.Flusher).Flush()
}

func drainModelStream(ctx context.Context, chat model.ToolCallingChatModel) (string, error) {
	stream, err := chat.Stream(ctx, timingTestPrompt())
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var content strings.Builder
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return content.String(), nil
		}
		if err != nil {
			return content.String(), err
		}
		content.WriteString(message.Content)
	}
}
