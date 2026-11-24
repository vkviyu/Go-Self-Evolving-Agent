package agentruntime

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const toolFormatCorrection = "Regenerate the next step. A previous tool call was rejected before execution. To use a tool, make a native function call to one of the advertised tool names with a JSON object matching its parameter schema. Put any shell command inside the argument field defined by that tool. Use one tool call for this step."

func (m *recordingModel) prepareDispatchInput(ctx context.Context, input []*schema.Message, options []model.Option) ([]*schema.Message, []model.Option, bool, error) {
	input, options, err := m.prepareModelInput(ctx, input, options)
	if err != nil || (!m.correctToolFormat && !m.receipt.ToolFormatCorrection) {
		return input, options, false, err
	}
	m.correctToolFormat = false
	// The correction belongs to this dispatch. Keep its marker in the
	// receipt so an interrupted retry reconstructs the same provider input.
	// The base prompt stays separate from the one-time correction.
	input = append([]*schema.Message{schema.SystemMessage(toolFormatCorrection)}, input...)
	options, err = m.profileOptions(ctx, input, options)
	if err == nil {
		err = m.checkDispatchCapacity(input, options)
	}
	if err == nil {
		err = m.captureRequest(input, options)
	}
	if err == nil && !m.receipt.ToolFormatCorrection {
		m.receipt.ToolFormatCorrection = true
		err = m.recordPreparedRequest(ctx)
	}
	return input, options, true, err
}
