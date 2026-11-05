package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const (
	defaultResponsesBaseURL = "https://api.openai.com/v1"
	maxResponsesBodyBytes   = 8 << 20
)

var _ model.ToolCallingChatModel = (*responsesModel)(nil)

func newResponsesModel(config responsesModelConfig) (*responsesModel, error) {
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" {
		baseURL = defaultResponsesBaseURL
	}
	endpoint, err := responsesEndpoint(baseURL)
	if err != nil {
		return nil, err
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	reasoningEffort, err := normalizeReasoningEffort(config.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	return &responsesModel{
		apiKey:          config.APIKey,
		model:           config.Model,
		endpoint:        endpoint,
		reasoningEffort: reasoningEffort,
		timeout:         config.Timeout,
		httpClient:      client,
	}, nil
}

func normalizeReasoningEffort(value string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(value))
	if effort == "" {
		effort = "medium"
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return effort, nil
	default:
		return "", fmt.Errorf("unsupported Responses reasoning effort %q (supported: none, minimal, low, medium, high, xhigh)", value)
	}
}

func responsesEndpoint(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid Responses base URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("Responses base URL must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return "", errors.New("Responses base URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Responses base URL must not contain a query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/responses") {
		parsed.Path += "/responses"
	}
	return parsed.String(), nil
}

func (m *responsesModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	copyModel := *m
	copyModel.tools = append([]*schema.ToolInfo(nil), tools...)
	return &copyModel, nil
}

func (m *responsesModel) Generate(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.Message, error) {
	common := model.GetCommonOptions(&model.Options{
		Model: &m.model,
		Tools: m.tools,
	}, options...)
	requestBody, err := buildResponsesRequest(common, input, m.reasoningEffort)
	if err != nil {
		return nil, fmt.Errorf("build Responses request: %w", err)
	}
	payload, err := encodeResponsesRequest(requestBody)
	if err != nil {
		return nil, err
	}
	response, cancel, err := m.doResponsesRequest(ctx, payload, false)
	if err != nil {
		return nil, err
	}
	if cancel != nil {
		defer cancel()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponsesBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read Responses response: %w", err)
	}
	return decodeResponsesPayload(body)
}

func (m *responsesModel) Stream(ctx context.Context, input []*schema.Message, options ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	common := model.GetCommonOptions(&model.Options{
		Model: &m.model,
		Tools: m.tools,
	}, options...)
	requestBody, err := buildResponsesRequest(common, input, m.reasoningEffort)
	if err != nil {
		return nil, fmt.Errorf("build streaming Responses request: %w", err)
	}
	requestBody.Stream = true
	payload, err := encodeResponsesRequest(requestBody)
	if err != nil {
		return nil, fmt.Errorf("encode streaming Responses request: %w", err)
	}

	response, cancel, err := m.doResponsesRequest(ctx, payload, true)
	if err != nil {
		return nil, fmt.Errorf("send streaming Responses request: %w", err)
	}

	reader, writer := schema.Pipe[*schema.Message](16)
	go func() {
		defer func() {
			if cancel != nil {
				cancel()
			}
		}()
		streamResponsesBody(response.Body, writer)
	}()
	return reader, nil
}

func buildResponsesRequest(options *model.Options, input []*schema.Message, reasoningEffort string) (responsesRequest, error) {
	request := responsesRequest{}
	if options == nil || options.Model == nil || strings.TrimSpace(*options.Model) == "" {
		return request, errors.New("model name is required")
	}
	request.Model = *options.Model
	if options.MaxTokens != nil {
		if *options.MaxTokens <= 0 {
			return request, errors.New("Responses output token limit must be positive")
		}
		limit := *options.MaxTokens
		request.MaxOutputTokens = &limit
	}
	for _, message := range input {
		if message == nil {
			return request, errors.New("input contains a nil message")
		}
		items, err := responseInputItems(message)
		if err != nil {
			return request, err
		}
		request.Input = append(request.Input, items...)
	}
	for _, tool := range options.Tools {
		converted, err := responseFunctionTool(tool)
		if err != nil {
			return request, err
		}
		request.Tools = append(request.Tools, converted)
	}
	toolChoice, err := responseToolChoice(options)
	if err != nil {
		return request, err
	}
	request.ToolChoice = toolChoice
	if reasoningEffort != "" {
		request.Reasoning = &responsesReasoningConfig{Effort: reasoningEffort}
		if reasoningEffort != "none" {
			request.Reasoning.Summary = "auto"
		}
	}
	return request, nil
}

func responseToolChoice(options *model.Options) (any, error) {
	if options == nil || options.ToolChoice == nil {
		return nil, nil
	}
	toolNames := make([]map[string]string, 0, len(options.AllowedToolNames))
	for _, name := range options.AllowedToolNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		toolNames = append(toolNames, map[string]string{"type": "function", "name": name})
	}
	switch *options.ToolChoice {
	case schema.ToolChoiceForbidden:
		return "none", nil
	case schema.ToolChoiceAllowed:
		if len(toolNames) == 0 {
			return "auto", nil
		}
		return map[string]any{"type": "allowed_tools", "mode": "auto", "tools": toolNames}, nil
	case schema.ToolChoiceForced:
		if len(toolNames) == 1 {
			return map[string]string{"type": "function", "name": toolNames[0]["name"]}, nil
		}
		if len(toolNames) > 1 {
			return map[string]any{"type": "allowed_tools", "mode": "required", "tools": toolNames}, nil
		}
		return "required", nil
	default:
		return nil, fmt.Errorf("unsupported tool choice %q for Responses API", *options.ToolChoice)
	}
}

func responseInputItems(message *schema.Message) ([]responsesInputItem, error) {
	content := messageContent(message)
	switch message.Role {
	case schema.System, schema.User:
		if content == "" {
			return nil, nil
		}
		return []responsesInputItem{{
			Role:    string(message.Role),
			Content: []responsesContentPart{{Type: "input_text", Text: content}},
		}}, nil
	case schema.Assistant:
		items := make([]responsesInputItem, 0, len(message.ToolCalls)+1)
		if content != "" {
			items = append(items, responsesInputItem{
				Role:    "assistant",
				Content: []responsesContentPart{{Type: "output_text", Text: content}},
			})
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Function.Name == "" {
				return nil, errors.New("assistant tool call requires an ID and function name")
			}
			items = append(items, responsesInputItem{
				Type:      "function_call",
				CallID:    call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
		return items, nil
	case schema.Tool:
		if message.ToolCallID == "" {
			return nil, errors.New("tool message requires a tool call ID")
		}
		return []responsesInputItem{{
			Type:   "function_call_output",
			CallID: message.ToolCallID,
			Output: content,
		}}, nil
	default:
		return nil, fmt.Errorf("unsupported message role %q for Responses API", message.Role)
	}
}

func messageContent(message *schema.Message) string {
	if message.Content != "" {
		return message.Content
	}
	for _, part := range message.UserInputMultiContent {
		if part.Type == schema.ChatMessagePartTypeText {
			return part.Text
		}
	}
	return ""
}

func responseFunctionTool(tool *schema.ToolInfo) (responsesFunctionTool, error) {
	if tool == nil || strings.TrimSpace(tool.Name) == "" {
		return responsesFunctionTool{}, errors.New("Responses tool requires a name")
	}
	parameters := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	if tool.ParamsOneOf != nil {
		schemaValue, err := tool.ParamsOneOf.ToJSONSchema()
		if err != nil {
			return responsesFunctionTool{}, fmt.Errorf("convert tool %q parameters: %w", tool.Name, err)
		}
		encoded, err := json.Marshal(schemaValue)
		if err != nil {
			return responsesFunctionTool{}, fmt.Errorf("encode tool %q parameters: %w", tool.Name, err)
		}
		if err := json.Unmarshal(encoded, &parameters); err != nil {
			return responsesFunctionTool{}, fmt.Errorf("decode tool %q parameters: %w", tool.Name, err)
		}
	}
	return responsesFunctionTool{
		Type:        "function",
		Name:        tool.Name,
		Description: tool.Desc,
		Parameters:  parameters,
	}, nil
}

func decodeResponsesMessage(body []byte) (*schema.Message, error) {
	var response responsesResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Error != nil && response.Error.Message != "" {
		return nil, errors.New(response.Error.Message)
	}
	message := schema.AssistantMessage("", nil)
	for _, rawItem := range response.Output {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			CallID    string          `json:"call_id"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Text      string          `json:"text"`
			Content   json.RawMessage `json:"content"`
			Summary   json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, err
		}
		switch item.Type {
		case "message", "output_text":
			message.Content += responseItemText(item.Text, item.Content)
		case "reasoning":
			message.ReasoningContent += responseItemText("", item.Summary)
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			if callID == "" || item.Name == "" {
				return nil, errors.New("Responses function call is missing an ID or name")
			}
			message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
				ID:   callID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      item.Name,
					Arguments: decodeResponsesArguments(item.Arguments),
				},
			})
		}
	}
	if message.Content == "" {
		message.Content = response.OutputText
	}
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return nil, errors.New("Responses response contains no assistant output")
	}
	message.ResponseMeta = &schema.ResponseMeta{
		FinishReason: response.Status,
		Usage:        response.Usage.toSchemaTokenUsage(),
	}
	return message, nil
}

// Responses encodes function arguments as a JSON string containing the tool's
// JSON input. Decode exactly that protocol layer, retaining object-valued input
// for compatible gateways. Tool argument validation remains the tool's job.
func decodeResponsesArguments(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func responseItemText(text string, content json.RawMessage) string {
	if text != "" {
		return text
	}
	var contentText string
	if json.Unmarshal(content, &contentText) == nil {
		return contentText
	}
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var result strings.Builder
	for _, rawPart := range parts {
		var part struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(rawPart, &part) == nil {
			result.WriteString(part.Text)
		}
	}
	return result.String()
}

func responseErrorText(body []byte) string {
	var envelope struct {
		Error *responsesResponseError `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil && envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 1024 {
		text = text[:1024] + "..."
	}
	if text == "" {
		return "empty response body"
	}
	return text
}
