package modelgateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
)

func TestGatewayContextMetadataAndFallback(t *testing.T) {
	for _, test := range []struct {
		name, response string
		status         int
		window         int
		source         modelcontext.Source
		output         int
		outputSource   modelcontext.Source
	}{
		{"smaller deployment", `{"id":"gpt-5.6-sol","context_window":131072}`, 200, 131072, modelcontext.SourceProvider, 128000, modelcontext.SourceCatalog},
		{"compatible extension", `{"context_length":64000,"max_model_len":32000}`, 200, 32000, modelcontext.SourceProvider, 128000, modelcontext.SourceCatalog},
		{"standard response", `{"id":"gpt-5.6-sol","object":"model"}`, 200, 1050000, modelcontext.SourceCatalog, 128000, modelcontext.SourceCatalog},
		{"output is not context", `{"max_tokens":4096,"max_output_tokens":4096}`, 200, 1050000, modelcontext.SourceCatalog, 4096, modelcontext.SourceProvider},
		{"both independent caps", `{"context_window":131072,"max_output_tokens":16384}`, 200, 131072, modelcontext.SourceProvider, 16384, modelcontext.SourceProvider},
		{"explicit provider output above catalog", `{"max_output_tokens":256000}`, 200, 1050000, modelcontext.SourceCatalog, 256000, modelcontext.SourceProvider},
		{"ambiguous max tokens excluded", `{"max_tokens":4096}`, 200, 1050000, modelcontext.SourceCatalog, 128000, modelcontext.SourceCatalog},
		{"missing endpoint", `{}`, 404, 1050000, modelcontext.SourceCatalog, 128000, modelcontext.SourceCatalog},
		{"wrong model", `{"id":"other-model","context_window":4096,"max_output_tokens":32}`, 200, 1050000, modelcontext.SourceCatalog, 128000, modelcontext.SourceCatalog},
		{"invalid metadata", `{"context_window":-1,"max_output_tokens":-1}`, 200, 1050000, modelcontext.SourceCatalog, 128000, modelcontext.SourceCatalog},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GSEA_TEST_CONTEXT_KEY", "local-test-only")
			client := contextMetadataHTTPClient(t, "/v1/models/gpt-5.6-sol", "Bearer local-test-only", test.status, test.response)
			chat, err := New(context.Background(), Config{Provider: ProviderOpenAI, WireAPI: WireAPIResponses, Model: "gpt-5.6-sol", BaseURL: "https://model.test/v1", APIKeyEnvironment: "GSEA_TEST_CONTEXT_KEY", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			bound, err := chat.WithTools(nil)
			if err != nil {
				t.Fatal(err)
			}
			got := bound.(modelcontext.Informer).ModelContext()
			if got.Window != test.window || got.Source != test.source || got.MaxOutputTokens != test.output || got.OutputSource != test.outputSource {
				t.Fatalf("metadata=%+v want window=%d source=%s output=%d output-source=%s", got, test.window, test.source, test.output, test.outputSource)
			}
		})
	}
}

func TestUnknownGatewayCanDeclareOnlyOutputCapacity(t *testing.T) {
	t.Setenv("GSEA_TEST_CONTEXT_KEY", "local-test-only")
	client := contextMetadataHTTPClient(t, "/v1/models/private-model", "Bearer local-test-only", 200, `{"id":"private-model","max_output_tokens":1024}`)
	metadata := discoverContext(context.Background(), Config{Provider: ProviderOpenAI, Model: "private-model", BaseURL: "https://model.test/v1", APIKeyEnvironment: "GSEA_TEST_CONTEXT_KEY", HTTPClient: client})
	if metadata.Window != modelcontext.FallbackWindow || metadata.Source != modelcontext.SourceFallback || metadata.MaxOutputTokens != 1024 || metadata.OutputSource != modelcontext.SourceProvider {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestOllamaUsesOnlyMatchingLoadedDeploymentContext(t *testing.T) {
	for _, test := range []struct {
		name, model, response string
		window                int
		source                modelcontext.Source
	}{
		{"loaded model", "local-model:4b", `{"models":[{"model":"local-model:4b","context_length":8192}]}`, 8192, modelcontext.SourceProvider},
		{"implicit latest", "local-model", `{"models":[{"name":"local-model:latest","context_length":16384}]}`, 16384, modelcontext.SourceProvider},
		{"namespace latest", "team/local-model", `{"models":[{"model":"team/local-model:latest","context_length":12000}]}`, 12000, modelcontext.SourceProvider},
		{"registry namespace latest", "registry:5000/team/local-model", `{"models":[{"model":"registry:5000/team/local-model:latest","context_length":12000}]}`, 12000, modelcontext.SourceProvider},
		{"different tag", "local-model:4b", `{"models":[{"model":"local-model:8b","context_length":131072}]}`, modelcontext.FallbackWindow, modelcontext.SourceFallback},
		{"unloaded", "local-model", `{"models":[]}`, modelcontext.FallbackWindow, modelcontext.SourceFallback},
		{"architecture context excluded", "local-model", `{"model_info":{"llama.context_length":131072}}`, modelcontext.FallbackWindow, modelcontext.SourceFallback},
		{"old server missing context", "local-model", `{"models":[{"model":"local-model:latest"}]}`, modelcontext.FallbackWindow, modelcontext.SourceFallback},
		{"invalid context", "local-model", `{"models":[{"model":"local-model:latest","context_length":-1}]}`, modelcontext.FallbackWindow, modelcontext.SourceFallback},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := contextMetadataHTTPClient(t, "/proxy/api/ps", "", 200, test.response)
			chat, err := New(context.Background(), Config{Provider: ProviderOllama, Model: test.model, BaseURL: "https://model.test/proxy", HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			metadata := chat.(modelcontext.Informer).ModelContext()
			if metadata.Window != test.window || metadata.Source != test.source || metadata.MaxOutputTokens != 0 || metadata.OutputSource != "" {
				t.Fatalf("metadata=%+v", metadata)
			}
		})
	}
}

func contextMetadataHTTPClient(t *testing.T, path, authorization string, status int, response string) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != path || request.Header.Get("Authorization") != authorization {
			t.Errorf("unexpected metadata request: %s %s", request.Method, request.URL.Path)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
}
