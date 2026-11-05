package modelgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
)

type contextAwareModel struct {
	model.ToolCallingChatModel
	metadata modelcontext.Metadata
}

func (m *contextAwareModel) ModelContext() modelcontext.Metadata { return m.metadata }

func (m *contextAwareModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	bound, err := m.ToolCallingChatModel.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &contextAwareModel{ToolCallingChatModel: bound, metadata: m.metadata}, nil
}

// Discovery is one bounded metadata request per model construction, never a
// generation probe. Absent or invalid deployment metadata retains the catalog
// or explicitly labelled fallback, separately for each capacity field.
func discoverContext(ctx context.Context, config Config) modelcontext.Metadata {
	config.Provider = Provider(strings.ToLower(strings.TrimSpace(string(config.Provider))))
	config.Model, config.BaseURL = strings.TrimSpace(config.Model), strings.TrimSpace(config.BaseURL)
	metadata := modelcontext.Lookup(string(config.Provider), config.Model)
	if config.Provider == ProviderOllama {
		return discoverOllamaContext(ctx, config, metadata)
	}
	if config.Provider != ProviderOpenAI || config.BaseURL == "" {
		return metadata
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Hostname() == "api.openai.com" {
		return metadata
	}
	base.Path = strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/responses") + "/models"
	escapedPrefix := base.EscapedPath()
	base.Path += "/" + config.Model
	base.RawPath = escapedPrefix + "/" + url.PathEscape(config.Model)
	keyEnv := config.APIKeyEnvironment
	if keyEnv == "" {
		keyEnv = "OPENAI_API_KEY"
	}
	var info struct {
		ID              string `json:"id"`
		ContextWindow   int    `json:"context_window"`
		ContextLength   int    `json:"context_length"`
		MaxModelLen     int    `json:"max_model_len"`
		MaxOutputTokens int    `json:"max_output_tokens"`
	}
	if !readContextMetadata(ctx, config, base.String(), "Bearer "+os.Getenv(keyEnv), &info) || (info.ID != "" && info.ID != config.Model) {
		return metadata
	}
	window := 0
	for _, value := range []int{info.ContextWindow, info.ContextLength, info.MaxModelLen} {
		if value > 1 && (window == 0 || value < window) {
			window = value
		}
	}
	if window > 1 {
		metadata.Window, metadata.Source = window, modelcontext.SourceProvider
	}
	// max_tokens is deliberately excluded: gateways use it for different
	// meanings. A separately declared output cap must never become a window.
	if info.MaxOutputTokens > 0 {
		metadata.MaxOutputTokens, metadata.OutputSource = info.MaxOutputTokens, modelcontext.SourceProvider
	}
	return metadata
}

// /api/ps exposes the context allocated to a running deployment. Architecture
// metadata from /api/show is not the configured runtime context and cannot be
// used as that deployment's hard limit. An unloaded/older server falls back.
// https://docs.ollama.com/api/ps
func discoverOllamaContext(ctx context.Context, config Config, metadata modelcontext.Metadata) modelcontext.Metadata {
	if config.BaseURL == "" {
		config.BaseURL = defaultOllamaBaseURL
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil {
		return metadata
	}
	base.Path, base.RawPath = strings.TrimRight(base.Path, "/")+"/api/ps", ""
	var info struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if !readContextMetadata(ctx, config, base.String(), "", &info) {
		return metadata
	}
	window := 0
	for _, loaded := range info.Models {
		if loaded.ContextLength <= 1 || (ollamaModelName(loaded.Name) != ollamaModelName(config.Model) && ollamaModelName(loaded.Model) != ollamaModelName(config.Model)) {
			continue
		}
		if window == 0 || loaded.ContextLength < window {
			window = loaded.ContextLength
		}
	}
	if window > 1 {
		metadata.Window, metadata.Source = window, modelcontext.SourceProvider
	}
	return metadata
}

func ollamaModelName(name string) string {
	if name != "" && strings.LastIndex(name, ":") < strings.LastIndex(name, "/")+1 {
		return name + ":latest"
	}
	return name
}

func readContextMetadata(ctx context.Context, config Config, endpoint, authorization string, target any) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(target) == nil
}
