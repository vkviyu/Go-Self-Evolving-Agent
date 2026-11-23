package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestControlSnapshotUsesVersionedJSONL(t *testing.T) {
	cliATEEnvironment(t)
	workspace := t.TempDir()
	dataDirectory := t.TempDir()
	application, err := app.Open(context.Background(), cliCommandConfig(t, dataDirectory, workspace))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := application.EnsureRootThread(context.Background(), session.ID("control-session")); err != nil {
		_ = application.Close()
		t.Fatal(err)
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	input := strings.NewReader("{\"command\":\"snapshot\"}\n")
	err = runControl(context.Background(), []string{"--data-dir", dataDirectory, "--workspace", workspace}, input, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Protocol string `json:"protocol"`
		Type     string `json:"type"`
		TeamID   string `json:"team_id"`
		Agents   []any  `json:"agents"`
		Tasks    []any  `json:"tasks"`
		Views    []any  `json:"views"`
		Merges   []any  `json:"merges"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode control response: %v\n%s", err, stdout.String())
	}
	if response.Protocol != controlProtocol || response.Type != "snapshot" || response.TeamID == "" || len(response.Agents) != 1 {
		t.Fatalf("unexpected control response: %+v", response)
	}
	if response.Tasks == nil || response.Views == nil || response.Merges == nil {
		t.Fatalf("control snapshot omitted durable projections: %+v", response)
	}
}

func TestControlEventsPagesOneTeamWithNextCursor(t *testing.T) {
	cliATEEnvironment(t)
	ctx := context.Background()
	workspace, dataDirectory := t.TempDir(), t.TempDir()
	application, err := app.Open(ctx, cliCommandConfig(t, dataDirectory, workspace))
	if err != nil {
		t.Fatal(err)
	}
	// The other Team's events come first, so a limit applied before the
	// Team filter would return empty pages.
	if _, _, err := application.EnsureRootThread(ctx, session.ID("other-session")); err != nil {
		_ = application.Close()
		t.Fatal(err)
	}
	root, _, err := application.EnsureRootThread(ctx, session.ID("paged-session"))
	if err != nil {
		_ = application.Close()
		t.Fatal(err)
	}
	want, err := application.Orchestration().LoadTeam(ctx, root.TeamID, 0, 0)
	if closeErr := application.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if len(want) == 0 {
		t.Fatal("root Team has no events")
	}
	var got []uint64
	after := uint64(0)
	for range len(want) + 1 {
		var stdout, stderr bytes.Buffer
		input := strings.NewReader(fmt.Sprintf("{\"command\":\"events\",\"team_id\":%q,\"limit\":1,\"after\":%d}\n", root.TeamID, after))
		if err := runControl(ctx, []string{"--data-dir", dataDirectory, "--workspace", workspace}, input, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(&stdout)
		var page struct {
			Count      int    `json:"count"`
			NextCursor uint64 `json:"next_cursor"`
		}
		for decoder.More() {
			var line struct {
				Type   string          `json:"type"`
				Cursor uint64          `json:"cursor"`
				TeamID string          `json:"team_id"`
				Data   json.RawMessage `json:"data"`
			}
			if err := decoder.Decode(&line); err != nil {
				t.Fatal(err)
			}
			if line.Type == "result" {
				if err := json.Unmarshal(line.Data, &page); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if line.TeamID != string(root.TeamID) {
				t.Fatalf("event of Team %s in a %s page", line.TeamID, root.TeamID)
			}
			got = append(got, line.Cursor)
		}
		if page.Count == 0 {
			if page.NextCursor != after {
				t.Fatalf("empty page moved the cursor from %d to %d", after, page.NextCursor)
			}
			break
		}
		after = page.NextCursor
	}
	if len(got) != len(want) || got[0] != want[0].GlobalSequence || got[len(got)-1] != want[len(want)-1].GlobalSequence {
		t.Fatalf("paged cursors = %v, want %d events of the Team", got, len(want))
	}
}

func TestControlTeamCancelUntilTheSessionReopens(t *testing.T) {
	cliATEEnvironment(t)
	ctx := context.Background()
	workspace, dataDirectory := t.TempDir(), t.TempDir()
	cfg := cliCommandConfig(t, dataDirectory, workspace)
	application, err := app.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := application.EnsureRootThread(ctx, session.ID("cancel-session"))
	if closeErr := application.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	var stdout, stderr bytes.Buffer
	input := strings.NewReader(fmt.Sprintf("{\"command\":\"team.cancel\",\"team_id\":%q,\"reason\":\"stop\",\"timeout_ms\":1000}\n", root.TeamID))
	if err := runControl(ctx, []string{"--data-dir", dataDirectory, "--workspace", workspace}, input, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Type string `json:"type"`
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Type != "result" || result.Data.State != "canceled" {
		t.Fatalf("team.cancel response = %s, err=%v", stdout.String(), err)
	}
	application, err = app.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	reopened, _, err := application.EnsureRootThread(ctx, session.ID("cancel-session"))
	if closeErr := application.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if reopened.TeamID != root.TeamID || reopened.TeamState != "active" {
		t.Fatalf("reopened root = %#v", reopened)
	}
}
