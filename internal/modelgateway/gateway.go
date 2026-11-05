// Package modelgateway constructs Eino chat models from provider-neutral
// configuration. Credentials are resolved inside this boundary and are never
// returned to callers or written to session events.
package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/ollama"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

// Provider identifies a concrete Eino model adapter.
type Provider string

const (
	ProviderOpenAI Provider = "openai"
	ProviderOllama Provider = "ollama"
)

// WireAPI identifies the HTTP protocol used by a model provider.
type WireAPI string

const (
	WireAPIChatCompletions WireAPI = "chat-completions"
	WireAPIResponses       WireAPI = "responses"
)

const (
	defaultOllamaBaseURL = "http://127.0.0.1:11434"
	defaultTimeout       = 2 * time.Minute
)

// Config intentionally contains only the environment variable name for a
// credential, never the credential value itself.
type Config struct {
	Provider          Provider
	WireAPI           WireAPI
	Model             string
	TokenizerEncoding string
	BaseURL           string
	APIKeyEnvironment string
	ReasoningEffort   string
	Timeout           time.Duration
	HTTPClient        *http.Client
}

// NewTokenizer resolves the local model tokenizer without requiring provider
// credentials. The returned counter reports whether it is exact or calibrated
// from provider usage, so callers can expose that distinction to users.
func NewTokenizer(config Config) tokenizer.Counter {
	return tokenizer.NewForModelWithEncoding(string(config.Provider), config.Model, config.TokenizerEncoding)
}

// New resolves credentials from the process environment and constructs an
// immutable Eino ToolCallingChatModel.
func New(ctx context.Context, config Config) (model.ToolCallingChatModel, error) {
	chatModel, err := newWithLookup(ctx, config, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return &contextAwareModel{ToolCallingChatModel: chatModel, metadata: discoverContext(ctx, config)}, nil
}

type environmentLookup func(string) (string, bool)

func newWithLookup(ctx context.Context, config Config, lookup environmentLookup) (model.ToolCallingChatModel, error) {
	config.Provider = Provider(strings.ToLower(strings.TrimSpace(string(config.Provider))))
	config.WireAPI = WireAPI(strings.ToLower(strings.TrimSpace(string(config.WireAPI))))
	config.Model = strings.TrimSpace(config.Model)
	config.BaseURL = strings.TrimSpace(config.BaseURL)
	config.APIKeyEnvironment = strings.TrimSpace(config.APIKeyEnvironment)
	if config.Model == "" {
		return nil, errors.New("model name is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}

	switch config.Provider {
	case ProviderOpenAI:
		if config.WireAPI == "" {
			config.WireAPI = WireAPIChatCompletions
		}
		if config.APIKeyEnvironment == "" {
			config.APIKeyEnvironment = "OPENAI_API_KEY"
		}
		apiKey, exists := lookup(config.APIKeyEnvironment)
		if !exists || strings.TrimSpace(apiKey) == "" {
			return nil, fmt.Errorf("OpenAI credential environment variable %s is not set", config.APIKeyEnvironment)
		}
		if err := validateBaseURL(config.BaseURL, true); err != nil {
			return nil, err
		}
		switch config.WireAPI {
		case WireAPIChatCompletions:
			httpClient := cacheUsageHTTPClient(config.HTTPClient, config.Timeout)
			chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
				APIKey:     apiKey,
				Model:      config.Model,
				BaseURL:    config.BaseURL,
				Timeout:    config.Timeout,
				HTTPClient: httpClient,
			})
			if err != nil {
				return nil, fmt.Errorf("construct OpenAI-compatible model: %w", err)
			}
			return chatModel, nil
		case WireAPIResponses:
			responsesModel, err := newResponsesModel(responsesModelConfig{
				APIKey:          apiKey,
				Model:           config.Model,
				BaseURL:         config.BaseURL,
				ReasoningEffort: config.ReasoningEffort,
				Timeout:         config.Timeout,
				HTTPClient:      config.HTTPClient,
			})
			if err != nil {
				return nil, fmt.Errorf("construct OpenAI Responses model: %w", err)
			}
			return responsesModel, nil
		default:
			return nil, fmt.Errorf("unsupported OpenAI wire API %q (supported: %s, %s)", config.WireAPI, WireAPIChatCompletions, WireAPIResponses)
		}

	case ProviderOllama:
		if config.BaseURL == "" {
			config.BaseURL = defaultOllamaBaseURL
		}
		if err := validateBaseURL(config.BaseURL, false); err != nil {
			return nil, err
		}
		chatModel, err := ollama.NewChatModel(ctx, &ollama.ChatModelConfig{
			Model:      config.Model,
			BaseURL:    config.BaseURL,
			Timeout:    config.Timeout,
			HTTPClient: config.HTTPClient,
		})
		if err != nil {
			return nil, fmt.Errorf("construct Ollama model: %w", err)
		}
		return chatModel, nil

	default:
		return nil, fmt.Errorf("unsupported model provider %q (supported: %s, %s)", config.Provider, ProviderOpenAI, ProviderOllama)
	}
}

func validateBaseURL(value string, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid model base URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("model base URL must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return errors.New("model base URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("model base URL must not contain a query or fragment")
	}
	return nil
}
