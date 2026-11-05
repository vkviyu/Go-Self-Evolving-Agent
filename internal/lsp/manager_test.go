package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagerSynchronizesAndBoundsDiagnostics(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	content := []byte("package main\n\nfunc main() { println(missing) }\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Enabled: true, Command: os.Args[0],
		Args:                  []string{"-test.run=^TestLSPHelperProcess$", "--", "--lsp-helper"},
		DiagnosticTimeout:     2 * time.Second,
		MaxDiagnosticsPerFile: 1,
		MaxDiagnosticsTotal:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})

	report := manager.DiagnoseChange(context.Background(), root, path, content)
	if report.Status != StatusReady {
		t.Fatalf("status = %q, message = %q", report.Status, report.Message)
	}
	if report.Path != "main.go" || !report.Truncated {
		t.Fatalf("report metadata = %#v", report)
	}
	if len(report.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %#v", report.Diagnostics)
	}
	if first := report.Diagnostics[0]; first.Path != "main.go" || first.Severity != "error" || first.Start.Line != 3 || first.Code != "compiler" {
		t.Fatalf("first diagnostic = %#v", first)
	}
	if second := report.Diagnostics[1]; second.Path != "other.go" || second.Severity != "error" {
		t.Fatalf("second diagnostic = %#v", second)
	}
}

func TestManagerReportsUnsupportedFilesWithoutStartingServer(t *testing.T) {
	manager, err := NewManager(Config{
		Enabled: true, Command: "must-not-start",
		DiagnosticTimeout: time.Second, MaxDiagnosticsPerFile: 1, MaxDiagnosticsTotal: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	report := manager.DiagnoseChange(context.Background(), t.TempDir(), "README.md", []byte("text"))
	if report.Status != StatusNotApplicable || report.Server != "" {
		t.Fatalf("report = %#v", report)
	}
}

func TestManagerRecoversDurableDiagnosticsForSameRevision(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	content := []byte("package main\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryProjectionStore{items: make(map[string]Projection)}
	manager, err := NewManager(Config{
		Enabled: true, Command: os.Args[0], Args: []string{"-test.run=^TestLSPHelperProcess$", "--", "--lsp-helper"},
		DiagnosticTimeout: 2 * time.Second, MaxDiagnosticsPerFile: 5, MaxDiagnosticsTotal: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.AttachProjectionStore(store)
	first := manager.DiagnoseChange(context.Background(), root, path, content)
	if first.Status != StatusReady || !first.Persisted {
		t.Fatalf("first report = %#v", first)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewManager(Config{
		Enabled: true, Command: filepath.Join(root, "missing-language-server"),
		DiagnosticTimeout: time.Second, MaxDiagnosticsPerFile: 5, MaxDiagnosticsTotal: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.AttachProjectionStore(store)
	recovered := restarted.DiagnoseChange(context.Background(), root, path, content)
	if recovered.Status != StatusPartial || !recovered.Cached || !recovered.Persisted {
		t.Fatalf("recovered report = %#v", recovered)
	}
}

type memoryProjectionStore struct {
	mu    sync.Mutex
	items map[string]Projection
}

func (store *memoryProjectionStore) SaveProjection(_ context.Context, projection Projection) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.items[projection.WorkspaceRoot+"\x00"+projection.Path+"\x00"+projection.Revision] = projection
	return nil
}

func (store *memoryProjectionStore) LoadProjection(_ context.Context, root, path, revision string) (Projection, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	projection, found := store.items[root+"\x00"+path+"\x00"+revision]
	return projection, found, nil
}

func TestProtocolRejectsOversizedFrame(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n", maxProtocolMessageBytes+1)))
	if _, err := readProtocolMessage(reader); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("error = %v", err)
	}
}

func TestLSPHelperProcess(t *testing.T) {
	if !hasArgument("--lsp-helper") {
		return
	}
	if err := serveTestLSP(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func serveTestLSP(input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	var root string
	for {
		payload, err := readProtocolMessage(reader)
		if err != nil {
			return err
		}
		var message wireMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			return err
		}
		switch message.Method {
		case "initialize":
			var params struct {
				RootURI string `json:"rootUri"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return err
			}
			root, err = uriToPath(params.RootURI)
			if err != nil {
				return err
			}
			if err := writeProtocolMessage(output, wireMessage{JSONRPC: "2.0", ID: message.ID, Result: mustMarshal(map[string]any{"capabilities": map[string]any{"textDocumentSync": 1}})}); err != nil {
				return err
			}
		case "textDocument/didOpen":
			var params struct {
				TextDocument struct {
					URI     string `json:"uri"`
					Version int    `json:"version"`
				} `json:"textDocument"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return err
			}
			otherVersion := params.TextDocument.Version
			if err := publishTestDiagnostics(output, pathToURI(filepath.Join(root, "other.go")), otherVersion, []protocolDiagnostic{{
				Range:    protocolRange{Start: protocolPosition{Line: 0}, End: protocolPosition{Line: 0, Character: 1}},
				Severity: 1, Source: "fake", Message: "other file error",
			}}); err != nil {
				return err
			}
			errorDiagnostic := protocolDiagnostic{
				Range:    protocolRange{Start: protocolPosition{Line: 2, Character: 22}, End: protocolPosition{Line: 2, Character: 29}},
				Severity: 1, Code: "compiler", Source: "fake", Message: "undefined: missing",
			}
			if err := publishTestDiagnostics(output, params.TextDocument.URI, params.TextDocument.Version, []protocolDiagnostic{
				{Range: protocolRange{Start: protocolPosition{Line: 1}, End: protocolPosition{Line: 1, Character: 1}}, Severity: 2, Source: "fake", Message: "warning"},
				errorDiagnostic,
				errorDiagnostic,
			}); err != nil {
				return err
			}
		case "shutdown":
			if err := writeProtocolMessage(output, wireMessage{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage("null")}); err != nil {
				return err
			}
		case "exit":
			return nil
		}
	}
}

func publishTestDiagnostics(output io.Writer, uri string, version int, diagnostics []protocolDiagnostic) error {
	return writeProtocolMessage(output, wireMessage{
		JSONRPC: "2.0", Method: "textDocument/publishDiagnostics",
		Params: mustMarshal(publishDiagnosticsParams{URI: uri, Version: &version, Diagnostics: diagnostics}),
	})
}

func hasArgument(want string) bool {
	return slices.Contains(os.Args, want)
}

func TestGoplsIntegration(t *testing.T) {
	if os.Getenv("GSEA_TEST_GOPLS") != "1" {
		t.Skip("set GSEA_TEST_GOPLS=1 to run the real gopls integration")
	}
	command, err := exec.LookPath("gopls")
	if err != nil {
		t.Skip("gopls is not installed")
	}
	root := t.TempDir()
	t.Setenv("GOCACHE", filepath.Join(t.TempDir(), "go-cache"))
	t.Setenv("GOPATH", filepath.Join(t.TempDir(), "go-path"))
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/lspcheck\n\ngo 1.23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	content := []byte("package main\n\nfunc main() { println(missing) }\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Enabled: true, Command: command, Args: []string{"serve"}, DiagnosticTimeout: 10 * time.Second,
		MaxDiagnosticsPerFile: 10, MaxDiagnosticsTotal: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	report := manager.DiagnoseChange(context.Background(), root, path, content)
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity == "error" && strings.Contains(diagnostic.Message, "missing") {
			return
		}
	}
	t.Fatalf("gopls did not report undefined symbol: %#v", report)
}
