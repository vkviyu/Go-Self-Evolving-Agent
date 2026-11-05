package modelgateway

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestResponsesCompleteArgumentsMatchNonStreaming(t *testing.T) {
	for _, test := range []struct {
		name string
		wire any
		want string
	}{
		{"object string", `{"path":"internal/agentflow/runtime.go"}`, `{"path":"internal/agentflow/runtime.go"}`},
		{"escaped and nested", `{"content":"你好\n\"quoted\"","nested":{"items":[1,true]}}`, `{"content":"你好\n\"quoted\"","nested":{"items":[1,true]}}`},
		{"compatible object", json.RawMessage(`{"path":"."}`), `{"path":"."}`},
		{"empty", "", ""},
		{"null", nil, ""},
		// Only the protocol string layer is decoded. Invalid tool input must
		// still reach the normal schema/argument validation unchanged.
		{"model supplied string", `"not an object"`, `"not an object"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := map[string]any{"type": "function_call", "id": "item-1", "call_id": "call-1", "name": "workspace_read", "arguments": test.wire}
			body, err := json.Marshal(map[string]any{"output": []any{item}})
			if err != nil {
				t.Fatal(err)
			}
			message, err := decodeResponsesMessage(body)
			if err != nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Arguments != test.want {
				t.Fatalf("non-streaming message=%+v err=%v want=%q", message, err, test.want)
			}
			for _, added := range []bool{false, true} {
				var frames []string
				if added {
					frames = append(frames, `{"type":"response.output_item.added","item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"workspace_read","arguments":""}}`)
				}
				frame, err := json.Marshal(map[string]any{"type": "response.output_item.done", "item": item})
				if err != nil {
					t.Fatal(err)
				}
				frames = append(frames, string(frame), string(frame)) // Replay must not append arguments twice.
				streamed := concatCompletedResponses(t, frames...)
				if len(streamed.ToolCalls) != 1 {
					t.Fatalf("added=%v tool calls=%+v", added, streamed.ToolCalls)
				}
				call := streamed.ToolCalls[0]
				if call.ID != "call-1" || call.Function.Name != "workspace_read" || call.Function.Arguments != test.want {
					t.Fatalf("added=%v call=%+v want arguments=%q", added, call, test.want)
				}
			}
		})
	}
}

func TestResponsesArgumentsInterleavedDeltaAndCompleteItems(t *testing.T) {
	message := concatCompletedResponses(t,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"a","call_id":"call-a","name":"workspace_read"}}`,
		`{"type":"response.output_item.added","output_index":4,"item":{"type":"function_call","id":"b","call_id":"call-b","name":"workspace_list"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"a","delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"b","delta":""}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"b","call_id":"call-b","name":"workspace_list","arguments":"{\"path\":\".\"}"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"a","delta":"\"main.go\"}"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","id":"a","call_id":"call-a","name":"workspace_read","arguments":"{\"path\":\"main.go\"}"}}`,
	)
	if len(message.ToolCalls) != 2 {
		t.Fatalf("calls=%+v", message.ToolCalls)
	}
	for index, want := range []struct {
		id, name, arguments string
		outputIndex         int
	}{{"call-a", "workspace_read", `{"path":"main.go"}`, 2}, {"call-b", "workspace_list", `{"path":"."}`, 4}} {
		call := message.ToolCalls[index]
		if call.ID != want.id || call.Function.Name != want.name || call.Function.Arguments != want.arguments || call.Index == nil || *call.Index != want.outputIndex {
			t.Fatalf("call=%+v want=%+v", call, want)
		}
	}
}

func concatCompletedResponses(t *testing.T, frames ...string) *schema.Message {
	t.Helper()
	frames = append(frames, `{"type":"response.completed","response":{"status":"completed"}}`)
	chunks, err := consumeResponseFrames(io.NopCloser(strings.NewReader(responseFrames(frames...))))
	if err != nil {
		t.Fatal(err)
	}
	message, err := schema.ConcatMessages(chunks)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
