package experience

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/model"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
)

func actionsModel(t *testing.T, p *testProcess, actions ...action) Generate {
	return func(_ context.Context, _ []*schema.Message) (*schema.Message, error) {
		t.Helper()
		if p.leased.Load() {
			t.Fatal("organization model inside lease")
		}
		raw, _ := json.Marshal(organization{Actions: actions})
		return schema.AssistantMessage(string(raw), nil), nil
	}
}

func TestNativeCrossDomainOrganizationAndOriginalPreservation(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	for _, session := range []string{"network-project", "database-project"} {
		f.events(t, session, 1)
		if err := f.s.Complete(t.Context(), session, "turn", m.generate); err != nil {
			t.Fatal(err)
		}
	}
	d, err := f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	origin := d.Spaces[0].ID
	target := model.SpaceID("reliability-domain")
	body, err := spacecatalog.EncodeApply(spacecatalog.Apply{Name: "Reliability", Description: "Cross-domain learning"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.commit(t.Context(), "new-domain", api.CommitRequest{Space: target, Method: api.MethodApplySpace, Body: body}); err != nil {
		t.Fatal(err)
	}
	cat, err := f.s.retrieve(t.Context(), []model.SpaceID{origin, target}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Memories) != 2 {
		t.Fatalf("expected two originals: %d", len(cat.Memories))
	}
	err = f.s.organize(t.Context(), "cross-domain", cat, actionsModel(t, f.p,
		action{Kind: "attach", SpaceID: string(target), SourceID: cat.Memories[0].ID, Description: "Foreign origin retained"},
		action{Kind: "relate", SpaceID: string(target), SourceID: cat.Memories[0].ID, ToID: cat.Memories[1].ID, RelationKind: "generalizes", Description: "Related recovery observations"},
		action{Kind: "rename", SpaceID: string(target), Name: "Reliability across domains", Description: "Shared network and database recovery lessons"},
	))
	if err != nil {
		t.Fatal(err)
	}
	// No local originals and deliberately no lexical matches. Organization
	// entries and relation endpoints still recover the foreign originals.
	organized, err := f.s.retrieve(t.Context(), []model.SpaceID{target}, "unmatched-text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(organized.Memories) != 2 || len(organized.Entries[target]) != 1 || len(organized.Relations[target]) != 1 {
		t.Fatalf("foreign references not retrieved: %+v", organized)
	}
	for _, item := range organized.Memories {
		if item.Source.Anchor.Space != origin {
			t.Fatal("organization copied or rebound source provenance")
		}
	}
	err = f.s.organize(t.Context(), "remove-organization", organized, actionsModel(t, f.p,
		action{Kind: "remove", SpaceID: string(target), TargetID: organized.Entries[target][0].ID},
		action{Kind: "remove_relation", SpaceID: string(target), TargetID: organized.Relations[target][0].ID},
		action{Kind: "archive", SpaceID: string(target)},
	))
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.s.retrieve(t.Context(), []model.SpaceID{origin, target}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Memories) != 2 || len(after.Entries[target]) != 0 || len(after.Relations[target]) != 0 {
		t.Fatal("organization removal changed original memories")
	}
	d, err = f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, space := range d.Spaces {
		if space.ID == target {
			found = true
			if !space.Archived {
				t.Fatal("archive not applied")
			}
		}
	}
	if !found {
		t.Fatal("archived semantic space improperly filtered")
	}
}

func TestOrganizationCASConflictDoesNotReselect(t *testing.T) {
	f := newFixture(t)
	r := route{NewSpace: &newSpace{Name: "Original", Description: "Original description"}}
	if err := f.s.applyRoute(t.Context(), "cas", r); err != nil {
		t.Fatal(err)
	}
	id := routeSpaces("cas", r)[0]
	cat, err := f.s.retrieve(t.Context(), []model.SpaceID{id}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := spacecatalog.EncodeApply(spacecatalog.Apply{ExpectedVersion: cat.Spaces[0].Version, Name: "Concurrent name", Description: "Concurrent edit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.commit(t.Context(), "concurrent", api.CommitRequest{Space: id, Method: api.MethodApplySpace, Body: body}); err != nil {
		t.Fatal(err)
	}
	generate := actionsModel(t, f.p, action{Kind: "rename", SpaceID: string(id), Name: "Stale proposal", Description: "Must not overwrite"})
	if err = f.s.organize(t.Context(), "cas-conflict", cat, generate); err != nil {
		t.Fatalf("definitive conflict left pending: %v", err)
	}
	var blocked OrganizationActionResult
	if found, err := f.s.load(t.Context(), "cas-conflict/organize/action/0/result", &blocked); err != nil || !found || blocked.Status != "blocked" {
		t.Fatalf("conflict not reported explicitly: %+v %v", blocked, err)
	}
	commits := f.p.commits
	if err = f.s.organize(t.Context(), "cas-conflict", cat, nil); err != nil || f.p.commits != commits {
		t.Fatal("terminal blocked action retried or rebased")
	}
	err = f.b.WithClient(t.Context(), func(c *client.Client) error {
		result, err := c.GetSpace(t.Context(), api.GetSpaceRequest{Space: id})
		if err == nil && result.Space.Name != "Concurrent name" {
			t.Fatal("concurrent metadata overwritten")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCurrentSupersessionBlocksExactPendingSelection(t *testing.T) {
	f := newFixture(t)
	m := newModel(t, f)
	f.events(t, "source", 1)
	if err := f.s.Complete(t.Context(), "source", "turn", m.generate); err != nil {
		t.Fatal(err)
	}
	value, err := f.s.Prepare(t.Context(), "target", "turn", "retry", m.generate)
	if err != nil {
		t.Fatal(err)
	}
	selected := value.Memories[0].Source
	if err = f.s.applyMemory(t.Context(), "supersede", selected.Anchor.Space, memory.Command{Transition: &memory.TransitionCommand{ID: selected.SourceID, To: memory.StatusSuperseded, Why: "newer information"}}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Check(t.Context(), value); err == nil {
		t.Fatal("superseded historical version accepted for current dispatch")
	}
	if result, err := f.s.Prepare(t.Context(), "target", "turn", "retry", nil); err == nil || result.Text != "" {
		t.Fatal("failed pending gate exposed stale context")
	}
}

func organizationFixture(t *testing.T) (*fixture, catalog) {
	t.Helper()
	f := newFixture(t)
	f.events(t, "organization-source", 1)
	if err := f.s.Complete(t.Context(), "organization-source", "publish", newModel(t, f).generate); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cat, err := f.s.retrieve(t.Context(), []model.SpaceID{d.Spaces[0].ID}, "", nil)
	if err != nil || len(cat.Memories) != 1 {
		t.Fatalf("missing native memory fixture: %v", err)
	}
	return f, cat
}

func TestOrganizationCorrectsBareDigestBeforeAnyMutation(t *testing.T) {
	f, cat := organizationFixture(t)
	const key = "correct-source"
	commits, calls := f.p.commits, 0
	var originalInput string
	generate := func(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
		calls++
		if f.p.leased.Load() || f.p.commits != commits {
			t.Fatal("model correction ran inside a lease or after an invalid mutation")
		}
		if _, found, err := f.state.Get(ctx, key+"/organize/decision"); err != nil || found {
			t.Fatal("unvalidated decision was persisted")
		}
		if calls == 1 {
			originalInput = messages[1].Content
		} else if messages[1].Content != originalInput || !strings.Contains(messages[0].Content, "invalid supplied_source") {
			t.Fatal("correction lost original input or safe identity feedback")
		}
		var input organizationInput
		if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
			t.Fatal(err)
		}
		if len(input.Memories) != 1 || input.Memories[0].SourceID != cat.Memories[0].ID ||
			input.Memories[0].Claim != string(cat.Memories[0].Memory.Claim) {
			t.Fatal("model input lost canonical identity or readable claim")
		}
		var fields struct {
			Memories []map[string]json.RawMessage `json:"memories"`
		}
		if err := json.Unmarshal([]byte(messages[1].Content), &fields); err != nil {
			t.Fatal(err)
		}
		for name := range fields.Memories[0] {
			switch name {
			case "source_id", "space_id", "claim", "kind", "status", "tags":
			default:
				t.Fatalf("ambiguous native memory field exposed: %s", name)
			}
		}
		source := input.Memories[0].SourceID
		if calls == 1 {
			source = string(cat.Memories[0].Memory.ID)
		}
		// A valid first action must not execute if a later source is invalid.
		return actionsModel(t, f.p,
			action{Kind: "rename", SpaceID: input.Spaces[0].SpaceID, Name: "Corrected organization"},
			action{Kind: "attach", SpaceID: input.Spaces[0].SpaceID, SourceID: source, Description: "Useful grounded lesson"},
		)(ctx, messages)
	}
	if err := f.s.organize(t.Context(), key, cat, generate); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || f.p.commits != commits+2 {
		t.Fatalf("wrong correction/mutation count: calls=%d commits=%d", calls, f.p.commits-commits)
	}
	err := f.b.WithClient(t.Context(), func(c *client.Client) error {
		entries, err := c.ListSpaceMemories(t.Context(), api.ListSpaceMemoriesRequest{Space: cat.Spaces[0].ID})
		if err == nil && (len(entries.Items) != 1 || !reflect.DeepEqual(entries.Items[0].Source, cat.Memories[0].Source)) {
			t.Fatal("corrected selection did not preserve exact native source anchor")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	commits = f.p.commits
	// Even an empty replacement catalog cannot change the saved mapping.
	if err = f.s.organize(t.Context(), key, catalog{}, nil); err != nil || f.p.commits != commits || calls != 2 {
		t.Fatalf("validated replay regenerated or remapped the decision: %v", err)
	}
}

func TestOrganizationInvalidOutputBudgetSurvivesReplay(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "bare-source"
		if malformed {
			name = "malformed-json"
		}
		t.Run(name, func(t *testing.T) {
			f, cat := organizationFixture(t)
			calls, commits := 0, f.p.commits
			generate := func(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
				calls++
				if calls > 2 || f.p.commits != commits {
					t.Fatal("invalid output triggered extra attempts or mutations")
				}
				if malformed {
					return schema.AssistantMessage(`{"actions":[`, nil), nil
				}
				return actionsModel(t, f.p, action{Kind: "attach", SpaceID: string(cat.Spaces[0].ID),
					SourceID: string(cat.Memories[0].Memory.ID)})(ctx, messages)
			}
			if err := f.s.organize(t.Context(), name, cat, generate); !errors.Is(err, ErrDecision) || calls != 2 {
				t.Fatalf("invalid decision was accepted or budget exceeded: %v calls=%d", err, calls)
			}
			if _, found, err := f.state.Get(t.Context(), name+"/organize/decision"); err != nil || found {
				t.Fatal("invalid output persisted as decision")
			}
			f.restart(t)
			if err := f.s.organize(t.Context(), name, cat, generate); !errors.Is(err, ErrDecision) || calls != 2 || f.p.commits != commits {
				t.Fatalf("replay reset the exhausted correction budget: %v calls=%d", err, calls)
			}
		})
	}
}

func TestOrganizationProviderErrorDoesNotTriggerCorrectiveRetry(t *testing.T) {
	f, cat := organizationFixture(t)
	stop := errors.New("provider unavailable")
	calls, commits := 0, f.p.commits
	err := f.s.organize(t.Context(), "provider-error", cat, func(context.Context, []*schema.Message) (*schema.Message, error) {
		calls++
		return nil, stop
	})
	if !errors.Is(err, stop) || calls != 1 || f.p.commits != commits {
		t.Fatalf("provider error was retried as structured output: %v calls=%d", err, calls)
	}
}
