package modelgateway

import "testing"

func TestResponsesUsageAcceptsProviderCacheFieldVariants(t *testing.T) {
	for _, test := range []struct {
		name   string
		usage  responsesResponseTokenUsage
		cached int
	}{
		{"standard responses", responsesResponseTokenUsage{
			InputTokens:       10,
			InputTokenDetails: responsesInputTokenDetails{CachedTokens: 6},
		}, 6},
		{"openai-compatible prompt details", responsesResponseTokenUsage{
			InputTokens:        10,
			PromptTokenDetails: responsesInputTokenDetails{CachedTokens: 5},
		}, 5},
		{"provider cache hit field", responsesResponseTokenUsage{
			InputTokens: 10, PromptCacheHit: 4,
		}, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted := test.usage.toSchemaTokenUsage()
			if converted.PromptTokenDetails.CachedTokens != test.cached {
				t.Fatalf("cached tokens = %d, want %d", converted.PromptTokenDetails.CachedTokens, test.cached)
			}
		})
	}
}
