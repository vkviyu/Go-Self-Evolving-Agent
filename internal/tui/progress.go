package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
)

type (
	storageProgressMsg struct {
		progress ateprocess.Progress
	}
)

// observeStorage forwards metadata through the same ordered UI queue as turn
// events. Background activity that lacks this context cannot replace the stage.
func observeStorage(ctx, lifetime context.Context, events chan<- tea.Msg) context.Context {
	return ateprocess.WithObserver(ctx, func(_ context.Context, progress ateprocess.Progress) {
		select {
		case events <- storageProgressMsg{progress: progress}:
		case <-ctx.Done():
		case <-lifetime.Done():
		}
	})
}

// resetProgress starts timing a new foreground operation.
func (model *Model) resetProgress() {
	model.stageStartedAt = time.Now()
	model.storageProgress = nil
}

// applyStorageProgress tracks batches independently so completing a concurrent
// batch cannot clear another batch's wait indicator.
func (model *Model) applyStorageProgress(message storageProgressMsg) {
	event := message.progress
	if !model.busy {
		return
	}
	if event.Stage == ateprocess.StageDone {
		delete(model.storageProgress, event.BatchID)
		return
	}
	if model.storageProgress == nil {
		model.storageProgress = make(map[uint64]storageProgressMsg)
	}
	if previous, ok := model.storageProgress[event.BatchID]; ok && previous.progress.Stage == event.Stage {
		return
	}
	model.storageProgress[event.BatchID] = message
}

// progressLabel keeps the caller's operation visible while exposing its current
// storage wait. Streaming deltas do not reset the timer for an unchanged stage.
func (model *Model) progressLabel(now time.Time) string {
	label := model.stage
	if !model.stageStartedAt.IsZero() {
		label += " · " + formatElapsed(max(0, now.Sub(model.stageStartedAt)))
	}
	var latest ateprocess.Progress
	progress := model.storageProgress
	if model.startup != nil {
		if phase := startupPhaseLabel(model.startupPhase); phase != "" {
			label += " · " + phase + " " + formatElapsed(max(0, now.Sub(model.startupPhaseAt)))
		}
		progress = model.startupStorage
	}
	for _, message := range progress {
		if message.progress.At.After(latest.At) {
			latest = message.progress
		}
	}
	if detail := storageStageLabel(latest.Stage); detail != "" {
		label += " · " + detail + " " + formatElapsed(max(0, now.Sub(latest.At)))
	}
	return label
}

// storageStageLabel uses trusted fixed labels, never native protocol text.
func storageStageLabel(stage ateprocess.Stage) string {
	switch stage {
	case ateprocess.StageQueue:
		return "ATE waiting in queue"
	case ateprocess.StageOwnership:
		return "ATE waiting for lock"
	case ateprocess.StageInitialize:
		return "ATE checking store"
	case ateprocess.StageStart:
		return "ATE starting"
	case ateprocess.StageHandshake:
		return "ATE opening store / handshake"
	case ateprocess.StageOperation:
		return "ATE executing operation"
	case ateprocess.StageShutdown:
		return "ATE closing"
	default:
		return ""
	}
}
