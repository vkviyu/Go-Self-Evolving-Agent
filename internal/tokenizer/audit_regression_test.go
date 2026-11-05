package tokenizer

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestModernCodexAliasesDoNotClaimLegacyEncoding(t *testing.T) {
	for _, name := range []string{"gpt-5-codex", "gpt-5.3-codex", "codex-mini-latest", "private-codex"} {
		counter := NewForModel("openai", name)
		if counter.Encoding() != "" || counter.Exact() {
			t.Fatalf("%s: false exact legacy encoding %q", name, counter.Encoding())
		}
	}
	if got := NewForModel("openai", "code-davinci-002").Encoding(); got != "p50k_base" {
		t.Fatalf("legacy encoding = %q", got)
	}
	if got := NewForModel("openai", "text-davinci-001").Encoding(); got != "r50k_base" {
		t.Fatalf("legacy r50k encoding = %q", got)
	}
}

func TestLazyLocalInitializationPreservesProviderStatus(t *testing.T) {
	t.Setenv("TIKTOKEN_CACHE_DIR", t.TempDir())
	for _, name := range []string{"gpt-4", "private-model"} {
		counter := NewForModel("openai", name)
		counter.ObserveProviderInput(17, 3)
		counter.CountMessages([]*schema.Message{schema.UserMessage("hello")})
		if got := counter.Status(); got != StatusProvider {
			t.Fatalf("%s: provider status overwritten by lazy init: %s", name, got)
		}
	}
}
