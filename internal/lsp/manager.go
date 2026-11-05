package lsp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const failedStartRetryDelay = 30 * time.Second

type startFailure struct {
	at  time.Time
	err error
}

type normalizedChange struct {
	Change
	canonical string
}

// Manager lazily owns one language-server process per workspace root. Git
// worktrees therefore receive isolated document and diagnostic state, while
// shared workspace views reuse one server.
type Manager struct {
	config Config
	store  ProjectionStore

	mu       sync.Mutex
	clients  map[string]*client
	failures map[string]startFailure
	closed   bool
}

// AttachProjectionStore enables durable reports without coupling language
// server process ownership to SQLite. It must be called before first use.
func (m *Manager) AttachProjectionStore(store ProjectionStore) {
	if m != nil {
		m.store = store
	}
}

// NewManager validates configuration without starting a process. Startup is
// deferred until a supported file is changed or explicitly diagnosed.
func NewManager(config Config) (*Manager, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Manager{
		config:   config,
		clients:  make(map[string]*client),
		failures: make(map[string]startFailure),
	}, nil
}

// DiagnoseChange synchronizes one document, waits for the next diagnostic
// publication, then returns a ranked, deduplicated and bounded workspace
// snapshot. Failures are structured because the file mutation may already
// have succeeded and must never be reported as safe to retry.
func (m *Manager) DiagnoseChange(ctx context.Context, root, path string, content []byte) Report {
	digest := sha256.Sum256(content)
	return m.DiagnoseChanges(ctx, ChangeSet{Root: root, Changes: []Change{{
		Path: path, Kind: ChangeModified, Revision: hex.EncodeToString(digest[:]), Content: append([]byte(nil), content...),
	}}})
}

// DiagnoseChanges synchronizes a bounded mutation set and returns one ranked
// workspace snapshot. Created, modified and deleted files share one LSP
// lifecycle so shell mutations do not lose passive feedback.
func (m *Manager) DiagnoseChanges(ctx context.Context, set ChangeSet) Report {
	report := Report{Diagnostics: []Diagnostic{}, Truncated: set.Truncated, Message: set.Message}
	if m == nil || !m.config.Enabled {
		report.Status = StatusUnavailable
		report.Message = joinMessage(report.Message, "language-server diagnostics are disabled")
		return report
	}
	canonicalRoot, err := canonicalWorkspaceRoot(set.Root)
	if err != nil {
		report.Status = StatusUnavailable
		report.Message = joinMessage(report.Message, err.Error())
		return report
	}
	changes := make([]normalizedChange, 0, len(set.Changes))
	for _, change := range set.Changes {
		if !SupportsPath(change.Path) {
			continue
		}
		canonicalPath, relative, normalizeErr := canonicalChangedPath(canonicalRoot, change.Path, change.Kind == ChangeDeleted)
		if normalizeErr != nil {
			report.Status = StatusUnavailable
			report.Message = joinMessage(report.Message, normalizeErr.Error())
			return report
		}
		change.Relative = relative
		if change.Revision == "" {
			if change.Kind == ChangeDeleted {
				change.Revision = "deleted"
			} else {
				digest := sha256.Sum256(change.Content)
				change.Revision = hex.EncodeToString(digest[:])
			}
		}
		changes = append(changes, normalizedChange{Change: change, canonical: canonicalPath})
		report.Changes = append(report.Changes, ChangeSummary{Path: relative, Kind: change.Kind, Revision: change.Revision})
	}
	if len(changes) == 0 {
		report.Status = StatusNotApplicable
		return report
	}
	report.Server = m.config.Command
	report.Revision = changeSetRevision(report.Changes)
	if len(changes) == 1 {
		report.Path = changes[0].Relative
		report.Revision = changes[0].Revision
	}
	client, err := m.clientFor(ctx, canonicalRoot)
	if err != nil {
		return m.recoverProjection(ctx, canonicalRoot, changes, report, err)
	}

	diagnosticContext, cancel := context.WithTimeout(ctx, m.config.DiagnosticTimeout)
	var syncErr error
	changedPaths := make(map[string]struct{}, len(changes))
	for index, change := range changes {
		changedPaths[change.canonical] = struct{}{}
		if change.Kind == ChangeDeleted {
			syncErr = errors.Join(syncErr, client.deleteDocument(change.canonical))
			continue
		}
		if len(change.Content) == 0 {
			content, readErr := os.ReadFile(change.canonical)
			if readErr != nil {
				syncErr = errors.Join(syncErr, readErr)
				continue
			}
			change.Content = content
		}
		syncErr = errors.Join(syncErr, client.syncDocument(diagnosticContext, change.canonical, change.Content))
		if diagnosticContext.Err() != nil {
			if index+1 < len(changes) {
				report.Truncated = true
				report.Message = joinMessage(report.Message, fmt.Sprintf("diagnostics stopped after %d of %d changed files", index+1, len(changes)))
			}
			break
		}
	}
	cancel()
	diagnostics, diagnosticsTruncated := rankDedupLimit(
		canonicalRoot,
		changedPaths,
		client.diagnosticsSnapshot(),
		m.config.MaxDiagnosticsPerFile,
		m.config.MaxDiagnosticsTotal,
	)
	report.Diagnostics = diagnostics
	report.Truncated = report.Truncated || diagnosticsTruncated
	if syncErr == nil {
		report.Status = StatusReady
		return m.persistProjection(ctx, canonicalRoot, changes, report)
	}
	if !client.alive() {
		m.removeClient(canonicalRoot, client)
	}
	if len(report.Diagnostics) == 0 && !errors.Is(syncErr, context.DeadlineExceeded) {
		// recoverProjection records syncErr in the message itself.
		return m.recoverProjection(ctx, canonicalRoot, changes, report, syncErr)
	}
	report.Status = StatusPartial
	report.Message = joinMessage(report.Message, syncErr.Error())
	return m.persistProjection(ctx, canonicalRoot, changes, report)
}

func (m *Manager) persistProjection(ctx context.Context, root string, changes []normalizedChange, report Report) Report {
	if m.store == nil {
		return report
	}
	report.Persisted = true
	for _, change := range changes {
		projection := Projection{WorkspaceRoot: root, Path: change.Relative, Revision: change.Revision, Report: report, UpdatedAt: time.Now().UTC()}
		if err := m.store.SaveProjection(context.WithoutCancel(ctx), projection); err != nil {
			report.Persisted = false
			report.Message = joinMessage(report.Message, "persist diagnostics: "+err.Error())
			break
		}
	}
	return report
}

func (m *Manager) recoverProjection(ctx context.Context, root string, changes []normalizedChange, report Report, cause error) Report {
	report.Status = StatusUnavailable
	report.Message = joinMessage(report.Message, cause.Error())
	if m.store == nil {
		return report
	}
	seen := make(map[string]struct{})
	items := make([]Diagnostic, 0)
	found := false
	for _, change := range changes {
		projection, ok, err := m.store.LoadProjection(context.WithoutCancel(ctx), root, change.Relative, change.Revision)
		if err != nil {
			report.Message = joinMessage(report.Message, "load durable diagnostics: "+err.Error())
			continue
		}
		if !ok {
			continue
		}
		found = true
		for _, diagnostic := range projection.Report.Diagnostics {
			encoded := fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s", diagnostic.Path, diagnostic.Start.Line, diagnostic.Start.Column, diagnostic.Code, diagnostic.Message)
			if _, exists := seen[encoded]; exists {
				continue
			}
			seen[encoded] = struct{}{}
			items = append(items, diagnostic)
		}
	}
	if !found {
		return report
	}
	if len(items) > m.config.MaxDiagnosticsTotal {
		items = items[:m.config.MaxDiagnosticsTotal]
		report.Truncated = true
	}
	report.Diagnostics = items
	report.Status = StatusPartial
	report.Cached = true
	report.Persisted = true
	report.Message = joinMessage(report.Message, "using durable diagnostics for the same file revision")
	return report
}

func changeSetRevision(changes []ChangeSummary) string {
	encoded := mustMarshal(changes)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func joinMessage(current, next string) string {
	if current == "" {
		return next
	}
	return current + "; " + next
}

func (m *Manager) clientFor(ctx context.Context, root string) (*client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("language-server manager is closed")
	}
	if existing := m.clients[root]; existing != nil {
		if existing.alive() {
			return existing, nil
		}
		delete(m.clients, root)
	}
	if failure, ok := m.failures[root]; ok && time.Since(failure.at) < failedStartRetryDelay {
		return nil, failure.err
	}
	started, err := startClient(ctx, root, m.config)
	if err != nil {
		m.failures[root] = startFailure{at: time.Now(), err: err}
		return nil, err
	}
	delete(m.failures, root)
	m.clients[root] = started
	return started, nil
}

func (m *Manager) removeClient(root string, target *client) {
	m.mu.Lock()
	if m.clients[root] == target {
		delete(m.clients, root)
	}
	m.mu.Unlock()
}

// Close stops every language server before workspace and database resources
// are released.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := make([]*client, 0, len(m.clients))
	for _, current := range m.clients {
		clients = append(clients, current)
	}
	m.clients = make(map[string]*client)
	m.mu.Unlock()
	var result error
	for _, current := range clients {
		result = errors.Join(result, current.Close())
	}
	return result
}

func canonicalDocument(root, path string) (string, string, string, error) {
	canonicalRoot, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return "", "", "", err
	}
	canonicalPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve LSP document path: %w", err)
	}
	canonicalPath, err = filepath.EvalSymlinks(canonicalPath)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve LSP document symlinks: %w", err)
	}
	relative, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", "", errors.New("LSP document escapes workspace root")
	}
	return canonicalRoot, canonicalPath, filepath.ToSlash(relative), nil
}

func canonicalWorkspaceRoot(root string) (string, error) {
	canonicalRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve LSP workspace root: %w", err)
	}
	canonicalRoot, err = filepath.EvalSymlinks(canonicalRoot)
	if err != nil {
		return "", fmt.Errorf("resolve LSP workspace root symlinks: %w", err)
	}
	return canonicalRoot, nil
}

func canonicalChangedPath(root, path string, deleted bool) (string, string, error) {
	if !deleted {
		_, canonical, relative, err := canonicalDocument(root, path)
		return canonical, relative, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve deleted LSP document path: %w", err)
	}
	parent := filepath.Dir(abs)
	suffix := []string{filepath.Base(abs)}
	for {
		resolved, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr == nil {
			parent = resolved
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) || parent == filepath.Dir(parent) {
			return "", "", fmt.Errorf("resolve deleted LSP document parent: %w", resolveErr)
		}
		suffix = append([]string{filepath.Base(parent)}, suffix...)
		parent = filepath.Dir(parent)
	}
	canonicalPath := filepath.Join(append([]string{parent}, suffix...)...)
	relative, err := filepath.Rel(root, canonicalPath)
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", errors.New("LSP document escapes workspace root")
	}
	return canonicalPath, filepath.ToSlash(relative), nil
}

// SupportsPath reports whether the configured Go language-server integration
// owns the file type.
func SupportsPath(path string) bool {
	base := filepath.Base(path)
	return strings.EqualFold(filepath.Ext(path), ".go") || base == "go.mod" || base == "go.work"
}

func languageID(path string) string {
	switch filepath.Base(path) {
	case "go.mod":
		return "go.mod"
	case "go.work":
		return "go.work"
	default:
		return "go"
	}
}

func pathToURI(path string) string {
	slashPath := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(slashPath) >= 2 && slashPath[1] == ':' {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}

func uriToPath(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("unsupported diagnostic URI scheme %q", parsed.Scheme)
	}
	path := parsed.Path
	if parsed.Host != "" && parsed.Host != "localhost" {
		path = "//" + parsed.Host + path
	}
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path), nil
}

type rankedDiagnostic struct {
	diagnostic Diagnostic
	severity   int
	changed    bool
	key        string
}

func rankDedupLimit(root string, changedPaths map[string]struct{}, published map[string]publishedDiagnostics, perFile, total int) ([]Diagnostic, bool) {
	candidates := make([]rankedDiagnostic, 0)
	seen := make(map[string]struct{})
	for uri, publication := range published {
		path, err := uriToPath(uri)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(relative) {
			continue
		}
		relative = filepath.ToSlash(relative)
		for _, source := range publication.diagnostics {
			message := strings.TrimSpace(source.Message)
			if message == "" {
				continue
			}
			code := diagnosticCode(source.Code)
			diagnostic := Diagnostic{
				Path:     relative,
				Start:    Position{Line: source.Range.Start.Line + 1, Column: source.Range.Start.Character + 1},
				End:      Position{Line: source.Range.End.Line + 1, Column: source.Range.End.Character + 1},
				Severity: severityName(source.Severity),
				Code:     code, Source: source.Source, Message: message,
			}
			key := strings.Join([]string{
				diagnostic.Path,
				strconv.Itoa(diagnostic.Start.Line), strconv.Itoa(diagnostic.Start.Column),
				strconv.Itoa(diagnostic.End.Line), strconv.Itoa(diagnostic.End.Column),
				diagnostic.Severity, diagnostic.Code, diagnostic.Source, diagnostic.Message,
			}, "\x00")
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, rankedDiagnostic{
				diagnostic: diagnostic,
				severity:   severityRank(source.Severity),
				changed:    containsPath(changedPaths, path),
				key:        key,
			})
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		a, b := candidates[left], candidates[right]
		if a.changed != b.changed {
			return a.changed
		}
		if a.severity != b.severity {
			return a.severity < b.severity
		}
		if a.diagnostic.Path != b.diagnostic.Path {
			return a.diagnostic.Path < b.diagnostic.Path
		}
		if a.diagnostic.Start.Line != b.diagnostic.Start.Line {
			return a.diagnostic.Start.Line < b.diagnostic.Start.Line
		}
		if a.diagnostic.Start.Column != b.diagnostic.Start.Column {
			return a.diagnostic.Start.Column < b.diagnostic.Start.Column
		}
		return a.key < b.key
	})
	result := make([]Diagnostic, 0, min(total, len(candidates)))
	fileCounts := make(map[string]int)
	for _, candidate := range candidates {
		if len(result) >= total {
			break
		}
		if fileCounts[candidate.diagnostic.Path] >= perFile {
			continue
		}
		fileCounts[candidate.diagnostic.Path]++
		result = append(result, candidate.diagnostic)
	}
	return result, len(result) < len(candidates)
}

func containsPath(paths map[string]struct{}, target string) bool {
	for path := range paths {
		if samePath(path, target) {
			return true
		}
	}
	return false
}

func diagnosticCode(value any) string {
	switch code := value.(type) {
	case nil:
		return ""
	case string:
		return code
	case float64:
		if code == float64(int64(code)) {
			return strconv.FormatInt(int64(code), 10)
		}
		return strconv.FormatFloat(code, 'f', -1, 64)
	default:
		return fmt.Sprint(code)
	}
}

func severityRank(severity int) int {
	if severity >= 1 && severity <= 4 {
		return severity
	}
	return 5
}

func severityName(severity int) string {
	switch severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "information"
	case 4:
		return "hint"
	default:
		return "unknown"
	}
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
