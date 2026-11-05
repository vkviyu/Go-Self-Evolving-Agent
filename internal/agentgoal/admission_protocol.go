package agentgoal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

const StartToolName = "goal_start"

const AdmissionInstruction = `Task admission is optional. Ordinary conversation, explanations, diagnosis, and read/search-assisted answers do NOT require Goal Mode. Calling a tool is not a reason to start a goal. For a user-authorized task whose completion requires an outcome beyond merely producing a reply (including exhaustive read-only verification), call goal_start with the objective, authorized scope, and ALL acceptance criteria before executing the task's changes. You may inspect context first. Do not start a goal for incidental logs/caches or turn diagnosis into permission to fix. Resolve ambiguous follow-ups from the conversation; ask if the intended task or authorization is unclear. goal_start does not grant permissions. After it succeeds, retain the contract and follow the returned goal reporting protocol. Never call goal_report without an active goal. A file_content criterion checks exact UTF-8 file content; use semantic criteria for requirements it cannot establish. Do not omit semantic requirements merely to avoid evaluation.`

// Contract states completion responsibility, not authorization. Original user
// input and tool permissions remain authoritative. Criteria are immutable after
// admission so a failed task cannot silently weaken its own acceptance test.
type Contract struct {
	Objective string      `json:"objective"`
	Scope     string      `json:"scope"`
	Criteria  []Criterion `json:"criteria"`
}

type Criterion struct {
	Description string `json:"description"`
	Kind        string `json:"kind"`
	// For semantic criteria these are optional evaluator context, not an
	// exact-content assertion. Only file_content enables mechanical checking.
	Path    string  `json:"path,omitempty"`
	Content *string `json:"content,omitempty"`
}

func (contract Contract) Validate() error {
	if strings.TrimSpace(contract.Objective) == "" || strings.TrimSpace(contract.Scope) == "" || len(contract.Criteria) == 0 || len(contract.Criteria) > 20 {
		return errors.New("task contract requires objective, authorized scope, and 1–20 acceptance criteria")
	}
	encoded, _ := json.Marshal(contract)
	if len(encoded) > 128*1024 {
		return errors.New("task contract exceeds 128 KiB")
	}
	for _, criterion := range contract.Criteria {
		if strings.TrimSpace(criterion.Description) == "" {
			return errors.New("criterion description is required")
		}
		switch criterion.Kind {
		case "semantic":
			if criterion.Path != "" && !validCriterionPath(criterion.Path) {
				return errors.New("semantic criterion path must be workspace-relative")
			}
			if criterion.Content != nil && len(*criterion.Content) > 64*1024 {
				return errors.New("semantic criterion context exceeds 64 KiB")
			}
		case "file_content":
			if criterion.Content == nil || len(*criterion.Content) > 64*1024 || !validCriterionPath(criterion.Path) {
				return errors.New("file_content requires a workspace-relative path and exact content of at most 64 KiB")
			}
		default:
			return fmt.Errorf("unsupported acceptance criterion %q", criterion.Kind)
		}
	}
	return nil
}

func validCriterionPath(value string) bool {
	clean := path.Clean(value)
	return clean != "." && !path.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.ContainsAny(value, "\\\x00")
}

type AdmissionFunc func(context.Context, tool.Invocation, Contract) (Goal, error)
type admissionKey struct{}

// WithAdmission attaches the conversation's trusted admission boundary. The
// model cannot supply session/turn identity, original input, limits or actor.
func WithAdmission(ctx context.Context, admit AdmissionFunc) context.Context {
	return context.WithValue(ctx, admissionKey{}, admit)
}

func AdmissionEnabled(ctx context.Context) bool {
	admit, ok := ctx.Value(admissionKey{}).(AdmissionFunc)
	return ok && admit != nil
}

func StartBinding() tool.Binding {
	return tool.Binding{
		Descriptor: tool.Descriptor{
			Name: StartToolName, Revision: "builtin-v1", RiskClass: tool.RiskPure, Capabilities: []string{"goal.start"},
			Description: "Accept an authorized task with an immutable objective, scope and complete acceptance criteria. Not needed for ordinary answers or tool use alone; grants no new permissions.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["objective","scope","criteria"],"properties":{"objective":{"type":"string"},"scope":{"type":"string"},"criteria":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","additionalProperties":false,"required":["description","kind"],"properties":{"description":{"type":"string"},"kind":{"type":"string","enum":["semantic","file_content"],"description":"semantic: evaluator judges the requirement. file_content: exact whole-file UTF-8 comparison."},"path":{"type":"string","description":"Workspace-relative file path. Required for file_content; optional context for semantic."},"content":{"type":"string","description":"At most 64 KiB. file_content: required exact entire file contents, not prose or a substring. semantic: optional contextual requirement, judged by the evaluator, never mechanically compared."}}}}}}`),
		},
		Executor: tool.InvokeFunc(func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
			admit, ok := ctx.Value(admissionKey{}).(AdmissionFunc)
			invocation, trusted := tool.InvocationFromContext(ctx)
			if !ok || admit == nil || !trusted {
				return nil, errors.New("goal_start requires an admission-enabled conversation turn")
			}
			var contract Contract
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&contract); err != nil {
				return nil, err
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				return nil, errors.New("unexpected trailing task contract data")
			}
			if err := contract.Validate(); err != nil {
				return nil, err
			}
			goal, err := admit(ctx, invocation, contract)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"goal_id": goal.ID, "contract": goal.Contract, "instruction": ProtocolInstruction})
		}),
	}
}
