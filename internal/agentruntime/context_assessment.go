package agentruntime

import (
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// requestAssessment retains only the latest effective request, not Eino's
// uncompressed checkpoint. Its private copies cannot be mutated by Eino IDs.
type requestAssessment struct {
	mu     sync.Mutex
	input  []*schema.Message
	tools  []contextToolSchema
	output int
}

func (m *recordingModel) captureRequest(input []*schema.Message, options []model.Option) error {
	if m.factory == nil || m.factory.tokenizer == nil || m.purpose != "" {
		return nil
	}
	stable := make([]*schema.Message, len(input))
	for i, message := range input {
		if message == nil {
			continue
		}
		clone, err := cloneMessage(message)
		if err != nil {
			return fmt.Errorf("capture context assessment: %w", err)
		}
		stable[i] = clone
	}
	m.assessment.mu.Lock()
	defer m.assessment.mu.Unlock()
	m.assessment.input, m.assessment.tools = stable, requestToolSchemas(options)
	m.assessment.output = requestOutputLimit(m.factory, options)
	return nil
}

// completedContext performs local accounting only. Crossing the soft or hard
// input line after an answer does not invalidate that successful answer; it
// requires preflight management before the next model dispatch.
func (m *recordingModel) completedContext(final *schema.Message) *ContextUsage {
	if m == nil || m.factory == nil || m.factory.tokenizer == nil {
		return nil
	}
	m.assessment.mu.Lock()
	defer m.assessment.mu.Unlock()
	if len(m.assessment.input) == 0 {
		return nil
	}
	input := durableContextMessages(append(append([]*schema.Message(nil), m.assessment.input...), final))
	usage := m.factory.contextUsage(input, m.assessment.tools, m.assessment.output)
	return &usage
}
