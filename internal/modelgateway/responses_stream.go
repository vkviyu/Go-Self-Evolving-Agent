package modelgateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
)

var ErrResponsesStreamTruncated = fmt.Errorf("Responses stream ended before response.completed: %w", io.ErrUnexpectedEOF)

// streamResponsesBody parses SSE frames and forwards normalized message
// chunks. It owns only stream state; HTTP setup is handled by the transport
// layer and request conversion remains in responses.go.
func streamResponsesBody(body io.Reader, writer *schema.StreamWriter[*schema.Message], observation *responsesObservation, watchdog *modelEventWatchdog) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4<<10), maxResponsesBodyBytes)
	var sender responsesMessageSender = writer
	if observation.stats != nil {
		scanner.Split(observation.stats.scanLines)
		sender = responsesObservingSender{writer, observation}
	}
	var eventName string
	var data strings.Builder
	callStates := make(map[string]*responseStreamCallState)
	textSeen := false
	reasoningSeen := false
	outputIndex := 0

	flush := func() (bool, error) {
		if data.Len() == 0 {
			observation.emptyFrame()
			eventName = ""
			return false, nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "[DONE]" {
			observation.invalidFrame()
			return true, fmt.Errorf("%w: unexpected [DONE] sentinel", ErrResponsesStreamTruncated)
		}
		var event responsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			observation.invalidFrame()
			return true, fmt.Errorf("decode Responses stream event %q: %w", eventName, err)
		}
		if event.Type == "" {
			event.Type = eventName
		} else if eventName != "" && eventName != event.Type {
			observation.invalidFrame()
			return true, errors.New("Responses SSE event name and payload type disagree")
		}
		observation.LastEvent = responsesEventLabel(event.Type)
		observation.frame(observation.LastEvent)
		stop, err := emitResponsesStreamEvent(event, sender, callStates, &textSeen, &reasoningSeen, &outputIndex)
		if err == nil && !stop {
			watchdog.note(event)
		}
		observation.Completed = event.Type == "response.completed" && err == nil
		eventName = ""
		return stop, err
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if observation.stats != nil {
			observation.line(line, scanner.Err() != nil)
		}
		switch {
		case line == "":
			if stop, err := flush(); stop || err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len()+len(line) > maxResponsesBodyBytes {
				return errors.New("Responses stream frame exceeds size limit")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("read Responses stream: %w", scanner.Err())
	}
	if stop, err := flush(); stop || err != nil {
		return err
	}
	return ErrResponsesStreamTruncated
}

type responsesMessageSender interface {
	// Send forwards a response delta or stream error and reports whether the receiving
	// side has closed.
	Send(*schema.Message, error) bool
}

// emitResponsesStreamEvent translates a provider event into text, reasoning or tool deltas
// and signals when consumption should stop.
func emitResponsesStreamEvent(event responsesStreamEvent, writer responsesMessageSender, callStates map[string]*responseStreamCallState, textSeen, reasoningSeen *bool, outputIndex *int) (bool, error) {
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
		return true, responsesProviderFailure(event.Code, event.Message, "Responses stream error: "+firstNonEmpty(event.Message, event.Code, "unspecified provider error"))
	case "response.failed", "response.incomplete", "response.cancelled":
		return true, responsesStreamFailure(event)
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

func sendResponsesMessage(writer responsesMessageSender, message *schema.Message) (bool, error) {
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

// firstNonEmpty picks the first supplied protocol field with a nonempty string value.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func responsesStreamFailure(event responsesStreamEvent) error {
	if len(event.Response) > 0 {
		var response struct {
			Error *responsesResponseError `json:"error,omitempty"`
		}
		if json.Unmarshal(event.Response, &response) == nil && response.Error != nil {
			return responsesProviderFailure(response.Error.Code, response.Error.Message, firstNonEmpty(response.Error.Message, "Responses stream failed"))
		}
	}
	if event.Type == "response.incomplete" {
		return errors.New("Responses stream ended with an incomplete response")
	}
	return errors.New("Responses stream failed")
}

func responsesProviderFailure(code, message, fallback string) error {
	// Some Responses-compatible endpoints report this as structured code,
	// others place the two diagnostic fields in the error message.
	if code == "invalid_tool_call_envelope" ||
		strings.Contains(message, "cause_category=transport_format, cause_reason=invalid_envelope,") {
		return modeltelemetry.ErrToolCallFormat
	}
	return errors.New(fallback)
}
