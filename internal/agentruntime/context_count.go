package agentruntime

import (
	"encoding/json"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type contextToolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

// countRequestInput counts the request messages together with the tool schemas selected by
// its model options.
func (m *recordingModel) countRequestInput(messages []*schema.Message, options []model.Option) int {
	if m.factory == nil {
		return 0
	}
	return m.factory.countContextInput(messages, requestToolSchemas(options))
}

func requestToolSchemas(options []model.Option) []contextToolSchema {
	tools := model.GetCommonOptions(nil, options...).Tools
	schemas := make([]contextToolSchema, 0, len(tools))
	for _, info := range tools {
		if info == nil {
			continue
		}
		item := contextToolSchema{Name: info.Name, Description: info.Desc}
		if info.ParamsOneOf != nil {
			item.Parameters, _ = info.ParamsOneOf.ToJSONSchema()
		}
		schemas = append(schemas, item)
	}
	return schemas
}

func (f *Factory) countContextInput(messages []*schema.Message, tools []contextToolSchema) int {
	if f.tokenizer == nil {
		return 0
	}
	if len(tools) == 0 {
		return f.tokenizer.CountMessages(messages)
	}
	encoded, _ := json.Marshal(tools) // canonical schemas contain JSON values only
	input := append(append([]*schema.Message(nil), messages...), schema.SystemMessage(string(encoded)))
	// Apply provider calibration once to the complete request, not once per
	// tool/message. Tool schemas and the system instruction consume context too.
	return f.tokenizer.CountMessages(input)
}

func (f *Factory) contextToolSchemas() []contextToolSchema {
	var schemas []contextToolSchema
	for _, descriptor := range f.registry.Snapshot().Descriptors() {
		if f.profile != nil && !permitsProfileTool(*f.profile, descriptor) {
			continue
		}
		schemas = append(schemas, contextToolSchema{Name: descriptor.Name, Description: descriptor.Description, Parameters: descriptor.InputSchema})
	}
	return schemas
}
