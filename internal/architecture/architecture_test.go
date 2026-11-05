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
	module + "internal/app":                 {"internal/cli"},
	module + "internal/cli":                 {"cmd/local-agent"},
	module + "internal/session/sqlitestore": {"internal/app"},
	"database/sql":                          {"internal/session/sqlitestore", "internal/sessionregistry", "internal/workspacedomain"},
	"modernc.org/sqlite":                    {"internal/session/sqlitestore", "internal/sessionregistry", "internal/workspacedomain"},
	"github.com/cloudwego/eino/adk":         {"internal/agentruntime"},
	"github.com/cloudwego/eino/compose":     {"internal/agentruntime", "internal/einoadapter"},
	"github.com/cloudwego/eino-ext":         {"internal/modelgateway"},
	"github.com/tetratelabs/wazero":         {"internal/plugin/wasm"},
}

// Domain protocols and services depend inward through their explicit ports.
// Adding a new domain dependency requires reviewing the role, not approving an
// unrelated import merely because it compiles. Descendant packages inherit it.
var domainDependencies = map[string][]string{
	"internal/session":         {},
	"internal/sessionregistry": {}, // separate Session catalog database, not conversation history
	// Workspace coordination database shared by every Session on one
	// workspace; it must not reach into any Session database.
	"internal/workspacedomain": {"internal/orchestration"},
	"internal/orchestration":   {"internal/session"},
	"internal/policy":          {"internal/orchestration"},
	"internal/tool":            {"internal/orchestration", "internal/policy", "internal/session"},
	"internal/agentprofile":    {},
	"internal/strategy":        {},
	"internal/agentgoal":       {"internal/orchestration", "internal/session", "internal/tokenusage", "internal/tool"},
	"internal/agentflow":       {"internal/orchestration", "internal/session", "internal/tokenusage", "internal/tool"},
	"internal/conversation":    {"internal/agentgoal", "internal/agentruntime", "internal/orchestration", "internal/session", "internal/tool"},
	"internal/agentruntime":    {"internal/agentgoal", "internal/agentprofile", "internal/einoadapter", "internal/modelcontext", "internal/orchestration", "internal/policy", "internal/session", "internal/session/memorystore", "internal/strategy", "internal/tokenizer", "internal/tokenusage", "internal/tool"},
	"internal/agentmanager":    {"internal/agentprofile", "internal/orchestration", "internal/scheduler", "internal/tokenusage", "internal/tool", "internal/workspacecoord"},
	"internal/einoadapter":     {"internal/orchestration", "internal/session", "internal/tool"},
	"internal/modelgateway":    {"internal/modelcontext", "internal/tokenizer"},
	"internal/modelcontext":    {},
	"internal/retention":       {"internal/session", "internal/tool"},
	"internal/tokenusage":      {"internal/orchestration"},
	"internal/tokenizer":       {},
	"internal/runtimeowner":    {},
	"internal/scheduler":       {},
	"internal/evolutiontools":  {"internal/pluginbuild", "internal/strategy", "internal/tool"},
	"internal/pluginbuild":     {"internal/plugin", "internal/sandbox", "internal/tool", "sdk/pluginabi"},
	"internal/plugin":          {"internal/plugin/wasm", "internal/tool"},
	"internal/plugin/wasm":     {},
	"sdk/pluginabi":            {},
	// Adapters implement domain ports, but must not call the runtime they
	// persist. Explicit roles override the pure session protocol above.
	"internal/session/memorystore": {"internal/session"},
	"internal/session/sqlitestore": {"internal/agentflow", "internal/agentgoal", "internal/lsp", "internal/orchestration", "internal/plugin", "internal/pluginbuild", "internal/retention", "internal/session", "internal/strategy", "internal/tokenusage", "internal/tool", "internal/workspacecoord"},
}

// Check imported selectors, not source spelling, so aliases and taking a
// constructor as a function value cannot silently introduce another owner.
var constructors = map[string][]string{
	"github.com/cloudwego/eino/adk.NewRunner":         {"internal/agentruntime/eino_turn.go"},
	"github.com/cloudwego/eino/adk.NewChatModelAgent": {"internal/agentruntime/eino_turn.go"},
	module + "internal/session/sqlitestore.Open":      {"internal/app/open.go", "internal/app/maintenance.go"},
	module + "internal/workspacedomain.Open":          {"internal/app/open.go"},
	module + "internal/plugin/wasm.NewRuntime":        {"internal/app/open.go"},
	module + "internal/tool.NewPipeline":              {"internal/app/open.go", "internal/agentruntime/strategy_shadow.go"},
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
			violations = append(violations, qualified+" must be constructed in "+strings.Join(owners, ", "))
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
		{"internal/app", module + "internal/session/sqlitestore", true},
		{"internal/cli", module + "internal/session/sqlitestore", false},
		{"internal/cli", module + "internal/app", true},
		{"internal/strategy", module + "internal/app", false},
		{"internal/evolutiontools", "github.com/cloudwego/eino/schema", false},
		{"internal/evolutiontools", module + "internal/strategy", true},
		{"internal/agentflow", "github.com/cloudwego/eino/adk", false},
		{"internal/einoadapter", "github.com/cloudwego/eino/compose", true},
		{"internal/tool/nested", module + "internal/codingtools", false},
		{"internal/agentmanager", "database/sql", false},
		{"internal/workspacedomain", "database/sql", true},
		{"internal/workspacedomain", module + "internal/session/sqlitestore", false},
		{"internal/session/sqlitestore", module + "internal/agentgoal", true},
		{"internal/session/sqlitestore", module + "internal/agentruntime", false},
		{"internal/agentruntime", module + "internal/cli", false},
		{"internal/agentruntime", module + "internal/agentmanager", false},
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
		{"alias", `package fixture; import db "` + module + `internal/session/sqlitestore"; var open = db.Open`},
		{"dot", `package fixture; import . "` + module + `internal/session/sqlitestore"; var open = Open`},
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
