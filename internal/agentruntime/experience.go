package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/experienceport"
	"github.com/vkviyu/go-self-evolving-agent/internal/modelcontext"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
)

const (
	EventExperienceMaintenance session.EventType = "experience.maintenance"
	EventExperienceRetryFailed session.EventType = "experience.retry_failed"
	eventExperienceUsage       session.EventType = "experience.usage"
	maxExperienceGoalBytes                       = experienceport.MaxGoalBytes
	experienceRetryBudget                        = 2 * time.Second
	experienceRetryClaimBudget                   = 5 * time.Second
	maxExperienceRetryJobs                       = 1
)

type (
	// ExperienceMaintenance is independent of the durable turn outcome. Pending
	// jobs survive cancellation, service errors, and a crash before status append.
	ExperienceMaintenance struct {
		Status        string            `json:"status"`
		Error         string            `json:"error,omitempty"`
		Outcome       ExperienceOutcome `json:"outcome"`
		RetryAttempts int               `json:"retry_attempts,omitempty"`
		RetryAfter    *time.Time        `json:"retry_after,omitempty"`
	}

	experienceJob struct {
		sessionID          session.ID
		turnID, snapshotID string
		sequence           uint64
		maintenance        ExperienceMaintenance
	}
)

func (m *recordingModel) experienceEnabled() bool {
	return m != nil && m.purpose == "" && m.factory != nil && m.factory.config.Experience != nil
}

func (m *recordingModel) prepareExperience(ctx context.Context, input []*schema.Message) error {
	if !m.experienceEnabled() || m.experienceContext != nil {
		return nil
	}
	goal := m.openingInput
	if goal == "" {
		for i := len(input) - 1; i >= 0; i-- {
			if input[i] != nil && input[i].Role == schema.User {
				goal = input[i].Content
				break
			}
		}
	}
	// The service owns the compact ATE task reference. Retrieval receives the
	// actual opening goal; an excerpt could silently change its semantics.
	if len(goal) > maxExperienceGoalBytes || !utf8.ValidString(goal) {
		return fmt.Errorf("experience opening goal must be valid UTF-8 and at most %d bytes", maxExperienceGoalBytes)
	}
	m.emitLive(ctx, Event{Type: EventExperiencePreparationStarted, TurnID: m.turnID, Purpose: "experience"})
	if err := m.retryExperienceMaintenance(ctx); err != nil {
		return err
	}
	value, err := m.factory.config.Experience.Prepare(ctx, string(m.sessionID), m.turnID, goal, m.experienceGenerator())
	if err != nil {
		return fmt.Errorf("prepare experience: %w", err)
	}
	m.experienceContext, err = cloneExperienceContext(&value)
	return err
}

func cloneExperienceContext(value *experienceport.Context) (*experienceport.Context, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var stable experienceport.Context
	if err := json.Unmarshal(raw, &stable); err != nil {
		return nil, err
	}
	return &stable, nil
}

func validateExperienceReceipt(receipt *modelReceipt) error {
	if receipt.Experience == nil && receipt.ExperienceIndex == nil {
		return nil
	}
	if receipt.Experience == nil || receipt.ExperienceIndex == nil {
		return errors.New("incomplete experience recovery receipt")
	}
	value, index := receipt.Experience, *receipt.ExperienceIndex
	if len(value.Text) > experienceport.MaxContextBytes || !utf8.ValidString(value.Text) ||
		index < 0 || index >= len(receipt.Input) {
		return errors.New("invalid experience recovery receipt")
	}
	message := receipt.Input[index]
	if message == nil || message.Role != schema.User || message.Content != value.Text ||
		len(message.ToolCalls) != 0 || message.ToolCallID != "" {
		return errors.New("experience receipt does not match the dispatched user message")
	}
	return nil
}

func (m *recordingModel) attachExperience(input []*schema.Message, options []model.Option) ([]*schema.Message, error) {
	if !m.experienceEnabled() {
		if m.purpose == "" && m.experienceContext != nil {
			return nil, errors.New("experience service required to continue a prepared turn")
		}
		return input, nil
	}
	view, index, err := m.experienceInput(input)
	if err != nil {
		return nil, err
	}
	if err := m.checkDispatchCapacity(view, options); err != nil {
		return nil, err
	}
	m.receipt.Experience, err = cloneExperienceContext(m.experienceContext)
	m.receipt.ExperienceIndex = &index
	return view, err
}

// experienceInput constructs the exact future dispatch message without adding
// it to compaction evidence or the reusable history view.
func (m *recordingModel) experienceInput(input []*schema.Message) ([]*schema.Message, int, error) {
	value, err := cloneExperienceContext(m.experienceContext)
	if err != nil {
		return nil, 0, err
	}
	if value == nil || value.SessionID != string(m.sessionID) || value.TurnID != m.turnID ||
		len(value.Text) > experienceport.MaxContextBytes || !utf8.ValidString(value.Text) {
		return nil, 0, errors.New("invalid or oversized prepared experience context")
	}
	// Inserting at the end of the system prefix never separates an assistant
	// tool call from its results, including later iterations of the same turn.
	index := 0
	for index < len(input) && input[index] != nil && input[index].Role == schema.System {
		index++
	}
	hasUser := false
	for _, message := range input[index:] {
		hasUser = hasUser || (message != nil && message.Role == schema.User)
	}
	if !hasUser {
		return nil, 0, errors.New("experience requires an ordinary user prompt")
	}
	view := make([]*schema.Message, 0, len(input)+1)
	view = append(view, input[:index]...)
	view = append(view, value.Message())
	view = append(view, input[index:]...)
	return view, index, nil
}

func (m *recordingModel) experienceTokenReserve(input []*schema.Message, options []model.Option) (int, error) {
	if !m.experienceEnabled() {
		return 0, nil
	}
	view, _, err := m.experienceInput(input)
	if err != nil {
		return 0, err
	}
	// Count the exact message in position with the same schemas/calibration.
	// A separate CountMessages call for memory would charge calibration twice.
	return max(0, m.countRequestInput(view, options)-m.countRequestInput(input, options)), nil
}

func (m *recordingModel) checkDispatchCapacity(input []*schema.Message, options []model.Option) error {
	if m.factory == nil || m.factory.tokenizer == nil || m.factory.config.ContextWindowTokens <= 0 {
		return nil
	}
	return m.checkContextLimit(input, options, modelcontext.InputLimit(m.factory.config.ContextWindowTokens, requestOutputLimit(m.factory, options)))
}

func (m *recordingModel) checkExperience(ctx context.Context) error {
	if m.purpose != "" {
		return nil
	}
	if m.experienceEnabled() {
		m.emitLive(ctx, m.modelLifecycleEvent(EventExperienceSynchronizationStarted))
	}
	if err := m.syncExperience(ctx); err != nil {
		return err
	}
	if m.receipt.Experience == nil {
		if m.experienceEnabled() {
			return errors.New("ordinary model dispatch has no prepared experience")
		}
		return nil
	}
	if !m.experienceEnabled() {
		return errors.New("experience service required to authorize prepared replay")
	}
	if err := validateExperienceReceipt(&m.receipt); err != nil {
		return err
	}
	value, err := cloneExperienceContext(m.receipt.Experience)
	if err != nil {
		return err
	}
	if value.SessionID != string(m.sessionID) || value.TurnID != m.turnID {
		return errors.New("experience selection belongs to another turn")
	}
	if err := m.factory.config.Experience.Check(ctx, *value); err != nil {
		return fmt.Errorf("check experience before dispatch: %w", err)
	}
	return nil
}

func (m *recordingModel) experienceGenerator() experienceport.Generate {
	return func(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
		// Auxiliary calls share the turn's durable request sequence and budgets,
		// but have no main-response replay or context preparation state.
		auxiliary := &recordingModel{
			inner: m.inner, factory: m.factory, events: m.events,
			sessionID: m.sessionID, turnID: m.turnID, runtimeSnapshotID: m.runtimeSnapshotID,
			purpose:         "experience",
			budgetSessionID: m.budgetSessionID,
			liveSink:        m.liveSink,
		}
		auxiliary.requestSequence.Store(m.requestSequence.Load())
		defer func() { m.requestSequence.Store(auxiliary.requestSequence.Load()) }()
		return auxiliary.Generate(ctx, input, model.WithTools(nil))
	}
}

func (m *recordingModel) experienceTimeout() time.Duration {
	if timeout := m.factory.config.ExperienceTimeout; timeout > 0 {
		return timeout
	}
	return 2 * time.Minute
}

func (m *recordingModel) experiencePending(outcome ExperienceOutcome) session.PendingEvent {
	return session.PendingEvent{
		Type: EventExperienceMaintenance, CorrelationID: m.turnID, RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload: session.MustJSONPayload(ExperienceMaintenance{Status: "pending", Outcome: outcome}),
	}
}

func (m *recordingModel) maintainExperience(ctx context.Context, outcome ExperienceOutcome) *ExperienceMaintenance {
	status := &ExperienceMaintenance{Status: "pending", Outcome: outcome}
	return m.maintainExperienceStatus(ctx, status)
}

func (m *recordingModel) maintainExperienceStatus(ctx context.Context, status *ExperienceMaintenance) *ExperienceMaintenance {
	outcome := status.Outcome
	work, cancel := context.WithTimeout(ctx, m.experienceTimeout())
	defer cancel()
	hook := m.factory.config.ExperienceLifecycle
	if hook == nil {
		hook, _ = m.factory.config.Experience.(ExperienceLifecycle)
	}
	var err error
	if hook != nil {
		err = hook.Finish(work, string(m.sessionID), m.turnID, outcome)
	}
	if err == nil && outcome.Succeeded {
		err = m.factory.config.Experience.Complete(work, string(m.sessionID), m.turnID, m.experienceGenerator())
	}
	if err == nil {
		err = work.Err()
	}
	// Flush auxiliary source events even on a failed/cancelled maintenance
	// attempt. This has its own bounded context, not an expired model deadline.
	syncContext, stopSync := durabilityContext(ctx)
	err = errors.Join(err, m.syncExperience(syncContext))
	stopSync()
	if err == nil {
		status.Status = "completed"
		status.Error, status.RetryAfter = "", nil
	} else {
		status.Error = err.Error()
	}
	recordErr := m.recordExperienceMaintenance(ctx, status)
	if recordErr != nil {
		// The earlier pending event remains authoritative. Report this separately
		// to the caller without making a completed turn eligible for execution.
		status.Status = "pending"
		status.Error = errors.Join(err, fmt.Errorf("record experience maintenance: %w", recordErr)).Error()
	}
	return status
}

func (m *recordingModel) recordExperienceMaintenance(ctx context.Context, status *ExperienceMaintenance) error {
	// Retry completion has the same durability contract as first completion.
	// A short diagnostic deadline must not interrupt its native commit.
	durable, stop := durabilityContext(ctx)
	defer stop()
	_, err := m.events.Append(durable, m.sessionID, session.AnySequence, session.PendingEvent{
		Type: EventExperienceMaintenance, CorrelationID: m.turnID, RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload: session.MustJSONPayload(status),
	})
	return err
}

func (m *recordingModel) retryExperienceMaintenance(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.experienceRetry != nil {
		return nil
	}
	work, cancel := context.WithTimeout(ctx, min(experienceRetryBudget, m.experienceTimeout()))
	defer cancel()
	jobs := map[string]session.Event{}
	var order []string
	collect := func(event session.Event) error {
		if err := work.Err(); err != nil {
			return err
		}
		if event.Type != EventExperienceMaintenance || (event.SessionID == m.sessionID && event.CorrelationID == m.turnID) {
			return nil
		}
		key := string(event.SessionID) + "\x00" + event.CorrelationID
		if _, exists := jobs[key]; !exists {
			order = append(order, key)
		}
		jobs[key] = event
		return nil
	}
	var err error
	if index, ok := m.events.(PendingExperienceMaintenanceStore); ok {
		var pending []session.Event
		pending, err = index.PendingExperienceMaintenance(work, 32)
		if err == nil && len(pending) > 32 {
			err = errors.New("maintenance lookup exceeded its bound")
		}
		if err == nil {
			for _, event := range pending {
				if err = collect(event); err != nil {
					break
				}
			}
		}
	} else {
		err = session.VisitEvents(work, m.events, m.sessionID, collect)
	}
	if err != nil {
		m.recordExperienceRetryFailure(ctx, err)
		return ctx.Err()
	}
	for _, id := range order {
		event := jobs[id]
		var status ExperienceMaintenance
		if err := json.Unmarshal(event.Payload, &status); err != nil {
			m.recordExperienceRetryFailure(ctx, err)
			continue
		}
		if status.Status != "pending" ||
			(status.RetryAfter != nil && time.Now().Before(*status.RetryAfter)) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		job := experienceJob{
			sessionID: event.SessionID, turnID: event.CorrelationID,
			snapshotID: event.RuntimeSnapshotID, sequence: event.Sequence, maintenance: status,
		}
		job.maintenance.RetryAttempts++
		// Discovery may consume its entire read budget. Admission is a separate
		// durable CAS and receives a fresh bounded deadline from the caller.
		claim, stopClaim := context.WithTimeout(ctx, min(experienceRetryClaimBudget, m.experienceTimeout()))
		claimErr := m.claimExperienceRetry(claim, &job)
		stopClaim()
		if claimErr != nil {
			m.recordExperienceRetryFailure(ctx, claimErr)
			continue
		}
		m.experienceRetry = &job
		break // Admit at most one job. No model or lifecycle calls before the turn.
	}
	// Maintenance failure is not authorization failure for a new selection.
	// Only cancellation of the caller may prevent the new Prepare from running.
	return ctx.Err()
}

// claimExperienceRetry also renews an admission at the post-turn boundary.
// A later completion or competing admission invalidates the saved sequence.
func (m *recordingModel) claimExperienceRetry(ctx context.Context, job *experienceJob) error {
	if job.sessionID == "" || job.turnID == "" {
		return errors.New("maintenance job has no originating session or turn")
	}
	through, err := m.events.CurrentSequence(ctx, job.sessionID)
	if err != nil {
		return err
	}
	var latest uint64
	visit := func(event session.Event) error {
		if event.Type == EventExperienceMaintenance && event.CorrelationID == job.turnID {
			latest = event.Sequence
		}
		return nil
	}
	if indexed, ok := m.events.(session.CorrelatedEventStore); ok {
		var events []session.Event
		events, err = indexed.LoadByCorrelation(ctx, job.sessionID, job.turnID)
		for _, event := range events {
			if err != nil {
				break
			}
			err = visit(event)
		}
	} else {
		err = session.VisitEvents(ctx, m.events, job.sessionID, visit)
	}
	if err != nil {
		return err
	}
	if latest != job.sequence {
		return errors.New("maintenance admission superseded")
	}
	after := time.Now().UTC().Add(max(experienceRetryDelay(job.maintenance.RetryAttempts), m.experienceTimeout()+time.Minute))
	job.maintenance.RetryAfter = &after
	events, err := m.events.Append(ctx, job.sessionID, through, session.PendingEvent{
		Type: EventExperienceMaintenance, CorrelationID: job.turnID, RuntimeSnapshotID: job.snapshotID,
		Payload: session.MustJSONPayload(job.maintenance),
	})
	if err == nil {
		job.sequence = events[0].Sequence
	}
	return err
}

func (m *recordingModel) runExperienceRetry(ctx context.Context) {
	if !m.experienceEnabled() || m.experienceRetry == nil || ctx.Err() != nil {
		return
	}
	job := m.experienceRetry
	m.experienceRetry = nil
	work, cancel := context.WithTimeout(ctx, m.experienceTimeout())
	defer cancel()
	if err := m.claimExperienceRetry(work, job); err != nil {
		m.recordExperienceRetryFailure(ctx, err)
		return
	}
	// This is host maintenance: use today's factory actor/model/budget. Exact
	// model requests and source provenance stay on the originating session/turn.
	// Profile accounting is mirrored as usage-only events on the host session.
	retry := &recordingModel{
		inner: m.inner, factory: m.factory, events: m.events, sessionID: job.sessionID,
		turnID: job.turnID, runtimeSnapshotID: job.snapshotID, budgetSessionID: m.sessionID,
		liveSink: m.liveSink,
	}
	sequence, err := m.factory.modelRequestSequence(work, job.sessionID, job.turnID)
	if err != nil {
		job.maintenance.Error = err.Error()
		_ = retry.recordExperienceMaintenance(ctx, &job.maintenance)
		return
	}
	retry.requestSequence.Store(sequence)
	retry.maintainExperienceStatus(work, &job.maintenance)
}

func (m *recordingModel) syncExperience(ctx context.Context) error {
	if !m.experienceEnabled() {
		return nil
	}
	syncer, ok := m.factory.config.Experience.(ExperienceSync)
	if !ok {
		return nil
	}
	work, cancel := context.WithTimeout(ctx, min(5*time.Second, m.experienceTimeout()))
	defer cancel()
	if err := syncer.Sync(work); err != nil {
		return fmt.Errorf("sync experience sources: %w", err)
	}
	return work.Err()
}

func (m *recordingModel) usageSessionID() session.ID {
	if m.budgetSessionID != "" {
		return m.budgetSessionID
	}
	return m.sessionID
}

func (m *recordingModel) recordHostMaintenanceUsage(ctx context.Context, phase session.EventType, payload map[string]any) error {
	if m.purpose != "experience" || m.usageSessionID() == m.sessionID {
		return nil
	}
	usage := map[string]any{
		"phase": phase, "source_session_id": m.sessionID, "source_turn_id": m.turnID,
		"budget_session_id": m.usageSessionID(), "actor_scope": m.factory.config.ActorScope,
	}
	for _, key := range []string{"request_id", "estimated_input_tokens", "output_token_limit", "prompt_tokens", "completion_tokens"} {
		if value, exists := payload[key]; exists {
			usage[key] = value
		}
	}
	_, err := m.events.Append(ctx, m.usageSessionID(), session.AnySequence, session.PendingEvent{
		Type: eventExperienceUsage, CorrelationID: m.turnID, Payload: session.MustJSONPayload(usage),
	})
	return err
}

func experienceRetryDelay(attempts int) time.Duration {
	return min(time.Hour, time.Minute*time.Duration(1<<min(6, max(0, attempts-1))))
}

func (m *recordingModel) recordExperienceRetryFailure(ctx context.Context, cause error) {
	durable, cancel := durabilityContext(ctx)
	defer cancel()
	// This diagnostic never creates a maintenance job for the current turn.
	// If storage is unavailable, the original pending job remains authoritative.
	_, _ = m.events.Append(durable, m.sessionID, session.AnySequence, session.PendingEvent{
		Type: EventExperienceRetryFailed, CorrelationID: m.turnID, RuntimeSnapshotID: m.runtimeSnapshotID,
		Payload: session.MustJSONPayload(map[string]string{"error": cause.Error()}),
	})
}
