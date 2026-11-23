package modelgateway

import (
	"encoding/json"
	"net/http"

	"github.com/cloudwego/eino/schema"
)

type responsesModelConfig struct {
	APIKey          string
	Model           string
	BaseURL         string
	ReasoningEffort string
	HTTPClient      *http.Client
}

type responsesModel struct {
	apiKey          string
	model           string
	endpoint        string
	reasoningEffort string
	httpClient      *http.Client
	tools           []*schema.ToolInfo
}

type responsesRequest struct {
	Model           string                    `json:"model"`
	Input           []responsesInputItem      `json:"input,omitempty"`
	Tools           []responsesFunctionTool   `json:"tools,omitempty"`
	ToolChoice      any                       `json:"tool_choice,omitempty"`
	Reasoning       *responsesReasoningConfig `json:"reasoning,omitempty"`
	Stream          bool                      `json:"stream,omitempty"`
	MaxOutputTokens *int                      `json:"max_output_tokens,omitempty"`
}

type responsesReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesStreamEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta,omitempty"`
	ItemID      string          `json:"item_id,omitempty"`
	CallID      string          `json:"call_id,omitempty"`
	OutputIndex *int            `json:"output_index,omitempty"`
	Item        json.RawMessage `json:"item,omitempty"`
	Response    json.RawMessage `json:"response,omitempty"`
	Message     string          `json:"message,omitempty"`
	Code        string          `json:"code,omitempty"`
}

type responsesStreamItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Summary   json.RawMessage `json:"summary,omitempty"`
}

type responsesStreamResponse struct {
	Status string                       `json:"status,omitempty"`
	Usage  *responsesResponseTokenUsage `json:"usage,omitempty"`
}

type responseStreamCallState struct {
	index         int
	callID        string
	name          string
	argumentsSeen bool
}

type responsesInputItem struct {
	Type      string                 `json:"type,omitempty"`
	Role      string                 `json:"role,omitempty"`
	Content   []responsesContentPart `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
	Output    string                 `json:"output,omitempty"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesFunctionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	// Always emit false: Responses may normalize omitted strict into strict
	// mode, making optional parameters required. Do not add omitempty.
	Strict bool `json:"strict"`
}

type responsesResponse struct {
	ID         string                       `json:"id"`
	Status     string                       `json:"status"`
	OutputText string                       `json:"output_text"`
	Output     []json.RawMessage            `json:"output"`
	Error      *responsesResponseError      `json:"error,omitempty"`
	Usage      *responsesResponseTokenUsage `json:"usage,omitempty"`
}

type responsesResponseError struct {
	Message string `json:"message"`
}

type responsesResponseTokenUsage struct {
	InputTokens        int                         `json:"input_tokens"`
	OutputTokens       int                         `json:"output_tokens"`
	TotalTokens        int                         `json:"total_tokens"`
	InputTokenDetails  responsesInputTokenDetails  `json:"input_tokens_details"`
	PromptTokenDetails responsesInputTokenDetails  `json:"prompt_tokens_details"`
	PromptCacheHit     int                         `json:"prompt_cache_hit_tokens,omitempty"`
	OutputTokenDetails responsesOutputTokenDetails `json:"output_tokens_details"`
}

func (usage *responsesResponseTokenUsage) toSchemaTokenUsage() *schema.TokenUsage {
	if usage == nil {
		return nil
	}
	cachedTokens := usage.InputTokenDetails.CachedTokens
	if cachedTokens == 0 {
		cachedTokens = usage.PromptTokenDetails.CachedTokens
	}
	if cachedTokens == 0 {
		cachedTokens = usage.PromptCacheHit
	}
	return &schema.TokenUsage{
		PromptTokens: usage.InputTokens,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: cachedTokens,
		},
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: usage.OutputTokenDetails.ReasoningTokens,
		},
	}
}

type responsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type responsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}
