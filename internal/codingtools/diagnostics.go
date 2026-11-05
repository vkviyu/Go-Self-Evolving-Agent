package codingtools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/vkviyu/go-self-evolving-agent/internal/lsp"
)

type diagnosticsArgs struct {
	Path string `json:"path"`
}

func (workspace *Workspace) languageServerDiagnostics(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if workspace.diagnostics == nil {
		return json.Marshal(lsp.Report{Status: lsp.StatusUnavailable, Diagnostics: []lsp.Diagnostic{}, Message: "language-server diagnostics are disabled"})
	}
	var args diagnosticsArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Path) == "" {
		return nil, errors.New("path is required")
	}
	root, _, err := workspace.operationRoot(ctx)
	if err != nil {
		return nil, err
	}
	path, _, err := workspace.resolveExistingAt(root, args.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maximumReadBytes {
		return nil, errors.New("file is too large for language-server diagnostics")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	report := workspace.diagnostics.DiagnoseChange(ctx, root, path, content)
	return json.Marshal(report)
}

func (workspace *Workspace) appendLanguageDiagnostics(ctx context.Context, root, path, relative string, kind lsp.ChangeKind, content []byte, result map[string]any) {
	change := sourceChange(path, relative, kind, content)
	result["changes"] = []lsp.ChangeSummary{{Path: relative, Kind: kind, Revision: change.Revision}}
	if !workspace.passiveDiagnostics(root) {
		return
	}
	var report lsp.Report
	if diagnoser, ok := workspace.diagnostics.(lsp.ChangeSetDiagnoser); ok {
		report = diagnoser.DiagnoseChanges(ctx, lsp.ChangeSet{Root: root, Changes: []lsp.Change{change}})
	} else {
		report = workspace.diagnostics.DiagnoseChange(ctx, root, path, content)
	}
	if report.Status == "" || report.Status == lsp.StatusNotApplicable {
		return
	}
	result["diagnostics"] = report
}

func (workspace *Workspace) appendChangeSetDiagnostics(ctx context.Context, changes lsp.ChangeSet, result map[string]any) {
	if !workspace.passiveDiagnostics(changes.Root) || len(changes.Changes) == 0 {
		return
	}
	if diagnoser, ok := workspace.diagnostics.(lsp.ChangeSetDiagnoser); ok {
		report := diagnoser.DiagnoseChanges(context.WithoutCancel(ctx), changes)
		if report.Status != "" && report.Status != lsp.StatusNotApplicable {
			result["diagnostics"] = report
		}
		return
	}
	for _, change := range changes.Changes {
		if change.Kind == lsp.ChangeDeleted {
			continue
		}
		report := workspace.diagnostics.DiagnoseChange(context.WithoutCancel(ctx), changes.Root, change.Path, change.Content)
		if report.Status != "" && report.Status != lsp.StatusNotApplicable {
			result["diagnostics"] = report
		}
	}
}
