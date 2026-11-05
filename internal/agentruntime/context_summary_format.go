package agentruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	contextSummaryVersion   = 1
	summarySourceSemantic   = "semantic"
	summarySourceExtractive = "extractive"
	maxSummaryResponseBytes = 64 << 10
)

// contextSummary is a versioned handoff, not a task contract or authorization.
// Keep empty sections explicit: omitted verification is not successful testing.
// Old durable summaries remain plain strings; only newly generated ones use
// this format, so loading history never depends on decoding this structure.
type contextSummary struct {
	Version      int      `json:"version"`
	Source       string   `json:"source"`
	Task         string   `json:"task"`
	Constraints  []string `json:"constraints"`
	Completed    []string `json:"completed"`
	Changes      []string `json:"changes"`
	Verification []string `json:"verification"`
	OpenItems    []string `json:"open_items"`
	NextSteps    []string `json:"next_steps"`
	References   []string `json:"references"`
	Excerpts     []string `json:"excerpts,omitempty"`
}

func emptyContextSummary(source string) contextSummary {
	return contextSummary{
		Version: contextSummaryVersion, Source: source,
		Constraints: []string{}, Completed: []string{}, Changes: []string{},
		Verification: []string{}, OpenItems: []string{}, NextSteps: []string{}, References: []string{},
	}
}

func parseContextSummary(content string) (contextSummary, error) {
	var result contextSummary
	content = strings.TrimSpace(content)
	if len(content) > maxSummaryResponseBytes {
		return result, fmt.Errorf("summary exceeds %d bytes", maxSummaryResponseBytes)
	}
	// A JSON code fence is presentation, not a schema difference. Other prose
	// and trailing objects are rejected rather than silently discarded.
	if strings.HasPrefix(content, "```json\n") && strings.HasSuffix(content, "\n```") {
		content = strings.TrimSuffix(strings.TrimPrefix(content, "```json\n"), "\n```")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("invalid summary JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return result, errors.New("summary must contain exactly one JSON object")
	}
	if result.Version != contextSummaryVersion || result.Source != summarySourceSemantic {
		return result, errors.New("summary requires version=1 and source=semantic")
	}
	if strings.TrimSpace(result.Task) == "" {
		return result, errors.New("summary requires a nonempty task")
	}
	for _, section := range []struct {
		name  string
		items []string
	}{
		{"constraints", result.Constraints}, {"completed", result.Completed}, {"changes", result.Changes},
		{"verification", result.Verification}, {"open_items", result.OpenItems},
		{"next_steps", result.NextSteps}, {"references", result.References},
	} {
		if section.items == nil {
			return result, fmt.Errorf("summary requires %s as an array (empty when unknown)", section.name)
		}
		for _, item := range section.items {
			if strings.TrimSpace(item) == "" {
				return result, fmt.Errorf("summary %s contains an empty entry", section.name)
			}
		}
	}
	if len(result.Excerpts) != 0 {
		return result, errors.New("semantic summary may not claim extractive excerpts")
	}
	return result, nil
}

func encodeContextSummary(summary contextSummary) (string, error) {
	encoded, err := json.Marshal(summary)
	if err != nil {
		return "", fmt.Errorf("encode context summary: %w", err)
	}
	return string(encoded), nil
}

// summaryFormatSource lets event writers identify a deterministic fallback
// before host-generated Goal anchors are added. Unknown legacy text is valid
// history, but has no new-format source marker.
func summaryFormatSource(content string) string {
	var header struct {
		Version int    `json:"version"`
		Source  string `json:"source"`
	}
	if json.Unmarshal([]byte(content), &header) != nil || header.Version != contextSummaryVersion {
		return ""
	}
	if header.Source != summarySourceSemantic && header.Source != summarySourceExtractive {
		return ""
	}
	return header.Source
}
