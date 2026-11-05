package modelgateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// cacheUsageHTTPClient normalizes provider-specific OpenAI-compatible usage
// fields before the Eino adapter decodes them. OpenAI already emits
// prompt_tokens_details.cached_tokens; DeepSeek emits
// prompt_cache_hit_tokens/prompt_cache_miss_tokens.
func cacheUsageHTTPClient(client *http.Client, timeout time.Duration) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else {
		clone := *client
		client = &clone
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = cacheUsageTransport{inner: transport}
	return client
}

type cacheUsageTransport struct {
	inner http.RoundTripper
}

func (transport cacheUsageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.inner.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		response.Body = normalizeCacheUsageStream(response.Body)
		response.ContentLength = -1
		response.Header.Del("Content-Length")
		return response, nil
	}
	payload, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read model response for cache usage: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close model response for cache usage: %w", closeErr)
	}
	normalized := normalizeCacheUsageJSON(payload)
	response.Body = io.NopCloser(bytes.NewReader(normalized))
	response.ContentLength = int64(len(normalized))
	response.Header.Set("Content-Length", fmt.Sprintf("%d", len(normalized)))
	return response, nil
}

func normalizeCacheUsageStream(body io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer body.Close()
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 4<<10), maxResponsesBodyBytes)
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if payload, ok := strings.CutPrefix(trimmed, "data:"); ok {
				data := strings.TrimSpace(payload)
				if data != "" && data != "[DONE]" {
					line = "data: " + string(normalizeCacheUsageJSON([]byte(data)))
				}
			}
			if _, err := io.WriteString(writer, line+"\n"); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return reader
}

func normalizeCacheUsageJSON(payload []byte) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil {
		return payload
	}
	usagePayload, exists := root["usage"]
	if !exists {
		return payload
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(usagePayload, &usage) != nil {
		return payload
	}
	hitPayload, exists := usage["prompt_cache_hit_tokens"]
	if !exists {
		return payload
	}
	var hitTokens int
	if json.Unmarshal(hitPayload, &hitTokens) != nil || hitTokens < 0 {
		return payload
	}
	details := make(map[string]json.RawMessage)
	if current, ok := usage["prompt_tokens_details"]; ok {
		_ = json.Unmarshal(current, &details)
	}
	if _, exists := details["cached_tokens"]; !exists {
		details["cached_tokens"] = json.RawMessage(fmt.Sprintf("%d", hitTokens))
	}
	encodedDetails, err := json.Marshal(details)
	if err != nil {
		return payload
	}
	usage["prompt_tokens_details"] = encodedDetails
	encodedUsage, err := json.Marshal(usage)
	if err != nil {
		return payload
	}
	root["usage"] = encodedUsage
	encodedRoot, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return encodedRoot
}
