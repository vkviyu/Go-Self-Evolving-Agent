package agentruntime

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"

	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
)

const (
	maxCompactionEntryBytes = 32 << 10
	compactionTailBytes     = 8 << 10
)

type compactionEntry struct {
	text      string
	signature [sha256.Size]byte
}

// compactionTranscript deterministically reduces oversized entries and exact
// repeated loops before invoking the summary model. Durable history remains
// untouched; this only controls the summarizer's input budget.
func compactionTranscript(messages []*schema.Message) ([]string, error) {
	entries := make([]compactionEntry, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			continue
		}
		content := message.Content
		signatureContent := content
		if len(message.ToolCalls) > 0 {
			encoded, err := json.Marshal(message.ToolCalls)
			if err != nil {
				return nil, fmt.Errorf("encode tool calls for compaction: %w", err)
			}
			content += " tool_calls=" + string(encoded)
			normalized := make([]schema.FunctionCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				normalized = append(normalized, call.Function)
			}
			normalizedJSON, err := json.Marshal(normalized)
			if err != nil {
				return nil, fmt.Errorf("normalize tool calls for compaction: %w", err)
			}
			signatureContent += " tool_calls=" + string(normalizedJSON)
		}
		fullText := string(message.Role) + ": " + content
		signature := sha256.Sum256([]byte(string(message.Role) + "\x00" + signatureContent))
		entries = append(entries, compactionEntry{text: reduceCompactionEntry(fullText), signature: signature})
	}
	return collapseRepeatedEntries(entries), nil
}

func reduceCompactionEntry(content string) string {
	if len(content) <= maxCompactionEntryBytes {
		return content
	}
	headBytes := maxCompactionEntryBytes - compactionTailBytes
	headEnd := utf8PrefixBoundary(content, headBytes)
	tailStart := utf8SuffixBoundary(content, len(content)-compactionTailBytes)
	omitted := tailStart - headEnd
	return content[:headEnd] + fmt.Sprintf("\n[compaction omitted %d bytes from this entry; full content remains in durable history]\n", omitted) + content[tailStart:]
}

func utf8PrefixBoundary(content string, end int) int {
	if end >= len(content) {
		return len(content)
	}
	for end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	return end
}

func utf8SuffixBoundary(content string, start int) int {
	if start <= 0 {
		return 0
	}
	for start < len(content) && !utf8.RuneStart(content[start]) {
		start++
	}
	return start
}

func collapseRepeatedEntries(entries []compactionEntry) []string {
	result := make([]string, 0, len(entries))
	for index := 0; index < len(entries); {
		if repeats := repeatedBlockCount(entries, index, 2); repeats > 1 {
			result = append(result, entries[index].text, entries[index+1].text)
			result = append(result, fmt.Sprintf("[compaction collapsed the preceding 2-message pattern; it occurred %d times]", repeats))
			index += repeats * 2
			continue
		}
		if repeats := repeatedBlockCount(entries, index, 1); repeats > 1 {
			result = append(result, entries[index].text)
			result = append(result, fmt.Sprintf("[compaction collapsed the preceding message; it occurred %d times]", repeats))
			index += repeats
			continue
		}
		result = append(result, entries[index].text)
		index++
	}
	return result
}

func repeatedBlockCount(entries []compactionEntry, start, blockSize int) int {
	if blockSize <= 0 || start+2*blockSize > len(entries) {
		return 1
	}
	repeats := 1
	for next := start + blockSize; next+blockSize <= len(entries); next += blockSize {
		matches := true
		for offset := range blockSize {
			if entries[start+offset].signature != entries[next+offset].signature {
				matches = false
				break
			}
		}
		if !matches {
			break
		}
		repeats++
	}
	return repeats
}

func latestMessageCountWithCounter(counter tokenizer.Counter, messages []*schema.Message, targetTokens int) int {
	used := 0
	count := 0
	for index := range slices.Backward(messages) {
		next := tokenizer.CountLocalMessages(counter, messages[index:index+1])
		if count > 0 && used+next > targetTokens {
			break
		}
		used += next
		count++
		if used >= targetTokens {
			break
		}
	}
	// A tool result cannot survive without its originating assistant call.
	// Expand the suffix to include the whole tool-call/result message group.
	for start := len(messages) - count; start > 0 && messages[start].Role == schema.Tool; start = len(messages) - count {
		count++
	}
	return count
}
