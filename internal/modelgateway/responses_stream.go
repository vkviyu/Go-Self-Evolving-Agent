package modelgateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"
)

var ErrResponsesStreamTruncated = errors.New("Responses stream ended before response.completed")

// streamResponsesBody parses SSE frames and forwards normalized message
// chunks. It owns only stream state; HTTP setup is handled by the transport
// layer and request conversion remains in responses.go.
func streamResponsesBody(body io.ReadCloser, writer *schema.StreamWriter[*schema.Message]) {
	defer body.Close()
	defer writer.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4<<10), maxResponsesBodyBytes)
	var eventName string
	var data strings.Builder
	callStates := make(map[string]*responseStreamCallState)
	textSeen := false
	reasoningSeen := false
	outputIndex := 0

	flush := func() bool {
		if data.Len() == 0 {
			eventName = ""
			return false
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "[DONE]" {
			writer.Send(nil, fmt.Errorf("%w: unexpected [DONE] sentinel", ErrResponsesStreamTruncated))
			return true
		}
		var event responsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			writer.Send(nil, fmt.Errorf("decode Responses stream event %q: %w", eventName, err))
			eventName = ""
			return true
		}
		if event.Type == "" {
			event.Type = eventName
		} else if eventName != "" && eventName != event.Type {
			writer.Send(nil, errors.New("Responses SSE event name and payload type disagree"))
			return true
		}
		stop, err := emitResponsesStreamEvent(event, writer, callStates, &textSeen, &reasoningSeen, &outputIndex)
		if err != nil {
			writer.Send(nil, err)
			stop = true
		}
		eventName = ""
		return stop
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if flush() {
				return
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len()+len(line) > maxResponsesBodyBytes {
				writer.Send(nil, errors.New("Responses stream frame exceeds size limit"))
				return
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if scanner.Err() != nil {
		writer.Send(nil, fmt.Errorf("read Responses stream: %w", scanner.Err()))
		return
	}
	if !flush() {
		writer.Send(nil, ErrResponsesStreamTruncated)
	}
}

func emitResponsesStreamEvent(event responsesStreamEvent, writer *schema.StreamWriter[*schema.Message], callStates map[string]*responseStreamCallState, textSeen, reasoningSeen *bool, outputIndex *int) (bool, error) {
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta == "" {
			return false, nil
		}
		*textSeen = true
		return sendResponsesMessage(writer, schema.AssistantMessage(event.Delta, nil))
	case "response.reasoning_summary_text.delta":
		if event.Delta == "" {
			return false, nil
		}
		message := schema.AssistantMessage("", nil)
		message.ReasoningContent = event.Delta
		*reasoningSeen = true
		return sendResponsesMessage(writer, message)
	case "response.output_item.added":
		item, err := decodeResponsesStreamItem(event.Item)
		if err != nil {
			return false, err
		}
		if item.Type != "function_call" {
			return false, nil
		}
		key := firstNonEmpty(item.ID, event.ItemID, item.CallID, event.CallID)
		if key == "" {
			return false, errors.New("Responses function call stream item is missing an ID")
		}
		index := streamOutputIndex(event.OutputIndex, outputIndex)
		callStates[key] = &responseStreamCallState{index: index, callID: firstNonEmpty(item.CallID, event.CallID), name: item.Name}
		if writer.Send(schema.AssistantMessage("", []schema.ToolCall{{
			Index: &index,
			ID:    firstNonEmpty(item.CallID, event.CallID),
			Type:  "function",
			Function: schema.FunctionCall{
				Name: item.Name,
			},
		}}), nil) {
			return true, nil
		}
		return false, nil
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if event.Delta == "" {
			return false, nil
		}
		key := firstNonEmpty(event.ItemID, event.CallID)
		state := callStates[key]
		if state == nil {
			state = &responseStreamCallState{index: streamOutputIndex(event.OutputIndex, outputIndex), callID: event.CallID}
			callStates[key] = state
		}
		state.argumentsSeen = true
		index := state.index
		if writer.Send(schema.AssistantMessage("", []schema.ToolCall{{
			Index: &index,
			ID:    firstNonEmpty(state.callID, event.CallID),
			Type:  "function",
			Function: schema.FunctionCall{
				Name:      state.name,
				Arguments: event.Delta,
			},
		}}), nil) {
			return true, nil
		}
		return false, nil
	case "response.output_item.done":
		item, err := decodeResponsesStreamItem(event.Item)
		if err != nil {
			return false, err
		}
		if item.Type != "function_call" {
			if item.Type == "reasoning" && !*reasoningSeen {
				text := responseItemText("", item.Summary)
				if text != "" {
					message := schema.AssistantMessage("", nil)
					message.ReasoningContent = text
					*reasoningSeen = true
					return sendResponsesMessage(writer, message)
				}
			}
			if *textSeen {
				return false, nil
			}
			text := responseItemText(item.ContentString(), item.Content)
			if text != "" {
				*textSeen = true
				if writer.Send(schema.AssistantMessage(text, nil), nil) {
					return true, nil
				}
			}
			return false, nil
		}
		key := firstNonEmpty(item.ID, event.ItemID, item.CallID, event.CallID)
		if key == "" {
			return false, errors.New("Responses function call stream item is missing an ID")
		}
		state := callStates[key]
		if state != nil && state.argumentsSeen {
			return false, nil
		}
		if state == nil {
			state = &responseStreamCallState{index: streamOutputIndex(event.OutputIndex, outputIndex)}
			callStates[key] = state
		}
		state.callID = firstNonEmpty(item.CallID, event.CallID, state.callID, item.ID)
		state.name = firstNonEmpty(item.Name, state.name)
		if state.callID == "" || state.name == "" {
			return false, errors.New("Responses function call is missing an ID or name")
		}
		state.argumentsSeen = true
		index := state.index
		if writer.Send(schema.AssistantMessage("", []schema.ToolCall{{
			Index: &index,
			ID:    state.callID,
			Type:  "function",
			Function: schema.FunctionCall{
				Name:      state.name,
				Arguments: decodeResponsesArguments(item.Arguments),
			},
		}}), nil) {
			return true, nil
		}
		return false, nil
	case "error":
		return true, fmt.Errorf("Responses stream error: %s", firstNonEmpty(event.Message, event.Code, "unspecified provider error"))
	case "response.failed", "response.incomplete", "response.cancelled":
		return false, errors.New(responsesStreamFailureMessage(event))
	case "response.completed":
		var response responsesStreamResponse
		if err := json.Unmarshal(event.Response, &response); err != nil {
			return true, fmt.Errorf("decode completed Responses metadata: %w", err)
		}
		if response.Status != "completed" {
			return true, fmt.Errorf("response.completed has invalid status %q", response.Status)
		}
		writer.Send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{
			FinishReason: response.Status, Usage: response.Usage.toSchemaTokenUsage(),
		}}, nil)
		// This is the semantic terminal boundary. Do not wait for the gateway
		// to close its HTTP connection or emit a Chat-Completions sentinel.
		return true, nil
	default:
		return false, nil
	}
}

func sendResponsesMessage(writer *schema.StreamWriter[*schema.Message], message *schema.Message) (bool, error) {
	if writer.Send(message, nil) {
		return true, nil
	}
	return false, nil
}

func decodeResponsesStreamItem(raw json.RawMessage) (responsesStreamItem, error) {
	if len(raw) == 0 {
		return responsesStreamItem{}, errors.New("Responses stream item is missing")
	}
	var item responsesStreamItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return responsesStreamItem{}, fmt.Errorf("decode Responses stream item: %w", err)
	}
	return item, nil
}

func (item responsesStreamItem) ContentString() string {
	var text string
	if json.Unmarshal(item.Content, &text) == nil {
		return text
	}
	return ""
}

func streamOutputIndex(value *int, next *int) int {
	if value != nil {
		if *value >= *next {
			*next = *value + 1
		}
		return *value
	}
	index := *next
	(*next)++
	return index
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func responsesStreamFailureMessage(event responsesStreamEvent) string {
	if len(event.Response) > 0 {
		var response struct {
			Error *responsesResponseError `json:"error,omitempty"`
		}
		if json.Unmarshal(event.Response, &response) == nil && response.Error != nil && response.Error.Message != "" {
			return response.Error.Message
		}
	}
	if event.Type == "response.incomplete" {
		return "Responses stream ended with an incomplete response"
	}
	return "Responses stream failed"
}
