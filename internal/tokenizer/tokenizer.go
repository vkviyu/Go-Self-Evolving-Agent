// Package tokenizer provides model-aware context accounting.
//
// A tokenizer is deliberately a separate dependency boundary. Model names are
// not enough to infer an encoding for every provider, so the package exposes
// the accounting quality instead of silently calling an approximation exact.
package tokenizer

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/pkoukk/tiktoken-go"
)

// Status describes how a token count was obtained.
type Status string

const (
	StatusExact       Status = "exact"
	StatusProvider    Status = "provider"
	StatusEstimated   Status = "estimated"
	StatusUnavailable Status = "unavailable"
)

// Counter counts the model input represented by Eino messages.
type Counter interface {
	Model() string
	Encoding() string
	Status() Status
	Exact() bool
	// CountMessages returns the counter's input token count for the supplied message
	// sequence.
	CountMessages(messages []*schema.Message) int
	// ObserveProviderInput records the provider's measured input alongside the estimate
	// used for that request.
	ObserveProviderInput(actual int, estimated int)
}

// NewForModel chooses the best local tokenizer for a provider/model pair.
// Unknown model aliases intentionally use the calibrated estimator until the
// provider returns an authoritative input token count.
func NewForModel(provider, model string) Counter {
	return NewForModelWithEncoding(provider, model, "")
}

// NewForModelWithEncoding is the explicit override for compatible gateways
// whose public model alias does not reveal the tokenizer (for example a
// private deployment that serves an OpenAI-compatible API).
func NewForModelWithEncoding(provider, model, override string) Counter {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	encoding := strings.TrimSpace(override)
	if encoding == "" {
		encoding = encodingForModel(provider, model)
	}
	return &modelCounter{
		provider: provider,
		model:    model,
		encoding: encoding,
		status:   statusForEncoding(encoding),
	}
}

// NewApproximate is useful for tests and providers with no published
// tokenizer. It remains explicitly marked estimated.
func NewApproximate(model string) Counter {
	return &modelCounter{model: strings.TrimSpace(model), status: StatusEstimated}
}

type modelCounter struct {
	provider string
	model    string
	encoding string

	mutex          sync.RWMutex
	status         Status
	encodingModel  *tiktoken.Tiktoken
	loadAttempted  bool
	providerOffset int
}

func (counter *modelCounter) Model() string { return counter.model }

func (counter *modelCounter) Encoding() string { return counter.encoding }

func (counter *modelCounter) Status() Status {
	counter.mutex.RLock()
	defer counter.mutex.RUnlock()
	return counter.status
}

func (counter *modelCounter) Exact() bool { return counter.Status() == StatusExact }

// CountMessages counts message input with the model's available tokenization and
// calibration information.
func (counter *modelCounter) CountMessages(messages []*schema.Message) int {
	count := counter.CountLocalMessages(messages)
	counter.mutex.RLock()
	offset := counter.providerOffset
	counter.mutex.RUnlock()
	if count+offset < 0 {
		return 0
	}
	return count + offset
}

func (counter *modelCounter) CountLocalMessages(messages []*schema.Message) int {
	local := counter.localTokenizer()
	if local != nil {
		return countWithEncoding(local, messages)
	}
	return estimateMessages(messages)
}

// CountLocalMessages is for additive selection of a retained suffix. Provider
// calibration applies once per complete request; applying it to each message
// would multiply overhead (or make small messages appear free).
func CountLocalMessages(counter Counter, messages []*schema.Message) int {
	if local, ok := counter.(interface{ CountLocalMessages([]*schema.Message) int }); ok {
		return local.CountLocalMessages(messages)
	}
	return counter.CountMessages(messages)
}

// Fork returns a counter that starts from counter's current calibration and
// then learns only from its own observations, so Agents that share a model do
// not skew each other's context estimates. Counters without independent state
// are returned unchanged.
func Fork(counter Counter) Counter {
	if forkable, ok := counter.(interface{ Fork() Counter }); ok {
		return forkable.Fork()
	}
	return counter
}

// Fork shares the immutable encoding tables and copies the calibration.
func (counter *modelCounter) Fork() Counter {
	counter.mutex.RLock()
	defer counter.mutex.RUnlock()
	return &modelCounter{
		provider: counter.provider, model: counter.model, encoding: counter.encoding,
		status: counter.status, encodingModel: counter.encodingModel, loadAttempted: counter.loadAttempted,
		providerOffset: counter.providerOffset,
	}
}

// ObserveProviderInput uses a provider-reported input count to calibrate subsequent local
// estimates.
func (counter *modelCounter) ObserveProviderInput(actual int, estimated int) {
	if actual <= 0 || estimated < 0 {
		return
	}
	counter.mutex.Lock()
	counter.providerOffset += actual - estimated
	counter.status = StatusProvider
	counter.mutex.Unlock()
}

func (counter *modelCounter) localTokenizer() *tiktoken.Tiktoken {
	counter.mutex.RLock()
	if counter.encodingModel != nil {
		encoding := counter.encodingModel
		counter.mutex.RUnlock()
		return encoding
	}
	if counter.loadAttempted {
		counter.mutex.RUnlock()
		return nil
	}
	counter.mutex.RUnlock()

	counter.mutex.Lock()
	defer counter.mutex.Unlock()
	if counter.loadAttempted {
		return counter.encodingModel
	}
	counter.loadAttempted = true
	if counter.encoding == "" {
		counter.setLocalStatus(StatusEstimated)
		return nil
	}
	if !cachedBPE(counter.encoding) {
		counter.setLocalStatus(StatusUnavailable)
		return nil
	}
	encoding, err := tiktoken.GetEncoding(counter.encoding)
	if err != nil {
		counter.setLocalStatus(StatusUnavailable)
		return nil
	}
	counter.encodingModel = encoding
	counter.setLocalStatus(StatusExact)
	return encoding
}

// Called with mutex held. A provider observation remains authoritative when
// lazy initialization later discovers a local encoding or a missing cache.
func (counter *modelCounter) setLocalStatus(status Status) {
	if counter.status != StatusProvider {
		counter.status = status
	}
}

// cachedBPE prevents tiktoken-go's default loader from issuing an unbounded
// network request during a model call. Users can provision the standard cache
// ahead of time with TIKTOKEN_CACHE_DIR; a missing file remains explicit as an
// unavailable local tokenizer and falls back to provider calibration.
func cachedBPE(encoding string) bool {
	urls := map[string]string{
		"cl100k_base": "https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken",
		"p50k_base":   "https://openaipublic.blob.core.windows.net/encodings/p50k_base.tiktoken",
		"r50k_base":   "https://openaipublic.blob.core.windows.net/encodings/r50k_base.tiktoken",
	}
	url, ok := urls[encoding]
	if !ok {
		return false
	}
	cacheDir := strings.TrimSpace(os.Getenv("TIKTOKEN_CACHE_DIR"))
	if cacheDir == "" {
		cacheDir = strings.TrimSpace(os.Getenv("DATA_GYM_CACHE_DIR"))
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "data-gym-cache")
	}
	_, err := os.Stat(filepath.Join(cacheDir, fmt.Sprintf("%x", sha1.Sum([]byte(url)))))
	return err == nil
}

func countWithEncoding(encoding *tiktoken.Tiktoken, messages []*schema.Message) int {
	// This is the OpenAI chat message framing used by the compatible adapters.
	// Responses requests use the same textual fields plus provider framing; the
	// provider observation offset accounts for that framing and tool schemas.
	count := 3
	for _, message := range messages {
		if message == nil {
			continue
		}
		count += 3
		count += len(encoding.EncodeOrdinary(string(message.Role)))
		count += len(encoding.EncodeOrdinary(message.Name))
		count += len(encoding.EncodeOrdinary(message.Content))
		count += len(encoding.EncodeOrdinary(message.ReasoningContent))
		count += len(encoding.EncodeOrdinary(message.ToolCallID))
		count += len(encoding.EncodeOrdinary(message.ToolName))
		for _, call := range message.ToolCalls {
			count += len(encoding.EncodeOrdinary(call.ID))
			count += len(encoding.EncodeOrdinary(call.Type))
			count += len(encoding.EncodeOrdinary(call.Function.Name))
			count += len(encoding.EncodeOrdinary(call.Function.Arguments))
			if len(call.Extra) > 0 {
				if encoded, err := json.Marshal(call.Extra); err == nil {
					count += len(encoding.EncodeOrdinary(string(encoded)))
				}
			}
		}
		count += countParts(encoding, message)
	}
	return count
}

func countParts(encoding *tiktoken.Tiktoken, message *schema.Message) int {
	count := 0
	for _, part := range message.UserInputMultiContent {
		count += len(encoding.EncodeOrdinary(part.Text))
		if part.Image != nil {
			if part.Image.URL != nil {
				count += len(encoding.EncodeOrdinary(*part.Image.URL))
			}
			count += len(encoding.EncodeOrdinary(part.Image.MIMEType))
		}
	}
	for _, part := range message.AssistantGenMultiContent {
		count += len(encoding.EncodeOrdinary(part.Text))
	}
	return count
}

func estimateMessages(messages []*schema.Message) int {
	characters := 0
	for _, message := range messages {
		if message == nil {
			continue
		}
		characters += len([]rune(message.Content))
		characters += len([]rune(message.ReasoningContent))
		characters += len([]rune(message.Name)) + len([]rune(message.ToolName)) + len([]rune(message.ToolCallID))
		for _, call := range message.ToolCalls {
			characters += len([]rune(call.ID)) + len([]rune(call.Type)) + len([]rune(call.Function.Name)) + len([]rune(call.Function.Arguments))
		}
	}
	if characters == 0 {
		return 0
	}
	return (characters + 3) / 4
}

func encodingForModel(provider, model string) string {
	if provider != "openai" {
		return ""
	}
	lower := strings.ToLower(model)
	// These aliases are backed by the encodings shipped by tiktoken-go. Newer
	// o200k/GPT-5 aliases intentionally remain provider-calibrated because
	// mapping them to cl100k would produce a false exactness claim.
	switch {
	case lower == "gpt-4" || strings.HasPrefix(lower, "gpt-4-"):
		return "cl100k_base"
	case lower == "gpt-3.5-turbo" || strings.HasPrefix(lower, "gpt-3.5-turbo-"):
		return "cl100k_base"
	case lower == "text-davinci-001":
		return "r50k_base"
	case lower == "text-davinci-002" || lower == "text-davinci-003" || lower == "code-davinci-001" || lower == "code-davinci-002" || lower == "davinci-codex" || lower == "cushman-codex":
		return "p50k_base"
	default:
		return ""
	}
}

func statusForEncoding(encoding string) Status {
	if encoding == "" {
		return StatusEstimated
	}
	return StatusUnavailable
}
