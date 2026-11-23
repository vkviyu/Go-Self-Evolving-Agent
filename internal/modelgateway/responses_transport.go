package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
)

// doResponsesRequest owns HTTP request construction and response status
// handling. Protocol-specific encoding and stream decoding stay in
// responses.go, which keeps transport concerns independently testable.
func (m *responsesModel) doResponsesRequest(ctx context.Context, payload []byte, observation *modeltelemetry.Stream) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create Responses request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+m.apiKey)
	request.Header.Set("Content-Type", "application/json")
	if observation != nil {
		request.Header.Set("Accept", "text/event-stream")
	}
	response, err := m.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send Responses request: %w", err)
	}
	if observation != nil {
		observeResponsesHeaders(response, observation, m.apiKey)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, nil
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponsesBodyBytes))
	if readErr != nil {
		return nil, fmt.Errorf("read Responses error: %w", readErr)
	}
	return nil, &HTTPStatusError{Status: response.StatusCode, Message: responseErrorText(body)}
}

func decodeResponsesPayload(payload []byte) (*schema.Message, error) {
	message, err := decodeResponsesMessage(payload)
	if err != nil {
		return nil, fmt.Errorf("decode Responses response: %w", err)
	}
	return message, nil
}

func encodeResponsesRequest(request responsesRequest) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode Responses request: %w", err)
	}
	return payload, nil
}
