package tokenizer

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestUnknownModelIsExplicitlyEstimated(t *testing.T) {
	counter := NewForModel("openai", "gpt-5.6-sol")
	if counter.Exact() || counter.Status() != StatusEstimated {
		t.Fatalf("status = %q exact=%v", counter.Status(), counter.Exact())
	}
	if got := counter.CountMessages([]*schema.Message{schema.UserMessage("hello 世界")}); got <= 0 {
		t.Fatalf("estimated token count = %d", got)
	}
}

func TestProviderUsageCalibratesUnknownModel(t *testing.T) {
	counter := NewForModel("ollama", "llama3")
	estimated := counter.CountMessages([]*schema.Message{schema.UserMessage("hello")})
	counter.ObserveProviderInput(42, estimated)
	if counter.Status() != StatusProvider {
		t.Fatalf("status = %q, want provider", counter.Status())
	}
	if got := counter.CountMessages([]*schema.Message{schema.UserMessage("hello")}); got != 42 {
		t.Fatalf("calibrated count = %d, want 42", got)
	}
	secondEstimate := counter.CountMessages([]*schema.Message{schema.UserMessage("hello")})
	counter.ObserveProviderInput(42, secondEstimate)
	if got := counter.CountMessages([]*schema.Message{schema.UserMessage("hello")}); got != 42 {
		t.Fatalf("recalibrated count = %d, want 42", got)
	}
}

func TestExplicitEncodingIsReported(t *testing.T) {
	counter := NewForModelWithEncoding("openai", "private-model", "cl100k_base")
	if counter.Encoding() != "cl100k_base" {
		t.Fatalf("encoding = %q", counter.Encoding())
	}
	if counter.Status() != StatusUnavailable && counter.Status() != StatusExact {
		t.Fatalf("status = %q", counter.Status())
	}
}

func TestForkStartsFromCurrentCalibrationAndDivergesIndependently(t *testing.T) {
	messages := []*schema.Message{schema.UserMessage("hello")}
	parent := NewForModel("ollama", "llama3")
	parent.ObserveProviderInput(42, parent.CountMessages(messages))
	child := Fork(parent)
	if child == parent || child.CountMessages(messages) != 42 || child.Status() != StatusProvider {
		t.Fatalf("fork did not copy calibration: count=%d status=%q", child.CountMessages(messages), child.Status())
	}
	child.ObserveProviderInput(100, child.CountMessages(messages))
	if got := parent.CountMessages(messages); got != 42 {
		t.Fatalf("child observation changed the parent count to %d", got)
	}
	if got := child.CountMessages(messages); got != 100 {
		t.Fatalf("child count = %d, want 100", got)
	}
}
