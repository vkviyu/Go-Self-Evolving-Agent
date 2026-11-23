package agentruntime

import (
	"context"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/experienceport"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/strategy"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenizer"
	"github.com/vkviyu/go-self-evolving-agent/internal/tokenusage"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Config controls stable Agent behavior independent of any model provider.
type Config struct {
	Name        string
	Description string
	Instruction string
	// HostInstruction describes capabilities shared by root and child profiles.
	HostInstruction         string
	MaxIterations           int
	MaxOutputTokens         int
	ContextWindowTokens     int
	CompactionTriggerTokens int
	ContextWindowSource     modelcontext.Source
	Tokenizer               tokenizer.Counter
	ActorScope              orchestration.ActorScope
	Execution               orchestration.ExecutionBinding
	ExecutionContract       orchestration.ExecutionContract
	ModelProfile            string
	Provider                string
	Model                   string
	UsageLedger             tokenusage.Ledger
	Pricing                 tokenusage.Pricer
	// Experience explicitly enables automatic selection and post-turn curation.
	// The application leaves it unset for primary-model memory tool decisions.
	Experience          Experience
	ExperienceLifecycle ExperienceLifecycle
	// ExperienceTimeout bounds post-turn maintenance. Zero uses two minutes.
	// Foreground retry admission is bounded separately; admitted work runs after
	// the turn with this timeout.
	ExperienceTimeout time.Duration
	// ContextArtifacts receives oversized tool observations before they are
	// replaced with content-addressed references in model input.
	ContextArtifacts ContextArtifactStore
	// InspectFileReferences supplies workspace metadata for a turn's opening
	// input, under the same actor and tool snapshot as a normal read.
	InspectFileReferences func(context.Context, *tool.Pipeline, tool.Call, string) ([]FileReference, error)
	ResolveModel          func(context.Context, string) (ModelBinding, error)
	Strategies            strategy.Store
	// StrategyScope binds shadow evidence to host/provider settings not present
	// in the legacy execution identity. It contains no credentials.
	StrategyScope string
}

type ContextArtifactStore interface {
	Put(context.Context, []byte) (string, error)
}

type (
	// Experience owns selection and durable maintenance decisions. Context is
	// persisted in full; Check must authorize its current use on every dispatch.
	// Methods and their generator calls must honor context cancellation.
	// Prepare receives the full UTF-8 opening goal (at most 1 MiB). The service
	// must ensure typed execution readiness before fallible routing/model work,
	// so the failure lifecycle can synchronize and close terminal executions.
	Experience interface {
		Prepare(ctx context.Context, sessionID, turnID, goal string, generate experienceport.Generate) (experienceport.Context, error)
		Check(context.Context, experienceport.Context) error
		Complete(ctx context.Context, sessionID, turnID string, generate experienceport.Generate) error
	}

	// ExperienceSync is optionally implemented by the host's Experience wrapper.
	// Sync must honor cancellation and publish source events before dispatch,
	// at suspension, and after maintenance.
	ExperienceSync interface {
		Sync(context.Context) error
	}

	// PendingExperienceMaintenanceStore is an optional host-authorized lookup
	// of latest pending events across inactive sessions. Return at most limit
	// events, including SessionID and Sequence, with due jobs first. The runtime
	// revalidates and claims an event using optimistic sequence checking.
	PendingExperienceMaintenanceStore interface {
		PendingExperienceMaintenance(ctx context.Context, limit int) ([]session.Event, error)
	}

	// ExperienceLifecycle is an optional host adapter for source synchronization
	// and typed execution outcomes. Recoverable failures must sync evidence but
	// leave the execution open: closing it would conflict with resumed success.
	// Finish must be idempotent and honor context cancellation.
	ExperienceLifecycle interface {
		Finish(ctx context.Context, sessionID, turnID string, outcome ExperienceOutcome) error
	}

	ExperienceOutcome struct {
		Succeeded   bool   `json:"succeeded"`
		Recoverable bool   `json:"recoverable,omitempty"`
		Error       string `json:"error,omitempty"`
	}
)
