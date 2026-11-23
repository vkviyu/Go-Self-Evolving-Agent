package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/agentmanager"
	"github.com/vkviyu/go-self-evolving-agent/internal/app"
	"github.com/vkviyu/go-self-evolving-agent/internal/ateprocess"
	"github.com/vkviyu/go-self-evolving-agent/internal/aterequest"
	"github.com/vkviyu/go-self-evolving-agent/internal/config"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelgateway"
	"github.com/vkviyu/go-self-evolving-agent/internal/runtimeowner"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

// TestMain replaces GSEA_ATE_BINARY with its fixture. Preserve the explicit
// diagnostic input before TestMain; never silently substitute a rebuilt binary.
var startupTimingBinary = os.Getenv("GSEA_ATE_BINARY")

type startupTimingModel struct{ calls atomic.Int64 }

func (m *startupTimingModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.calls.Add(1)
	return nil, errors.New("startup diagnostic must not invoke a model")
}

func (m *startupTimingModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls.Add(1)
	return nil, errors.New("startup diagnostic must not invoke a model")
}

func (m *startupTimingModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type startupTimingMeasurement struct {
	elapsed          time.Duration
	starts           int64
	backgroundStarts int64
}

type startupTimingResult struct {
	open, startup startupTimingMeasurement
}

// TestInteractiveStartupTiming measures the local work before interactive
// readiness, excluding provider initialization, TUI rendering, worker dispatch
// and asynchronous catalog registration. Both variants execute the same real
// recovery/maintenance path on independent copies of one existing authority.
// No timing threshold is asserted: process counts and lifecycle success are
// deterministic checks; elapsed time is a diagnostic on this machine/store.
func TestInteractiveStartupTiming(t *testing.T) {
	if os.Getenv("GSEA_TEST_STARTUP_TIMING") != "1" {
		t.Skip("opt-in startup timing; set GSEA_TEST_STARTUP_TIMING=1")
	}
	source := os.Getenv("GSEA_TEST_STARTUP_STORE")
	if source == "" || !filepath.IsAbs(source) {
		t.Fatal("GSEA_TEST_STARTUP_STORE must explicitly name an absolute existing directory")
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal("resolve source:", startupTimingError(err))
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		t.Fatal("source must be an existing directory")
	}
	if !filepath.IsAbs(startupTimingBinary) {
		t.Fatal("GSEA_ATE_BINARY must explicitly name an absolute existing executable")
	}
	if info, err := os.Stat(startupTimingBinary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatal("GSEA_ATE_BINARY must name an existing executable")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	var configs [2]config.Config
	for i := range configs {
		private := t.TempDir()
		// CopyFS must never target source or any descendant of it.
		canonical, err := filepath.EvalSymlinks(private)
		if err != nil {
			t.Fatal("resolve temporary directory:", startupTimingError(err))
		}
		relative, err := filepath.Rel(source, canonical)
		if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			t.Fatal("temporary directory must be outside source")
		}
		workspace := filepath.Join(private, "workspace")
		if err := os.Mkdir(workspace, 0700); err != nil {
			t.Fatal("create temporary workspace:", startupTimingError(err))
		}
		configs[i] = config.Config{
			HomeDirectory:         filepath.Join(private, "home"),
			DataDirectory:         filepath.Join(private, "runtime"),
			WorkspaceDirectory:    workspace,
			CoordinationDirectory: filepath.Join(private, "coordination"),
			ATE: config.ATEConfig{
				Enabled: true, Binary: startupTimingBinary,
				Directory: filepath.Join(private, "temporal"), Timeout: 30 * time.Second,
			},
		}
	}

	// Use exactly the native process manager's ownership domain. Acquire only
	// for copying; never open an App, recover, or launch ATE against source.
	copyStarted := time.Now()
	err = func() (copyErr error) {
		owner, err := runtimeowner.Acquire(source)
		if err != nil {
			return err
		}
		defer func() { copyErr = errors.Join(copyErr, owner.Close()) }()
		for _, cfg := range configs {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Preserve the complete store, including its existing storeID.
			// CopyFS rejects symlinks instead of making writable aliases.
			if err := os.CopyFS(cfg.ATE.Directory, os.DirFS(source)); err != nil {
				return err
			}
		}
		return ctx.Err()
	}()
	t.Logf("copy_two_stores elapsed=%s", time.Since(copyStarted))
	if err != nil {
		t.Fatal("copy source under native ownership:", startupTimingError(err))
	}

	var results [2]startupTimingResult
	var passed [2]bool
	for i, name := range []string{"unbatched", "batched"} {
		// Sequential runs avoid competing for CPU/disk. Each gets the same
		// pre-startup snapshot and enough of the overall budget to report.
		passed[i] = t.Run(name, func(t *testing.T) {
			variantCtx, variantCancel := context.WithTimeout(ctx, 2*time.Minute)
			defer variantCancel()
			results[i] = runStartupTiming(t, variantCtx, configs[i], i == 1)
		})
	}
	if passed[0] && passed[1] {
		before, after := results[0], results[1]
		if before.startup.starts <= after.startup.starts {
			t.Error("batch did not reduce native startup process starts")
		}
		t.Logf("comparison startup_before=%s startup_after=%s startup_speedup=%.2fx total_before=%s total_after=%s",
			before.startup.elapsed, after.startup.elapsed,
			float64(before.startup.elapsed)/float64(after.startup.elapsed),
			before.open.elapsed+before.startup.elapsed, after.open.elapsed+after.startup.elapsed)
	}
}

func runStartupTiming(t *testing.T, ctx context.Context, cfg config.Config, batched bool) startupTimingResult {
	t.Helper()
	var result startupTimingResult
	var starts atomic.Int64
	var backgroundStarts atomic.Int64
	traced := ateprocess.WithObserver(ctx, func(_ context.Context, event ateprocess.Progress) {
		if event.Stage == ateprocess.StageStart {
			starts.Add(1)
		}
	})
	// The Supervisor's wait monitor has a lifetime outside the storage batch.
	// Count its launches too, without passing a batch lease to that worker.
	runContext := ateprocess.WithObserver(ctx, func(_ context.Context, event ateprocess.Progress) {
		if event.Stage == ateprocess.StageStart {
			backgroundStarts.Add(1)
		}
	})
	started := time.Now()
	application, err := app.Open(traced, cfg)
	result.open = startupTimingMeasurement{elapsed: time.Since(started), starts: starts.Load()}
	t.Logf("app_open elapsed=%s native_starts=%d", result.open.elapsed, result.open.starts)
	if err != nil {
		t.Fatal("app.Open:", startupTimingError(err))
	}
	fake := &startupTimingModel{}
	var supervisor *agentmanager.Supervisor
	defer func() {
		started := time.Now()
		// Explicitly exercise Close after the batch context has expired, then
		// App.Close exercises the same idempotent Supervisor lifecycle.
		if supervisor != nil {
			if err := supervisor.Close(); err != nil {
				t.Error("supervisor.Close:", startupTimingError(err))
			}
		}
		if err := application.Close(); err != nil {
			t.Error("app.Close:", startupTimingError(err))
		}
		t.Logf("close elapsed=%s", time.Since(started))
		if fake.calls.Load() != 0 {
			t.Error("startup unexpectedly invoked the fake model")
		}
	}()

	options := chatOptions{
		sessionID:     session.ID("startup-timing-" + rand.Text()),
		model:         modelgateway.Config{Model: "startup-timing-fake"},
		maxIterations: 4, maxOutputTokens: 1024,
		contextWindowTokens: 32768, compactionTriggerTokens: 24576,
		maxRunningAgents: 1,
	}
	approver, err := selectChatApprover(true, false, application.Approval())
	if err != nil {
		t.Fatal("select approver:", startupTimingError(err))
	}
	phase := "ensure_root"
	step := func(name string, work func() error) error {
		phase = name
		started, count := time.Now(), starts.Load()
		err := work()
		t.Logf("%s elapsed=%s native_starts=%d", name, time.Since(started), starts.Load()-count)
		return err
	}
	work := func(batchCtx context.Context) error {
		started, count := time.Now(), starts.Load()
		root, created, err := application.EnsureRootThreadWithOptions(batchCtx, options.sessionID, app.EnsureRootThreadOptions{DeferMaintenance: true})
		t.Logf("ensure_root elapsed=%s native_starts=%d", time.Since(started), starts.Load()-count)
		if err != nil {
			return err
		}
		if !created {
			return errors.New("diagnostic session must be newly created")
		}
		phase = "factory"
		started, count = time.Now(), starts.Load()
		factory, err := application.NewAgentFactory(fake, rootRuntimeConfig(options, root.ActorScope(), application.Orchestration()), approver)
		t.Logf("factory elapsed=%s native_starts=%d", time.Since(started), starts.Load()-count)
		if err != nil {
			return err
		}
		phase = "merge_executor"
		mergeExecutor, err := application.NewMergeExecutor(nil, approver)
		if err != nil {
			return err
		}
		if err := step("supervisor_recovery", func() error {
			supervisor, err = application.NewSupervisorContext(batchCtx, agentmanager.Config{
				ContextSeeder: newChildContextSeeder(factory), Runner: newChildRunner(factory),
				MergeExecutor: mergeExecutor, MaxRunningAgents: options.maxRunningAgents,
				MaxLiveAgents: cfg.Agents.MaxLive, MaxDepth: cfg.Agents.MaxDepth,
				MaxChildren: cfg.Agents.MaxChildren, ModelBucketLimits: cfg.Agents.ModelLimits,
				DefaultWorkspace:      agentmanager.WorkspaceDefault(cfg.Agents.DefaultWorkspace),
				DeferRecoveryDispatch: true,
				RunContext:            runContext, // Never let workers retain the batch lease.
			})
			return err
		}); err != nil {
			return err
		}
		if err := step("root_conversation", func() error {
			chat, err := openRootConversation(batchCtx, factory, supervisor, root)
			if err != nil {
				return err
			}
			if chat.ID() != options.sessionID || chat.CompletedTurns() != 0 || len(chat.History()) != 0 {
				return errors.New("new root conversation is not empty or has wrong identity")
			}
			return nil
		}); err != nil {
			return err
		}
		return step("maintenance", func() error {
			return application.ResumeSessionMaintenance(batchCtx, root.SessionID)
		})
	}
	started, count := time.Now(), starts.Load()
	if batched {
		err = application.WithStorageBatch(traced, work)
	} else {
		err = work(traced)
	}
	result.startup = startupTimingMeasurement{
		elapsed: time.Since(started), starts: starts.Load() - count,
		backgroundStarts: backgroundStarts.Load(),
	}
	t.Logf("startup elapsed=%s native_starts=%d foreground_starts=%d background_starts=%d",
		result.startup.elapsed, result.startup.starts+result.startup.backgroundStarts,
		result.startup.starts, result.startup.backgroundStarts)
	t.Logf("open_plus_startup elapsed=%s native_starts=%d",
		result.open.elapsed+result.startup.elapsed, result.open.starts+result.startup.starts+result.startup.backgroundStarts)
	if err != nil {
		t.Fatalf("startup failed at %s (timing is incomplete): %s", phase, startupTimingError(err))
	}
	if batched && result.startup.starts != 1 {
		t.Errorf("batched startup native starts=%d, want 1", result.startup.starts)
	}
	// Deliberately no StartAvailable*, RunTurn, provider construction or tool
	// invocation. Recovery and mandatory maintenance above remain enabled.
	return result
}

// Error strings can contain source paths, session identifiers or stored data.
// Report every failure with its phase and structural causes, never raw text.
func startupTimingError(err error) string {
	if err == nil {
		return "none"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var causes []string
		for _, cause := range joined.Unwrap() {
			causes = append(causes, startupTimingError(cause))
		}
		return strings.Join(causes, "; ")
	}
	for _, known := range []error{
		context.DeadlineExceeded, context.Canceled, runtimeowner.ErrOwned,
		agentmanager.ErrSupervisorCloseTimeout, app.ErrStorageBatchBusy,
		aterequest.ErrLeaseClosed, aterequest.ErrUnknown, aterequest.ErrConflict,
		aterequest.ErrInvalid, os.ErrNotExist, os.ErrPermission,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	if cause := errors.Unwrap(err); cause != nil {
		return fmt.Sprintf("%T: %s", err, startupTimingError(cause))
	}
	return fmt.Sprintf("%T (details withheld)", err)
}
