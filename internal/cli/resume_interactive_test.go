package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/agentruntime"
)

func TestResumeInteractiveSessionIsolation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("interactive resume requires python3 and a Unix PTY")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cfg := cliCommandConfig(t, t.TempDir(), t.TempDir())
	build := func(name string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "gsea")
		cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-buildvcs=false",
			"-ldflags=-buildid="+name, "-o", path, "github.com/vkviyu/go-self-evolving-agent/cmd/gsea")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
		return path
	}
	// The manifest records the actual executable digest of the seeding host.
	oldHost, currentHost := build("resume-old"), build("resume-current")
	const (
		selectedID = "amber-session"
		defaultID  = "blue-session"
		selected   = "Where is amber stored?"
		answer     = "Amber is in drawer seven."
		unfinished = "Inspect the unfinished ledger."
		partial    = "Unconfirmed ledger fragment."
		other      = "Where is cobalt stored?"
		otherReply = "Cobalt is on shelf four."
		follow     = "继续"
		reply      = "Continuing the ledger review from the recorded balance."
		after      = "Label that drawer."
		afterReply = "Label it amber."
		ledger     = "Verified ledger balance is 420."
	)
	ledgerPath := filepath.Join(cfg.WorkspaceDirectory, "ledger.txt")
	if err := os.WriteFile(ledgerPath, []byte(ledger+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		dialog   []string
		reply    string
		readTool bool
		evidence bool
	}{
		{dialog: []string{selected}, reply: answer},
		{dialog: []string{selected, answer, unfinished}, readTool: true},
		{dialog: []string{selected, answer, unfinished}, reply: partial},
		{dialog: []string{other}, reply: otherReply},
		{dialog: []string{selected, answer, follow}, reply: reply, evidence: true},
		{dialog: []string{selected, answer, follow, reply, after}, reply: afterReply},
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r) // Optional provider metadata lookup.
			return
		}
		var request struct {
			Stream bool
			Input  []struct {
				Role    string
				Type    string
				Content []struct{ Text string }
			}
		}
		n := int(calls.Add(1))
		if r.Method != http.MethodPost || r.URL.Path != "/responses" || n > len(steps) ||
			json.NewDecoder(r.Body).Decode(&request) != nil || !request.Stream {
			t.Errorf("unexpected provider request #%d: %s %s", n, r.Method, r.URL.Path)
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
			return
		}
		step := steps[n-1]
		var dialog []string
		var evidence string
		for _, item := range request.Input {
			if item.Type == "function_call" || item.Type == "function_call_output" {
				if step.reply != partial {
					t.Errorf("request #%d unexpectedly reused a tool protocol item", n)
				}
				continue
			}
			if item.Role == "system" {
				if n >= 4 {
					for _, part := range item.Content {
						for _, excluded := range []string{other, otherReply, unfinished, partial} {
							if strings.Contains(part.Text, excluded) {
								t.Errorf("request #%d leaked %q in system input", n, excluded)
							}
						}
					}
				}
				continue
			}
			if item.Role == "user" && len(item.Content) == 1 && strings.HasPrefix(item.Content[0].Text, "[Host context: unfinished work in this session]") {
				evidence = item.Content[0].Text
				continue
			}
			role := []string{"user", "assistant"}[len(dialog)%2]
			if item.Role != role || len(item.Content) != 1 {
				t.Errorf("request #%d message %d: got role %q, %d parts, want one %s message", n, len(dialog), item.Role, len(item.Content), role)
				continue
			}
			dialog = append(dialog, item.Content[0].Text)
		}
		if !slices.Equal(dialog, step.dialog) {
			t.Errorf("request #%d dialog = %q, want %q", n, dialog, step.dialog)
		}
		if step.evidence {
			for _, required := range []string{unfinished, ledger, "workspace_read", "model_request_failed"} {
				if !strings.Contains(evidence, required) {
					t.Errorf("request #%d missing recovery evidence %q: %q", n, required, evidence)
				}
			}
			for _, excluded := range []string{partial, other, otherReply, "CHANGED ON DISK"} {
				if strings.Contains(evidence, excluded) {
					t.Errorf("request #%d leaked %q into interruption evidence", n, excluded)
				}
			}
		} else if evidence != "" {
			t.Errorf("request #%d received unrelated or stale interruption evidence: %q", n, evidence)
		}
		if n >= 4 {
			t.Logf("provider request #%d dialog = %q", n, dialog)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if step.readTool {
			fmt.Fprint(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"ledger-item","call_id":"ledger-read","name":"workspace_read","arguments":"{\"path\":\"ledger.txt\"}"}}`+"\n\n")
			fmt.Fprint(w, `data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"list-item","call_id":"list-files","name":"workspace_list","arguments":"{\"path\":\".\"}"}}`+"\n\n")
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
			return
		}
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", step.reply)
		if step.reply != partial { // EOF before completion leaves a real interrupted turn.
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		}
	}))
	defer server.Close()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(),
		"GSEA_HOME=" + cfg.HomeDirectory, "GSEA_COORDINATION_DIR=" + t.TempDir(),
		"GSEA_ATE_BINARY=" + cfg.ATE.Binary, "GSEA_ATE_DIRECTORY=" + cfg.ATE.Directory,
		"GSEA_LSP_ENABLED=false", "GSEA_MODEL_PROVIDER=openai", "GSEA_MODEL_WIRE_API=responses",
		"GSEA_MODEL=fixture-model", "GSEA_MODEL_BASE_URL=" + server.URL,
		"GSEA_API_KEY_ENV=GSEA_RESUME_TEST_KEY", "GSEA_RESUME_TEST_KEY=fixture",
		"TERM=xterm-256color", "NO_COLOR=1", "COLORFGBG=15;0",
	}
	seed := func(id, prompt, want string, interrupted bool) {
		t.Helper()
		cmd := exec.CommandContext(ctx, oldHost, "chat", "--session", id, "--prompt-file", "-")
		cmd.Dir, cmd.Env, cmd.Stdin = cfg.WorkspaceDirectory, env, strings.NewReader(prompt)
		out, err := cmd.CombinedOutput()
		if (err != nil) != interrupted || !strings.Contains(string(out), want) {
			t.Fatalf("seed %s, %q: %v\n%s", id, prompt, err, out)
		}
	}
	seed(selectedID, selected, answer, false)
	seed(selectedID, unfinished, "execution paused (transport)", true)
	if err := os.WriteFile(ledgerPath, []byte("CHANGED ON DISK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed(defaultID, other, otherReply, false) // Most recently updated, initially highlighted.

	db := openCLIApp(t, cfg)
	turns, err := agentruntime.LoadTurnRecords(ctx, db.Events(), nil, selectedID)
	if err != nil || len(turns) != 2 || !turns[0].Completed || turns[1].Completed ||
		turns[1].OpeningPreview != unfinished || !turns[1].HasJournal || turns[1].RuntimeSnapshotID == "" {
		t.Fatalf("expected one completed turn and one journaled interruption: %+v, %v", turns, err)
	}
	oldTurn := turns[1].TurnID
	before, err := db.Events().CurrentSequence(ctx, defaultID)
	if err != nil || before == 0 {
		t.Fatalf("default session before resume: sequence=%d err=%v", before, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	driver, err := filepath.Abs(filepath.Join("testdata", "resume_interactive.py"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, python, "-I", driver, currentHost, oldTurn,
		selected, answer, unfinished, partial, other, otherReply, follow, reply, after, afterReply)
	cmd.Dir, cmd.Env = cfg.WorkspaceDirectory, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PTY resume: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
	if got := int(calls.Load()); got != len(steps) {
		t.Fatalf("model POSTs = %d, want %d", got, len(steps))
	}

	db = openCLIApp(t, cfg)
	turns, err = agentruntime.LoadTurnRecords(ctx, db.Events(), nil, selectedID)
	if err != nil || len(turns) != 4 || turns[1].TurnID != oldTurn || turns[1].Completed ||
		turns[2].OpeningPreview != follow || !turns[2].Completed ||
		turns[3].OpeningPreview != after || !turns[3].Completed {
		t.Fatalf("selected session after resume: %+v, %v", turns, err)
	}
	last, err := db.Events().CurrentSequence(ctx, defaultID)
	if err != nil || last != before {
		t.Fatalf("default session changed: sequence %d -> %d, err=%v", before, last, err)
	}
	t.Logf("ATE: both follow-ups completed in %s; %s sequence %d -> %d", selectedID, defaultID, before, last)
}
