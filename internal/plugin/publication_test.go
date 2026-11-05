package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

type publicationReports struct {
	report TestReport
	err    error
	reads  int
	saves  int
}

func (r *publicationReports) SavePluginTest(context.Context, TestReport) (string, error) {
	r.saves++
	return "report-id", r.err
}

func (r *publicationReports) PluginTest(_ context.Context, id string) (TestReport, error) {
	r.reads++
	if id != "report-id" {
		return TestReport{}, errors.New("unexpected report ID")
	}
	return r.report, r.err
}

// An already-active artifact exercises the publication gates and idempotent
// replay without executing WASM. Real activation is covered by integration tests.
func publicationFixture(t *testing.T) (*PublicationService, *publicationReports) {
	t.Helper()
	registry := tool.NewRegistry()
	manager := &Manager{registry: registry, artifacts: make(map[string]*Artifact)}
	artifact := &Artifact{owner: manager, state: StateActive, contentSHA: "artifact-sha"}
	manager.artifacts[artifactKey("extension_echo", "v1")] = artifact
	reports := &publicationReports{report: TestReport{
		Tool: "extension_echo", Revision: "v1", ContentSHA256: "artifact-sha", RuntimeIdentity: "runtime", Passed: true,
	}}
	service, err := NewPublicationService(manager, registry, reports, "runtime")
	if err != nil {
		t.Fatal(err)
	}
	return service, reports
}

func TestPublicationRequiresConfirmationAndEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, reportID string
		confirmed      bool
	}{
		{"unconfirmed", "report-id", false},
		{"missing report", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, reports := publicationFixture(t)
			if _, err := service.Promote(context.Background(), "extension_echo", "v1", tc.reportID, tc.confirmed); err == nil || reports.reads != 0 {
				t.Fatalf("missing authority accessed reports: reads=%d err=%v", reports.reads, err)
			}
		})
	}
}

func TestPublicationRejectsMismatchedEvidence(t *testing.T) {
	for _, field := range []string{"passed", "tool", "revision", "content", "runtime"} {
		t.Run(field, func(t *testing.T) {
			service, reports := publicationFixture(t)
			switch field {
			case "passed":
				reports.report.Passed = false
			case "tool":
				reports.report.Tool = "extension_other"
			case "revision":
				reports.report.Revision = "v2"
			case "content":
				reports.report.ContentSHA256 = "other-bytes"
			case "runtime":
				reports.report.RuntimeIdentity = "other-runtime"
			}
			before := service.registry.Snapshot()
			if _, err := service.Promote(context.Background(), "extension_echo", "v1", "report-id", true); err == nil {
				t.Fatal("accepted mismatched or failing evidence")
			}
			if service.registry.Snapshot() != before || reports.reads != 1 {
				t.Fatal("failed promotion changed registry or reread evidence")
			}
		})
	}
}

func TestPublicationReplaysActiveArtifact(t *testing.T) {
	service, reports := publicationFixture(t)
	before := service.registry.Snapshot()
	snapshot, err := service.Promote(context.Background(), "extension_echo", "v1", "report-id", true)
	if err != nil || snapshot != before || reports.reads != 1 || reports.saves != 0 {
		t.Fatalf("active publication was not idempotent: %v", err)
	}
}

func TestPublicationPropagatesMissingEvidenceAndArtifact(t *testing.T) {
	service, reports := publicationFixture(t)
	diskErr := errors.New("report unavailable")
	reports.err = diskErr
	if _, err := service.Promote(context.Background(), "extension_echo", "v1", "report-id", true); !errors.Is(err, diskErr) {
		t.Fatalf("report error = %v", err)
	}
	reports.err = nil
	if _, err := service.Promote(context.Background(), "extension_missing", "v1", "report-id", true); !errors.Is(err, tool.ErrToolNotFound) {
		t.Fatalf("missing artifact error = %v", err)
	}
}

func TestPublicationDoesNotSaveIncompleteVerification(t *testing.T) {
	service, reports := publicationFixture(t)
	// Invalid cases fail before invoking the fixture's intentionally absent guest.
	if _, id, err := service.Verify(context.Background(), "extension_echo", "v1", nil); err == nil || id != "" || reports.saves != 0 {
		t.Fatalf("incomplete evidence saved: id=%s saves=%d err=%v", id, reports.saves, err)
	}
}

func TestPublicationRequiresEveryDependency(t *testing.T) {
	for _, missing := range []string{"manager", "registry", "reports", "runtime"} {
		t.Run(missing, func(t *testing.T) {
			service, _ := publicationFixture(t)
			switch missing {
			case "manager":
				service.manager = nil
			case "registry":
				service.registry = nil
			case "reports":
				service.reports = nil
			case "runtime":
				service.runtimeIdentity = ""
			}
			if _, err := NewPublicationService(service.manager, service.registry, service.reports, service.runtimeIdentity); err == nil {
				t.Fatal("accepted missing dependency")
			}
		})
	}
}
