package plugin

import (
	"context"
	"errors"

	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// TestReportStore persists proof for an exact revision/runtime, not authority
// to publish it. Confirmation remains a separate host/policy decision.
type TestReportStore interface {
	SavePluginTest(context.Context, TestReport) (string, error)
	PluginTest(context.Context, string) (TestReport, error)
}

// PublicationService shares verification and promotion gates between CLI and
// Agent adapters. The Manager still owns immutable revisions and registry CAS.
type PublicationService struct {
	manager         *Manager
	registry        *tool.Registry
	reports         TestReportStore
	runtimeIdentity string
}

func NewPublicationService(manager *Manager, registry *tool.Registry, reports TestReportStore, runtimeIdentity string) (*PublicationService, error) {
	if manager == nil || registry == nil || reports == nil || runtimeIdentity == "" {
		return nil, errors.New("plugin publication requires manager, registry, report store and runtime identity")
	}
	return &PublicationService{
		manager:         manager,
		registry:        registry,
		reports:         reports,
		runtimeIdentity: runtimeIdentity,
	}, nil
}

func (service *PublicationService) Verify(ctx context.Context, name, revision string, cases []TestCase) (TestReport, string, error) {
	report, err := service.manager.Verify(ctx, name, revision, cases)
	if err != nil {
		return report, "", err
	}
	id, err := service.reports.SavePluginTest(ctx, report)
	return report, id, err
}

func (service *PublicationService) Promote(ctx context.Context, name, revision, reportID string, confirmed bool) (*tool.Snapshot, error) {
	if !confirmed || reportID == "" {
		return nil, errors.New("promotion requires a reviewed test report and explicit confirmation")
	}
	report, err := service.reports.PluginTest(ctx, reportID)
	if err != nil {
		return nil, err
	}
	artifact, found := service.manager.Artifact(name, revision)
	if !found {
		return nil, tool.ErrToolNotFound
	}
	if !report.Passed || report.Tool != name || report.Revision != revision || report.ContentSHA256 != artifact.ContentSHA256() || report.RuntimeIdentity != service.runtimeIdentity {
		return nil, errors.New("test report is failing or belongs to a different artifact/runtime")
	}
	if artifact.State() == StateActive {
		return service.registry.Snapshot(), nil
	}
	return service.manager.Activate(artifact)
}
