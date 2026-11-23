// Package architecture_test guards production dependency and construction
// boundaries. It parses all platform variants without compiling or executing
// application code; integration tests remain responsible for runtime behavior.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/vkviyu/go-self-evolving-agent/"

// These are architectural roles, not a snapshot of every current import. UI
// and assembly packages may evolve, but cannot be imported by lower layers.
var ownedImports = map[string][]string{
	// Production hosts use ATE contracts/client; engine assembly is test-only.
	"github.com/vkviyu/agent-temporal-engine/engine": {},
	module + "internal/app":                          {"internal/cli"},
	module + "internal/cli":                          {"cmd/gsea"},
	module + "internal/runtimestore":                 {"internal/app", "internal/testfixture"},
	"database/sql":                                   {"internal/runtimestore", "internal/workspacedomain"},
	"modernc.org/sqlite":                             {"internal/runtimestore", "internal/workspacedomain"},
	module + "internal/testfixture":                  {},
	"github.com/cloudwego/eino/adk":                  {"internal/agentruntime"},
	"github.com/cloudwego/eino/compose":              {"internal/agentruntime", "internal/einoadapter"},
	"github.com/cloudwego/eino-ext":                  {"internal/modelgateway"},
	"github.com/tetratelabs/wazero":                  {"internal/plugin/wasm"},
}

// Domain protocols and services depend inward through their explicit ports.
// Adding a new domain dependency requires reviewing the role, not approving an
// unrelated import merely because it compiles. Descendant packages inherit it.
var domainDependencies = map[string][]string{
	"internal/session":         {},
	"internal/sessionregistry": {"internal/ateauthority"},
	// Workspace coordination database shared by every Session on one
	// workspace; it must not reach into any Session database.
	"internal/workspacedomain": {"internal/orchestration"},
	"internal/orchestration":   {"internal/execution", "internal/session"},
	"internal/policy":          {"internal/orchestration"},
	"internal/tool":            {"internal/orchestration", "internal/policy", "internal/session"},
	"internal/agentprofile":    {},
	"internal/execution":       {},
	"internal/strategy":        {},
	"internal/agentflow":       {"internal/orchestration", "internal/session", "internal/tokenusage", "internal/tool"},
	"internal/conversation":    {"internal/agentruntime", "internal/orchestration", "internal/session", "internal/tool"},
	"internal/agentruntime":    {"internal/agentprofile", "internal/einoadapter", "internal/experienceport", "internal/modelcontext", "internal/modeltelemetry", "internal/orchestration", "internal/policy", "internal/session", "internal/session/memorystore", "internal/strategy", "internal/tokenizer", "internal/tokenusage", "internal/tool"},
	"internal/agentmanager":    {"internal/agentprofile", "internal/orchestration", "internal/scheduler", "internal/tokenusage", "internal/tool", "internal/workspacecoord"},
	"internal/einoadapter":     {"internal/orchestration", "internal/session", "internal/tool"},
	"internal/modelgateway":    {"internal/modelcontext", "internal/modeltelemetry", "internal/tokenizer"},
	"internal/modelcontext":    {},
	"internal/modeltelemetry":  {},
	"internal/retention":       {"internal/session", "internal/tool"},
	"internal/tokenusage":      {"internal/orchestration"},
	"internal/tokenizer":       {},
	"internal/runtimeowner":    {},
	// Temporal implementations depend on shared contracts; the runtime consumes
	// experienceport and must not import the service, bridge, or process manager.
	"internal/ateprocess":     {"internal/runtimeowner"},
	"internal/aterequest":     {},
	"internal/ateauthority":   {"internal/aterequest"},
	"internal/atebridge":      {"internal/aterequest", "internal/session", "internal/ateports"},
	"internal/experience":     {"internal/atebridge", "internal/experienceport", "internal/session", "internal/ateports"},
	"internal/experienceport": {"internal/ateports"},
	"internal/ateports":       {},
	"internal/scheduler":      {},
	"internal/evolutiontools": {"internal/pluginbuild", "internal/strategy", "internal/tool"},
	"internal/evolutionstore": {"internal/ateauthority", "internal/orchestration", "internal/plugin", "internal/pluginbuild", "internal/retention", "internal/strategy"},
	"internal/pluginbuild":    {"internal/plugin", "internal/sandbox", "internal/tool", "sdk/pluginabi"},
	"internal/plugin":         {"internal/plugin/wasm", "internal/tool"},
	"internal/plugin/wasm":    {},
	"sdk/pluginabi":           {},
	// Adapters implement domain ports, but must not call the runtime they
	// persist. Explicit roles override the pure session protocol above.
	"internal/session/memorystore":    {"internal/session"},
	"internal/session/atestore":       {"internal/aterequest", "internal/session", "internal/ateports"},
	"internal/orchestration/atestore": {"internal/ateauthority", "internal/agentflow", "internal/agentprofile", "internal/orchestration", "internal/retention", "internal/session", "internal/tokenusage", "internal/tool", "internal/workspacecoord"},
	"internal/runtimestore":           {"internal/agentflow", "internal/aterequest", "internal/lsp", "internal/orchestration", "internal/plugin", "internal/pluginbuild", "internal/retention", "internal/session", "internal/strategy", "internal/ateports", "internal/tokenusage", "internal/tool", "internal/workspacecoord"},
}

// Check imported selectors, not source spelling, so aliases and taking a
// constructor as a function value cannot silently introduce another owner.
var constructors = map[string][]string{
	"github.com/cloudwego/eino/adk.NewRunner":         {"internal/agentruntime/eino_turn.go"},
	"github.com/cloudwego/eino/adk.NewChatModelAgent": {"internal/agentruntime/eino_turn.go"},
	// Native integration fixtures are forbidden from production imports.
	module + "internal/runtimestore.OpenRuntime": {"internal/app/open.go", "internal/app/maintenance.go", "internal/app/catalog.go", "internal/app/control.go", "internal/testfixture/native.go"},
	module + "internal/workspacedomain.Open":     {"internal/app/open.go", "internal/app/control.go"},
	module + "internal/plugin/wasm.NewRuntime":   {"internal/app/open.go"},
	module + "internal/tool.NewPipeline":         {"internal/app/open.go", "internal/agentruntime/strategy_shadow.go"},
}

func within(value, prefix string) bool {
	return value == prefix || strings.HasPrefix(value, prefix+"/")
}

func dependencyViolation(importer, imported string) string {
	for dependency, owners := range ownedImports {
		if within(imported, dependency) && !slices.Contains(owners, importer) {
			return "dependency must remain owned by " + strings.Join(owners, ", ")
		}
	}
	if !strings.HasPrefix(imported, module) {
		if within(importer, "internal/evolutiontools") && within(imported, "github.com/cloudwego/eino") {
			return "model-facing tool adapters must not depend on Eino"
		}
		return ""
	}
	role := ""
	for candidate := range domainDependencies {
		if within(importer, candidate) && len(candidate) > len(role) {
			role = candidate
		}
	}
	if role != "" {
		dependency := strings.TrimPrefix(imported, module)
		if dependency != importer && !slices.Contains(domainDependencies[role], dependency) {
			return "dependency points outside the " + role + " domain ports"
		}
	}
	return ""
}

func constructionViolations(filePath string, file *ast.File) []string {
	imports := make(map[string]string)
	var violations []string
	for _, spec := range file.Imports {
		imported, _ := strconv.Unquote(spec.Path.Value)
		alias := path.Base(imported)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias == "." {
			violations = append(violations, "dot imports obscure construction ownership: "+imported)
		}
		imports[alias] = imported
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		alias, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		qualified := imports[alias.Name] + "." + selector.Sel.Name
		if owners, protected := constructors[qualified]; protected && !slices.Contains(owners, filePath) {
			if len(owners) == 0 {
				violations = append(violations, qualified+" is unavailable to production assembly")
			} else {
				violations = append(violations, qualified+" must be constructed in "+strings.Join(owners, ", "))
			}
		}
		return true
	})
	return violations
}

func TestProductionArchitecture(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate architecture test")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(source)))
	files := 0
	for _, tree := range []string{"cmd", "internal", "sdk"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
			if err != nil {
				return err
			}
			files++
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			for _, spec := range file.Imports {
				imported, _ := strconv.Unquote(spec.Path.Value)
				if reason := dependencyViolation(path.Dir(relative), imported); reason != "" {
					t.Errorf("%s imports %s: %s", relative, imported, reason)
				}
			}
			for _, reason := range constructionViolations(relative, file) {
				t.Errorf("%s: %s", relative, reason)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files == 0 {
		t.Fatal("no production files inspected")
	}
}

func TestArchitectureRulesDetectBoundaryViolations(t *testing.T) {
	for _, test := range []struct {
		importer, dependency string
		allowed              bool
	}{
		{"internal/session/atestore", "github.com/vkviyu/agent-temporal-engine/engine", false},
		{"internal/app", "github.com/vkviyu/agent-temporal-engine/engine", false},
		{"internal/session/atestore", "github.com/vkviyu/agent-temporal-engine/api", true},
		{"internal/session/atestore", "github.com/vkviyu/agent-temporal-engine/client", true},
		{"internal/app", module + "internal/runtimestore", true},
		{"internal/cli", module + "internal/runtimestore", false},
		{"internal/cli", module + "internal/app", true},
		{"internal/strategy", module + "internal/app", false},
		{"internal/evolutiontools", "github.com/cloudwego/eino/schema", false},
		{"internal/evolutiontools", module + "internal/strategy", true},
		{"internal/agentflow", "github.com/cloudwego/eino/adk", false},
		{"internal/einoadapter", "github.com/cloudwego/eino/compose", true},
		{"internal/tool/nested", module + "internal/codingtools", false},
		{"internal/agentmanager", "database/sql", false},
		{"internal/workspacedomain", "database/sql", true},
		{"internal/workspacedomain", module + "internal/runtimestore", false},
		{"internal/runtimestore", module + "internal/agentruntime", false},
		{"internal/agentruntime", module + "internal/cli", false},
		{"internal/agentruntime", module + "internal/agentmanager", false},
		{"internal/agentruntime", module + "internal/experienceport", true},
		{"internal/agentruntime", module + "internal/experience", false},
		{"internal/agentruntime", module + "internal/atebridge", false},
		{"internal/agentruntime", module + "internal/ateprocess", false},
		{"internal/agentruntime", module + "internal/ateports", false},
		{"internal/runtimestore", module + "internal/ateports", true},
		{"internal/runtimestore", module + "internal/experience", false},
		{"internal/ateprocess", module + "internal/runtimeowner", true},
		{"internal/ateprocess", module + "internal/agentruntime", false},
		{"internal/ateprocess", module + "internal/atebridge", false},
		{"internal/atebridge", module + "internal/session", true},
		{"internal/atebridge", module + "internal/ateports", true},
		{"internal/atebridge", module + "internal/agentruntime", false},
		{"internal/atebridge", module + "internal/experience", false},
		{"internal/experience", module + "internal/atebridge", true},
		{"internal/experience", module + "internal/experienceport", true},
		{"internal/experience", module + "internal/session", true},
		{"internal/experience", module + "internal/ateports", true},
		{"internal/experience", module + "internal/agentruntime", false},
		{"internal/experience", module + "internal/runtimestore", false},
		{"internal/experienceport", module + "internal/ateports", true},
		{"internal/experienceport", module + "internal/experience", false},
		{"internal/experienceport", module + "internal/atebridge", false},
		{"internal/experienceport", module + "internal/agentruntime", false},
		{"internal/ateports", module + "internal/runtimestore", false},
		{"internal/ateports", module + "internal/experienceport", false},
		{"internal/ateports", module + "internal/atebridge", false},
		{"internal/conversation", module + "internal/agentruntime", true},
		{"internal/modelgateway", module + "internal/conversation", false},
		{"internal/modelgateway", "github.com/cloudwego/eino-ext/components/model/openai", true},
		{"internal/cli", "github.com/cloudwego/eino-ext/components/model/openai", false},
		{"sdk/pluginabi", module + "internal/tool", false},
	} {
		if got := dependencyViolation(test.importer, test.dependency); (got == "") != test.allowed {
			t.Errorf("%s -> %s: allowed=%v, reason=%q", test.importer, test.dependency, test.allowed, got)
		}
	}
	for _, test := range []struct{ name, source string }{
		{"alias", `package fixture; import db "` + module + `internal/runtimestore"; var open = db.OpenRuntime`},
		{"dot", `package fixture; import . "` + module + `internal/runtimestore"; var open = OpenRuntime`},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", test.source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(constructionViolations("internal/cli/fixture.go", file)) == 0 {
			t.Errorf("%s bypassed constructor boundary", test.name)
		}
	}
}
