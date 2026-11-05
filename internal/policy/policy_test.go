package policy

import (
	"context"
	"testing"
)

func TestStaticEngineFirstMatchAndDefaultDeny(t *testing.T) {
	t.Parallel()

	engine, err := DefaultDeny(
		Rule{
			ID:                "read-workspace",
			ToolPattern:       "workspace_*",
			CapabilityPattern: "fs.read",
			ResourcePrefix:    "/workspace/",
			Effect:            EffectAllow,
		},
		Rule{
			ID:                "write-needs-approval",
			ToolPattern:       "workspace_*",
			CapabilityPattern: "fs.write",
			ResourcePrefix:    "/workspace/",
			Effect:            EffectRequireApproval,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		request    Request
		wantEffect Effect
		wantRule   string
	}{
		{
			name:       "allowed read",
			request:    Request{ToolName: "workspace_read", Capability: "fs.read", Resource: "/workspace/main.go"},
			wantEffect: EffectAllow,
			wantRule:   "read-workspace",
		},
		{
			name:       "write requires approval",
			request:    Request{ToolName: "workspace_edit", Capability: "fs.write", Resource: "/workspace/main.go"},
			wantEffect: EffectRequireApproval,
			wantRule:   "write-needs-approval",
		},
		{
			name:       "path escape denied",
			request:    Request{ToolName: "workspace_read", Capability: "fs.read", Resource: "/etc/passwd"},
			wantEffect: EffectDeny,
			wantRule:   "default",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := engine.Evaluate(context.Background(), test.request)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Effect != test.wantEffect || decision.RuleID != test.wantRule {
				t.Fatalf("decision = %#v", decision)
			}
		})
	}
}

func TestStaticEngineRejectsMalformedRule(t *testing.T) {
	t.Parallel()

	_, err := DefaultDeny(Rule{ID: "bad", ToolPattern: "[", Effect: EffectAllow})
	if err == nil {
		t.Fatal("expected malformed pattern error")
	}
}

func TestResourcePrefixRespectsBoundaries(t *testing.T) {
	for _, test := range []struct {
		prefix, resource string
		allowed          bool
	}{
		{"/workspace", "/workspace", true},
		{"/workspace", "/workspace/main.go", true},
		{"/workspace", "/workspace2/main.go", false},
		{"tool:read", "tool:read_all", false},
		{"tool:", "tool:read", true},
		{"/workspace/", "/workspace/main.go", true},
	} {
		engine, err := DefaultDeny(Rule{ID: "bounded", ResourcePrefix: test.prefix, Effect: EffectAllow})
		if err != nil {
			t.Fatal(err)
		}
		decision, err := engine.Evaluate(context.Background(), Request{ToolName: "read", Capability: "fs.read", Resource: test.resource})
		if err != nil {
			t.Fatal(err)
		}
		if (decision.Effect == EffectAllow) != test.allowed {
			t.Errorf("prefix %q resource %q: %s", test.prefix, test.resource, decision.Effect)
		}
	}
}
