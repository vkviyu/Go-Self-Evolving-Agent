package experience

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/agent-temporal-engine/api"
	"github.com/vkviyu/agent-temporal-engine/client"
	"github.com/vkviyu/agent-temporal-engine/memory"
	"github.com/vkviyu/agent-temporal-engine/spacecatalog"
	"github.com/vkviyu/go-self-evolving-agent/internal/atebridge"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

func TestSuccessiveFullSessionExtractionReusesOriginalPublication(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprint(rejected), func(t *testing.T) {
			f := newFixture(t)
			f.events(t, "repeat", 1)
			m := newModel(t, f)
			m.reject = rejected
			if err := f.s.Complete(t.Context(), "repeat", "first", m.generate); err != nil {
				t.Fatal(err)
			}
			before, err := f.s.publishedLessons(t.Context(), "repeat")
			if err != nil || len(before) != 1 || before[0].Source == nil {
				t.Fatalf("missing anchored publication: %+v %v", before, err)
			}
			wantStatus := string(memory.StatusPromoted)
			if rejected {
				wantStatus = string(memory.StatusCandidate)
			}
			if before[0].Status != wantStatus {
				t.Fatalf("wrong publication status: %s", before[0].Status)
			}
			f.restart(t)
			if _, err = f.history.Append(t.Context(), "repeat", session.AnySequence,
				session.PendingEvent{Type: session.EventUserMessage, Payload: json.RawMessage(`{"content":"continue"}`)}); err != nil {
				t.Fatal(err)
			}
			compared := false
			generate := func(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
				reply, err := m.generate(ctx, messages)
				if strings.Contains(messages[0].Content, "Stage: extract") {
					var input struct{ Published []publishedLesson }
					if err := json.Unmarshal([]byte(messages[1].Content), &input); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(input.Published, before) {
						t.Fatal("extractor did not receive original session publications")
					}
					compared = true
					// Ignore semantic comparison to test the deterministic guard.
					var extracted extraction
					if err := json.Unmarshal([]byte(reply.Content), &extracted); err != nil {
						t.Fatal(err)
					}
					extracted.Lessons[0].Claim = " \n" + strings.ReplaceAll(extracted.Lessons[0].Claim, " ", "  ") + "\t"
					raw, _ := json.Marshal(extracted)
					reply = schema.AssistantMessage(string(raw), nil)
				}
				return reply, err
			}
			commits := f.p.commits
			if err = f.s.Complete(t.Context(), "repeat", "second", generate); err != nil {
				t.Fatal(err)
			}
			after, err := f.s.publishedLessons(t.Context(), "repeat")
			if err != nil || !reflect.DeepEqual(before, after) || !compared || m.calls["verify"] != 1 || f.p.commits != commits {
				t.Fatalf("duplicate publication/review: before=%+v after=%+v verify=%d commits=%d/%d err=%v",
					before, after, m.calls["verify"], commits, f.p.commits, err)
			}
			if err = f.s.Complete(t.Context(), "repeat", "second", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublicationUnknownOutcomeAcrossTurnsUsesOriginalOperation(t *testing.T) {
	f := newFixture(t)
	f.events(t, "interrupted", 1)
	m := newModel(t, f)
	f.p.dropMethod = api.MethodApplyMemory
	if err := f.s.Complete(t.Context(), "interrupted", "first", m.generate); err == nil {
		t.Fatal("missing injected candidate publication failure")
	}
	entries, err := f.s.publishedLessons(t.Context(), "interrupted")
	if err != nil || len(entries) != 1 {
		t.Fatal("intent not durable before candidate write")
	}
	var original publicationIntent
	if _, err = f.s.load(t.Context(), entries[0].Publication+"/intent", &original); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if err = f.s.Complete(t.Context(), "interrupted", "second", m.generate); err != nil {
		t.Fatal(err)
	}
	after, err := f.s.publishedLessons(t.Context(), "interrupted")
	if err != nil || len(after) != 1 || after[0].Status != string(memory.StatusPromoted) {
		t.Fatalf("interrupted publication did not converge: %+v %v", after, err)
	}
	var replay publicationIntent
	if _, err = f.s.load(t.Context(), entries[0].Publication+"/intent", &replay); err != nil || !reflect.DeepEqual(original, replay) {
		t.Fatal("uncertain mutation moved to a new operation key")
	}
	if err = f.s.Complete(t.Context(), "interrupted", "first", m.generate); err != nil {
		t.Fatal(err)
	}
	if m.calls["verify"] != 1 {
		t.Fatal("repeated review of the same publication")
	}
}

func TestCompleteReportsBlockedOrganizationAndAllowsFreshNextTurn(t *testing.T) {
	f := newFixture(t)
	f.events(t, "blocked", 1)
	m := newModel(t, f)
	organizeCalls := 0
	generate := func(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
		if !strings.Contains(messages[0].Content, "Stage: organize") {
			return m.generate(ctx, messages)
		}
		var cat organizationInput
		if err := json.Unmarshal([]byte(messages[1].Content), &cat); err != nil {
			t.Fatal(err)
		}
		organizeCalls++
		if organizeCalls == 1 {
			var native catalog
			if found, err := f.s.load(ctx, f.s.key("blocked", "first")+"/complete/organization-catalog", &native); err != nil || !found || len(native.Spaces) == 0 {
				t.Fatalf("missing pinned native catalog: %v", err)
			}
			body, _ := spacecatalog.EncodeApply(spacecatalog.Apply{ExpectedVersion: native.Spaces[0].Version,
				Name: "Concurrent owner edit", Description: "Must survive stale maintenance"})
			if _, err := f.s.commit(ctx, "concurrent-maintenance", api.CommitRequest{
				Space: native.Spaces[0].ID, Method: api.MethodApplySpace, Body: body,
			}); err != nil {
				t.Fatal(err)
			}
		}
		return actionsModel(t, f.p, action{Kind: "rename", SpaceID: cat.Spaces[0].SpaceID, Name: "Fresh proposal"})(ctx, messages)
	}
	if err := f.s.Complete(t.Context(), "blocked", "first", generate); err != nil {
		t.Fatal(err)
	}
	report, err := f.s.MaintenanceReport(t.Context(), "blocked", "first")
	if err != nil || !report.Complete || len(report.Actions) != 1 || report.Actions[0].Status != "blocked" || report.Actions[0].Reason == "" {
		t.Fatalf("blocked maintenance not terminal and explicit: %+v %v", report, err)
	}
	commits := f.p.commits
	if err = f.s.Complete(t.Context(), "blocked", "first", nil); err != nil || f.p.commits != commits {
		t.Fatal("blocked maintenance retried forever")
	}
	d, err := f.s.directory(t.Context())
	if err != nil || d.Spaces[0].Name != "Concurrent owner edit" {
		t.Fatal("conflicting action was applied")
	}
	if err = f.s.Complete(t.Context(), "blocked", "second", generate); err != nil {
		t.Fatal(err)
	}
	report, err = f.s.MaintenanceReport(t.Context(), "blocked", "second")
	if err != nil || !report.Complete || len(report.Actions) != 1 || report.Actions[0].Status != "applied" || organizeCalls != 2 {
		t.Fatalf("fresh turn could not make a new decision: %+v %v", report, err)
	}
}

func TestOrganizationUnknownNeverBecomesTerminalBlock(t *testing.T) {
	rejected := api.CommitResult{Code: api.CodeRejected}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, atebridge.ErrOutcomeUnknown,
		&client.OutcomeUnknownError{Cause: errors.New("lost response")}} {
		if definitiveOrganizationRejection(rejected, err) {
			t.Fatalf("uncertain outcome treated as rejection: %v", err)
		}
	}
	for _, code := range []api.ResultCode{api.CodeCommitUnknown, api.CodeWaitInterrupted, api.CodeInFlight, api.CodeIndeterminate, api.CodeStoreFenced} {
		if definitiveOrganizationRejection(api.CommitResult{Code: code}, errors.New("unknown")) {
			t.Fatalf("uncertain result treated as rejection: %s", code)
		}
	}
}
