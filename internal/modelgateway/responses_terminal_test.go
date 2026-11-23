package modelgateway

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
)

func responseFrames(frames ...string) string {
	var result strings.Builder
	for _, frame := range frames {
		result.WriteString("data: ")
		result.WriteString(frame)
		result.WriteString("\n\n")
	}
	return result.String()
}

func consumeResponseFrames(body io.ReadCloser) ([]*schema.Message, error) {
	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer body.Close()
		defer writer.Close()
		if err := streamResponsesBody(body, writer, new(modeltelemetry.Stream)); err != nil {
			writer.Send(nil, err)
		}
	}()
	defer reader.Close()
	var messages []*schema.Message
	for {
		message, err := reader.Recv()
		if errors.Is(err, io.EOF) {
			return messages, nil
		}
		if err != nil {
			return messages, err
		}
		messages = append(messages, message)
	}
}

func TestResponsesRequiresSemanticTerminalEvent(t *testing.T) {
	for _, body := range []string{
		"", responseFrames(`{"type":"response.output_text.delta","delta":"partial"}`),
		responseFrames(`{"type":"response.function_call_arguments.delta","item_id":"f","delta":"{}"}`),
		responseFrames(`{"type":"response.output_item.done","item":{"type":"message","content":"partial"}}`),
		responseFrames(`[DONE]`),
	} {
		_, err := consumeResponseFrames(io.NopCloser(strings.NewReader(body)))
		if !errors.Is(err, ErrResponsesStreamTruncated) {
			t.Fatalf("body=%q err=%v", body, err)
		}
	}
}

func TestResponsesStopsAtCompletionWithoutWaitingForHTTPClose(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { reader.Close(); writer.Close() })
	done := make(chan error, 1)
	go func() { _, err := consumeResponseFrames(reader); done <- err }()
	if _, err := io.WriteString(writer, responseFrames(`{"type":"response.output_text.delta","delta":"complete"}`, `{"type":"response.completed","response":{"status":"completed"}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("semantic completion still waits for HTTP EOF")
	}
}

func TestResponsesRejectsErrorInvalidCompletionAndMalformedFrames(t *testing.T) {
	for _, body := range []string{
		responseFrames(`{"type":"error","message":"provider failed"}`),
		responseFrames(`{"type":"response.failed","response":{"error":{"message":"failed"}}}`),
		responseFrames(`{"type":"response.incomplete","response":{"status":"incomplete"}}`),
		responseFrames(`{"type":"response.completed"}`),
		responseFrames(`{"type":"response.completed","response":null}`),
		responseFrames(`{"type":"response.completed","response":{"status":"in_progress"}}`),
		responseFrames(`{"type":"response.completed",`),
		"event: response.completed\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"mismatch\"}\n\n",
	} {
		if _, err := consumeResponseFrames(io.NopCloser(strings.NewReader(body))); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestResponsesFinalFrameWithoutBlankLineAndNoUsage(t *testing.T) {
	body := responseFrames(`{"type":"response.output_text.delta","delta":"done"}`) + `data: {"type":"response.completed","response":{"status":"completed"}}`
	chunks, err := consumeResponseFrames(io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	message, err := schema.ConcatMessages(chunks)
	if err != nil || message.Content != "done" || message.ResponseMeta.FinishReason != "completed" || message.ResponseMeta.Usage != nil {
		t.Fatalf("message=%#v err=%v", message, err)
	}
}

func TestResponsesOutputLimitIsPresentOnWire(t *testing.T) {
	name, limit := "configured-model", 37
	request, err := buildResponsesRequest(&model.Options{Model: &name, MaxTokens: &limit}, []*schema.Message{schema.UserMessage("hello")}, "none")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeResponsesRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_output_tokens"] != float64(limit) {
		t.Fatalf("missing output limit: %s", encoded)
	}
}

func TestResponsesGeneratePreservesCompletionWithoutUsage(t *testing.T) {
	message, err := decodeResponsesPayload([]byte(`{"status":"completed","output_text":"complete answer"}`))
	if err != nil || message == nil || message.Content != "complete answer" || message.ResponseMeta == nil || message.ResponseMeta.FinishReason != "completed" || message.ResponseMeta.Usage != nil {
		t.Fatalf("message=%#v err=%v", message, err)
	}
}
