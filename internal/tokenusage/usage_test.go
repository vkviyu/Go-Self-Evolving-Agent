package tokenusage

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestFromMessageAndCacheHitPercent(t *testing.T) {
	message := schema.AssistantMessage("done", nil)
	message.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
		PromptTokens: 8,
		PromptTokenDetails: schema.PromptTokenDetails{
			CachedTokens: 6,
		},
		CompletionTokens: 3,
		TotalTokens:      11,
		CompletionTokensDetails: schema.CompletionTokensDetails{
			ReasoningTokens: 2,
		},
	}}

	usage := FromMessage(message)
	if usage.Input != 8 || usage.Cached != 6 || usage.Output != 3 || usage.Reasoning != 2 || usage.Total != 11 {
		t.Fatalf("usage = %#v", usage)
	}
	if got := usage.CacheHitPercent(); got != 75 {
		t.Fatalf("cache hit percent = %v, want 75", got)
	}
}

func TestCacheHitPercentHandlesMissingAndInvalidUsage(t *testing.T) {
	if got := (Snapshot{}).CacheHitPercent(); got != 0 {
		t.Fatalf("empty cache hit percent = %v", got)
	}
	if got := (Snapshot{Input: 5, Cached: 7}).CacheHitPercent(); got != 100 {
		t.Fatalf("clamped cache hit percent = %v", got)
	}
}
