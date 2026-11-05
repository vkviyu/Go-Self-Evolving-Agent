package pluginbuild

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
)

// BuildStore owns immutable proposal reservations and their durable receipts.
// It is deliberately independent of SQLite and of the executable registry.
type BuildStore interface {
	BeginPluginBuild(context.Context, Request) (string, *Result, error)
	FinishPluginBuild(context.Context, Result) error
	PluginBuild(context.Context, string) (Request, *Result, error)
}

type SourceCompiler interface {
	Compile(context.Context, string) ([]byte, BuildEvidence, error)
}

type Stager interface {
	Stage(context.Context, plugin.Manifest, []byte) (*plugin.Artifact, error)
}

type Verifier interface {
	Verify(context.Context, string, string, []plugin.TestCase) (plugin.TestReport, string, error)
}

var (
	_ SourceCompiler = Compiler{}
	_ Stager         = (*plugin.Manager)(nil)
	_ Verifier       = (*plugin.PublicationService)(nil)
)

type ServiceConfig struct {
	Store    BuildStore
	Compiler SourceCompiler
	Stager   Stager
	Verifier Verifier
}

// Service owns the proposal workflow and its single-worker admission gate.
// The process owner supplies cancellation and joins workers before shutdown.
// There is intentionally no activation or approval-bypass dependency here.
type Service struct {
	store    BuildStore
	compiler SourceCompiler
	stager   Stager
	verifier Verifier
	gate     chan struct{}
}

func NewService(config ServiceConfig) (*Service, error) {
	if config.Store == nil || config.Compiler == nil || config.Stager == nil || config.Verifier == nil {
		return nil, errors.New("plugin build service requires store, compiler, stager and verifier")
	}
	return &Service{
		store:    config.Store,
		compiler: config.Compiler,
		stager:   config.Stager,
		verifier: config.Verifier,
		gate:     make(chan struct{}, 1),
	}, nil
}

// Build creates a tested candidate, never a published tool. Failures with a
// BuildID retain diagnostics so Eino can propose a new immutable revision.
func (service *Service) Build(ctx context.Context, request Request) (result Result, err error) {
	// Do not inherit the longer, general-purpose WASM timeout.
	if request.Manifest.Timeout == 0 {
		request.Manifest.Timeout = 5 * time.Second
	}
	if err := request.Validate(); err != nil {
		return result, err
	}
	select {
	case service.gate <- struct{}{}:
		defer func() { <-service.gate }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	id, completed, err := service.store.BeginPluginBuild(ctx, request)
	if err != nil {
		return result, err
	}
	if completed != nil {
		return *completed, nil
	}
	result.BuildID, result.State = id, "failed"
	defer func() {
		if err != nil {
			result.Error = fmt.Sprintf("%.4096s", err.Error())
		}
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if persistErr := service.store.FinishPluginBuild(recordCtx, result); persistErr != nil {
			err = errors.Join(err, ErrEvidence, persistErr)
		}
	}()
	artifact, evidence, buildErr := service.compiler.Compile(ctx, request.Source)
	result.Build = evidence
	if buildErr != nil {
		return result, buildErr
	}
	if _, err = service.stager.Stage(ctx, request.Manifest, artifact); err != nil {
		return result, err
	}
	report, reportID, err := service.verifier.Verify(ctx, request.Manifest.Tool.Name, request.Manifest.Tool.Revision, request.Cases)
	result.TestReport, result.TestReportID = &report, reportID
	if err != nil {
		return result, err
	}
	if !report.Passed {
		result.State = "quarantined"
		return result, nil
	}
	result.State = "awaiting_approval"
	return result, nil
}

func (service *Service) Inspect(ctx context.Context, id string) (Request, *Result, error) {
	return service.store.PluginBuild(ctx, id)
}
