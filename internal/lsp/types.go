// Package lsp provides optional language-server diagnostics for workspace
// changes. It owns language-server process and protocol lifecycles, while the
// coding tool layer decides when feedback is exposed to an Agent.
package lsp

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	StatusReady         = "ready"
	StatusPartial       = "partial"
	StatusUnavailable   = "unavailable"
	StatusNotApplicable = "not_applicable"
)

// Config controls the optional Go language server. Command is executed
// directly, without a shell, and is expected to speak LSP over stdio.
type Config struct {
	Enabled               bool
	Command               string
	Args                  []string
	DiagnosticTimeout     time.Duration
	MaxDiagnosticsPerFile int
	MaxDiagnosticsTotal   int
}

// Validate checks enabled language-server settings. A disabled zero value is
// valid so programmatic callers do not need to configure LSP.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Command) == "" {
		return errors.New("lsp.command is required when LSP is enabled")
	}
	if c.DiagnosticTimeout <= 0 {
		return errors.New("lsp.diagnostic_timeout must be positive")
	}
	if c.MaxDiagnosticsPerFile <= 0 {
		return errors.New("lsp.max_diagnostics_per_file must be positive")
	}
	if c.MaxDiagnosticsTotal <= 0 {
		return errors.New("lsp.max_diagnostics_total must be positive")
	}
	return nil
}

// Position is one-based for direct display to an Agent. Column is derived
// from the LSP UTF-16 character offset, not a byte offset.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Diagnostic is a bounded, workspace-relative semantic finding.
type Diagnostic struct {
	Path     string   `json:"path"`
	Start    Position `json:"start"`
	End      Position `json:"end"`
	Severity string   `json:"severity"`
	Code     string   `json:"code,omitempty"`
	Source   string   `json:"source,omitempty"`
	Message  string   `json:"message"`
}

// Report is returned both by passive post-write feedback and the active
// workspace_diagnostics tool.
type Report struct {
	Status      string          `json:"status"`
	Server      string          `json:"server,omitempty"`
	Path        string          `json:"path,omitempty"`
	Revision    string          `json:"revision,omitempty"`
	Changes     []ChangeSummary `json:"changes,omitempty"`
	Diagnostics []Diagnostic    `json:"items"`
	Truncated   bool            `json:"truncated,omitempty"`
	Cached      bool            `json:"cached,omitempty"`
	Persisted   bool            `json:"persisted,omitempty"`
	Message     string          `json:"message,omitempty"`
}

type ChangeKind string

const (
	ChangeCreated  ChangeKind = "created"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
)

// Change is a trusted workspace mutation discovered by a write tool. Path is
// absolute and Content is present only for supported created/modified files.
type Change struct {
	Path     string     `json:"-"`
	Relative string     `json:"path"`
	Kind     ChangeKind `json:"kind"`
	Revision string     `json:"revision,omitempty"`
	Content  []byte     `json:"-"`
}

type ChangeSummary struct {
	Path     string     `json:"path"`
	Kind     ChangeKind `json:"kind"`
	Revision string     `json:"revision,omitempty"`
}

type ChangeSet struct {
	Root      string   `json:"-"`
	Changes   []Change `json:"changes"`
	Truncated bool     `json:"truncated,omitempty"`
	Message   string   `json:"message,omitempty"`
}

type Projection struct {
	WorkspaceRoot string
	Path          string
	Revision      string
	Report        Report
	UpdatedAt     time.Time
}

// ProjectionStore persists diagnostics by workspace/file/content revision.
// It intentionally does not own language-server process state.
type ProjectionStore interface {
	SaveProjection(context.Context, Projection) error
	LoadProjection(context.Context, string, string, string) (Projection, bool, error)
}

// ChangeDiagnoser is the narrow boundary consumed by workspace tools.
// Implementations report infrastructure failures as structured status rather
// than errors because a diagnostic failure must not invalidate a completed
// file write.
type ChangeDiagnoser interface {
	DiagnoseChange(ctx context.Context, root, path string, content []byte) Report
}

type ChangeSetDiagnoser interface {
	DiagnoseChanges(ctx context.Context, changes ChangeSet) Report
}
