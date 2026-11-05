package sqlitestore

// migrations is the append-only, globally ordered schema ledger. Keep published
// DDL byte-for-byte: its whitespace is covered by persisted SHA-256 checksums.
// Cross-domain versions (including 6, 26 and 27) must remain indivisible and in
// this order, not be regrouped by today's store boundaries. Domain code lives
// in separate files; corrections to a published schema require a new version.
var migrations = []migration{
	{version: 1, ddl: `
CREATE TABLE IF NOT EXISTS schema_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS session_events (
    session_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    event_id TEXT NOT NULL UNIQUE,
    event_type TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    causation_id TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    runtime_snapshot_id TEXT NOT NULL DEFAULT '',
    payload BLOB NOT NULL,
    checksum TEXT NOT NULL,
    schema_version INTEGER NOT NULL,
    PRIMARY KEY (session_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_session_events_type
    ON session_events(session_id, event_type, sequence);
CREATE TABLE IF NOT EXISTS agent_checkpoints (
    checkpoint_id TEXT PRIMARY KEY,
    checkpoint BLOB NOT NULL,
    checksum TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO schema_meta(key, value) VALUES('session_events', '1')
    ON CONFLICT(key) DO NOTHING;
`},
	{version: 2, ddl: `
CREATE TABLE orchestration_events (
    global_sequence INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL UNIQUE,
    stream_type TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    stream_sequence INTEGER NOT NULL,
    team_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    actor_agent_id TEXT NOT NULL DEFAULT '',
    causation_id TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    payload BLOB NOT NULL,
    checksum TEXT NOT NULL,
    schema_version INTEGER NOT NULL,
    UNIQUE(stream_type, stream_id, stream_sequence)
);
CREATE INDEX idx_orchestration_events_team
    ON orchestration_events(team_id, global_sequence);
CREATE INDEX idx_orchestration_events_type
    ON orchestration_events(event_type, global_sequence);
CREATE TABLE teams (
    team_id TEXT PRIMARY KEY,
    root_agent_id TEXT NOT NULL,
    state TEXT NOT NULL,
    version INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE agents (
    agent_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    parent_agent_id TEXT NOT NULL DEFAULT '',
    thread_id TEXT NOT NULL UNIQUE,
    assigned_task_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    activity TEXT NOT NULL DEFAULT '',
    profile_revision TEXT NOT NULL,
    current_run_id TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_agents_team_state ON agents(team_id, state);
CREATE TABLE agent_threads (
    thread_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL UNIQUE,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL UNIQUE REFERENCES agents(agent_id),
    parent_thread_id TEXT NOT NULL DEFAULT '',
    context_policy TEXT NOT NULL DEFAULT 'none',
    profile_revision TEXT NOT NULL,
    last_sequence INTEGER NOT NULL DEFAULT 0,
    active_turn_id TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE tasks (
    task_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    parent_task_id TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    state TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    assignee_agent_id TEXT NOT NULL DEFAULT '',
    required_profile TEXT NOT NULL DEFAULT '',
    attempt INTEGER NOT NULL DEFAULT 0,
    version INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_tasks_team_state ON tasks(team_id, state, priority);
CREATE TABLE task_dependencies (
    task_id TEXT NOT NULL REFERENCES tasks(task_id),
    dependency_task_id TEXT NOT NULL REFERENCES tasks(task_id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(task_id, dependency_task_id),
    CHECK(task_id <> dependency_task_id)
);
CREATE TABLE idempotency_records (
    scope TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    result_type TEXT NOT NULL,
    result_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(scope, idempotency_key)
);
CREATE TABLE projection_cursors (
    projection_name TEXT PRIMARY KEY,
    global_sequence INTEGER NOT NULL,
    updated_at TEXT NOT NULL
);
`},
	{version: 3, ddl: `
ALTER TABLE agents ADD COLUMN depth INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_agents_team_parent ON agents(team_id, parent_agent_id, state);
`},
	{version: 4, ddl: `
ALTER TABLE agent_threads ADD COLUMN initial_prompt TEXT NOT NULL DEFAULT '';
`},
	{version: 5, ddl: `
CREATE TABLE mailbox_messages (
    message_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    sender_agent_id TEXT NOT NULL,
    recipient_agent_id TEXT NOT NULL,
    message_type TEXT NOT NULL,
    body BLOB NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    sequence INTEGER NOT NULL,
    delivery_state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    delivered_at TEXT,
    acknowledged_at TEXT,
    UNIQUE(recipient_agent_id, sequence)
);
CREATE UNIQUE INDEX idx_mailbox_idempotency
    ON mailbox_messages(team_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE INDEX idx_mailbox_recipient_state
    ON mailbox_messages(recipient_agent_id, delivery_state, sequence);
`},
	{version: 6, ddl: `
ALTER TABLE idempotency_records ADD COLUMN request_hash TEXT NOT NULL DEFAULT '';
CREATE TABLE mailbox_recipient_sequences (
    recipient_agent_id TEXT PRIMARY KEY,
    next_sequence INTEGER NOT NULL
);
INSERT INTO mailbox_recipient_sequences(recipient_agent_id, next_sequence)
SELECT recipient_agent_id, MAX(sequence) + 1
FROM mailbox_messages
GROUP BY recipient_agent_id;
`},
	{version: 7, ddl: `
ALTER TABLE tasks ADD COLUMN verification_profile TEXT NOT NULL DEFAULT '';
`},
	{version: 8, ddl: `
ALTER TABLE tasks ADD COLUMN claim_expires_at TEXT NOT NULL DEFAULT '';
`},
	{version: 9, ddl: `
CREATE TABLE wait_sets (
    wait_set_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    run_id TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    condition TEXT NOT NULL,
    target_agent_ids BLOB NOT NULL,
    wait_any INTEGER NOT NULL DEFAULT 0,
    deadline TEXT,
    resume_token TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    resolved_at TEXT
);
CREATE INDEX idx_wait_sets_agent_state ON wait_sets(agent_id, state);
CREATE INDEX idx_wait_sets_team_state ON wait_sets(team_id, state);
CREATE UNIQUE INDEX idx_wait_sets_idempotency
    ON wait_sets(team_id, idempotency_key)
    WHERE idempotency_key <> '';
`},
	{version: 10, ddl: `
CREATE TABLE usage_records (
    record_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    run_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL,
    model_profile TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0,
    quality TEXT NOT NULL DEFAULT 'provider',
    recorded_at TEXT NOT NULL,
    UNIQUE(team_id, request_id)
);
CREATE INDEX idx_usage_records_team_time ON usage_records(team_id, recorded_at);
CREATE INDEX idx_usage_records_agent_time ON usage_records(agent_id, recorded_at);
CREATE TABLE team_budgets (
    team_id TEXT PRIMARY KEY REFERENCES teams(team_id),
    max_input_tokens INTEGER NOT NULL DEFAULT 0,
    max_output_tokens INTEGER NOT NULL DEFAULT 0,
    max_total_tokens INTEGER NOT NULL DEFAULT 0,
    max_cost_micros INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
`},
	{version: 11, ddl: `
CREATE TABLE workspace_leases (
    lease_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    run_id TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    mode TEXT NOT NULL,
    state TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    released_at TEXT
);
CREATE INDEX idx_workspace_leases_team_state ON workspace_leases(team_id, state, expires_at);
CREATE INDEX idx_workspace_leases_agent_state ON workspace_leases(agent_id, state);
CREATE UNIQUE INDEX idx_workspace_leases_idempotency
    ON workspace_leases(team_id, idempotency_key)
    WHERE idempotency_key <> '';
`},
	{version: 12, ddl: `
ALTER TABLE workspace_leases ADD COLUMN request_hash TEXT NOT NULL DEFAULT '';
`},
	{version: 13, ddl: `
CREATE TABLE workspace_views (
    view_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    task_id TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL,
    workspace_root TEXT NOT NULL,
    root_path TEXT NOT NULL,
    base_revision TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    released_at TEXT
);
CREATE INDEX idx_workspace_views_team_state ON workspace_views(team_id, state, created_at);
CREATE INDEX idx_workspace_views_agent_state ON workspace_views(agent_id, state);
CREATE UNIQUE INDEX idx_workspace_views_idempotency ON workspace_views(team_id, idempotency_key)
    WHERE idempotency_key <> '';
CREATE TABLE merge_queue (
    merge_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    task_id TEXT NOT NULL DEFAULT '',
    view_id TEXT NOT NULL REFERENCES workspace_views(view_id),
    base_revision TEXT NOT NULL,
    patch_artifact TEXT NOT NULL,
    state TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX idx_merge_queue_team_state ON merge_queue(team_id, state, created_at);
CREATE UNIQUE INDEX idx_merge_queue_idempotency ON merge_queue(team_id, idempotency_key)
    WHERE idempotency_key <> '';
`},
	{version: 14, ddl: `
ALTER TABLE wait_sets ADD COLUMN request_hash TEXT NOT NULL DEFAULT '';
`},
	{version: 15, ddl: `
CREATE TABLE usage_reservations (
    reservation_id TEXT PRIMARY KEY,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    run_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE(team_id, request_id)
);
CREATE INDEX idx_usage_reservations_team_state ON usage_reservations(team_id, state);
`},
	{version: 16, ddl: `
ALTER TABLE usage_reservations ADD COLUMN request_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_usage_reservations_pending_time ON usage_reservations(state, created_at);
`},
	{version: 17, ddl: `
ALTER TABLE merge_queue ADD COLUMN resolution_task_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_merge_queue_resolution_task ON merge_queue(resolution_task_id);
`},
	{version: 18, ddl: `
CREATE TABLE goal_runs (
    goal_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    team_id TEXT NOT NULL DEFAULT '',
    agent_id TEXT NOT NULL DEFAULT '',
    goal_text TEXT NOT NULL,
    state TEXT NOT NULL,
    assessment TEXT NOT NULL,
    current_round INTEGER NOT NULL DEFAULT 0,
    max_rounds INTEGER NOT NULL,
    max_wall_time_ns INTEGER NOT NULL,
    decision TEXT NOT NULL DEFAULT '',
    stop_reason TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE INDEX idx_goal_runs_session_state ON goal_runs(session_id, state, started_at);
CREATE TABLE goal_rounds (
    goal_id TEXT NOT NULL REFERENCES goal_runs(goal_id),
    round_number INTEGER NOT NULL,
    turn_id TEXT NOT NULL UNIQUE,
    input TEXT NOT NULL,
    candidate TEXT NOT NULL DEFAULT '',
    decision TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    completed_at TEXT,
    PRIMARY KEY(goal_id, round_number)
);
CREATE TABLE goal_reports (
    turn_id TEXT PRIMARY KEY REFERENCES goal_rounds(turn_id),
    goal_id TEXT NOT NULL REFERENCES goal_runs(goal_id),
    round_number INTEGER NOT NULL,
    decision TEXT NOT NULL,
    reason TEXT NOT NULL,
    evidence BLOB NOT NULL,
    blockers BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_goal_reports_goal_round ON goal_reports(goal_id, round_number);
`},
	{version: 19, ddl: `
ALTER TABLE agents ADD COLUMN result BLOB NOT NULL DEFAULT X'';
`},
	{version: 20, ddl: `
CREATE TABLE flow_runs (
    flow_id TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    definition TEXT NOT NULL,
    session_id TEXT NOT NULL,
    team_id TEXT NOT NULL REFERENCES teams(team_id),
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    state TEXT NOT NULL,
    input BLOB NOT NULL,
    outputs BLOB NOT NULL DEFAULT X'5B5D',
    error TEXT NOT NULL DEFAULT '',
    registry_generation INTEGER NOT NULL,
    version INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT,
    UNIQUE(team_id, idempotency_key)
);
CREATE INDEX idx_flow_runs_team_state ON flow_runs(team_id, state, started_at);
CREATE TABLE flow_steps (
    flow_id TEXT NOT NULL REFERENCES flow_runs(flow_id),
    step_id TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    state TEXT NOT NULL,
    outputs BLOB NOT NULL DEFAULT X'5B5D',
    error TEXT NOT NULL DEFAULT '',
    attempt INTEGER NOT NULL,
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    completed_at TEXT,
    PRIMARY KEY(flow_id, step_id)
);
CREATE TABLE flow_agent_sessions (
    flow_id TEXT NOT NULL REFERENCES flow_runs(flow_id),
    session_name TEXT NOT NULL,
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(flow_id, session_name)
);
`},
	{version: 21, ddl: `
CREATE INDEX idx_session_events_correlation
    ON session_events(session_id, correlation_id, sequence);
`},
	{version: 22, ddl: `
CREATE TABLE flow_agent_stages (
    flow_id TEXT NOT NULL REFERENCES flow_runs(flow_id),
    session_name TEXT NOT NULL,
    step_id TEXT NOT NULL,
    action TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(flow_id, session_name, step_id)
);
`},
	{version: 23, ddl: `
ALTER TABLE flow_runs ADD COLUMN definition_revision TEXT NOT NULL DEFAULT '';
ALTER TABLE flow_runs ADD COLUMN definition_fingerprint TEXT NOT NULL DEFAULT '';
`},
	{version: 24, ddl: `
CREATE TABLE lsp_diagnostic_projections (
    workspace_root TEXT NOT NULL,
    path TEXT NOT NULL,
    revision TEXT NOT NULL,
    report BLOB NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(workspace_root, path, revision)
);
CREATE INDEX idx_lsp_diagnostics_latest
    ON lsp_diagnostic_projections(workspace_root, path, updated_at DESC);
`},
	{version: 25, ddl: `
ALTER TABLE goal_runs ADD COLUMN contract BLOB NOT NULL DEFAULT X'6E756C6C';
`},
	{version: 26, ddl: `
ALTER TABLE flow_runs ADD COLUMN runtime_snapshot_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN profile_snapshot BLOB NOT NULL DEFAULT X'';
`},
	{version: 27, ddl: `
ALTER TABLE goal_rounds ADD COLUMN feedback TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_reservations ADD COLUMN lifecycle_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_reservations ADD COLUMN model_profile TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_reservations ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_reservations ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_reservations ADD COLUMN dispatched_at TEXT;
ALTER TABLE usage_reservations ADD COLUMN outcome BLOB;
`},
	{version: 28, ddl: `
CREATE TABLE plugin_blobs (
    content_sha TEXT PRIMARY KEY,
    source BLOB NOT NULL CHECK(length(source) > 0 AND length(source) <= 33554432)
);
CREATE TABLE plugin_revisions (
    tool_name TEXT NOT NULL,
    revision TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) <= 65536),
    record_sha TEXT NOT NULL,
    content_sha TEXT NOT NULL REFERENCES plugin_blobs(content_sha),
    state TEXT NOT NULL CHECK(state IN ('staged', 'active', 'retired')),
    PRIMARY KEY(tool_name, revision)
);
CREATE UNIQUE INDEX plugin_one_active_revision ON plugin_revisions(tool_name) WHERE state = 'active';
`},
	{version: 29, ddl: `
CREATE TABLE runtime_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    manifest BLOB NOT NULL CHECK(length(manifest) > 0 AND length(manifest) <= 1048576)
);
CREATE TABLE runtime_snapshot_refs (
    kind TEXT NOT NULL CHECK(kind IN ('turn', 'flow')),
    scope TEXT NOT NULL,
    consumer_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL REFERENCES runtime_snapshots(snapshot_id),
    PRIMARY KEY(kind, scope, consumer_id)
);
CREATE INDEX runtime_snapshot_ref_lookup ON runtime_snapshot_refs(snapshot_id);
`},
	{version: 30, ddl: `
ALTER TABLE runtime_snapshot_refs ADD COLUMN released_at TEXT;
ALTER TABLE runtime_snapshot_refs ADD COLUMN release_reason TEXT NOT NULL DEFAULT '';
CREATE TABLE recovery_abandonments (
    session_id TEXT NOT NULL,
    target_kind TEXT NOT NULL CHECK(target_kind IN ('turn', 'goal')),
    target_id TEXT NOT NULL,
    receipt BLOB NOT NULL,
    receipt_sha TEXT NOT NULL,
    PRIMARY KEY(session_id, target_kind, target_id)
);
`},
	{version: 31, ddl: `
CREATE TABLE runtime_consumer_closures (
    kind TEXT NOT NULL CHECK(kind IN ('turn', 'flow')),
    scope TEXT NOT NULL,
    consumer_id TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    closed_at TEXT NOT NULL,
    PRIMARY KEY(kind, scope, consumer_id)
);
CREATE TABLE maintenance_receipts (
    plan_id TEXT PRIMARY KEY,
    operation TEXT NOT NULL,
    receipt BLOB NOT NULL,
    receipt_sha TEXT NOT NULL
);
CREATE TABLE plugin_revision_tombstones (
    tool_name TEXT NOT NULL,
    revision TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    record BLOB NOT NULL,
    record_sha TEXT NOT NULL,
    content_sha TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    collected_at TEXT NOT NULL,
    PRIMARY KEY(tool_name, revision)
);
`},
	{version: 32, ddl: `
CREATE TABLE plugin_test_reports (
    report_id TEXT PRIMARY KEY,
    tool_name TEXT NOT NULL,
    revision TEXT NOT NULL,
    content_sha TEXT NOT NULL,
    report BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE plugin_builds (
    build_id TEXT PRIMARY KEY,
    request BLOB NOT NULL CHECK(length(request) <= 262144),
    request_sha TEXT NOT NULL,
    result BLOB,
    result_sha TEXT,
    created_at TEXT NOT NULL,
    completed_at TEXT
);
`},
	{version: 33, ddl: `
CREATE TABLE strategy_specs (spec_id TEXT PRIMARY KEY, owner_session TEXT NOT NULL, revision TEXT NOT NULL, spec BLOB NOT NULL, UNIQUE(owner_session,revision));
CREATE TABLE strategy_suites (suite_id TEXT PRIMARY KEY, suite BLOB NOT NULL);
CREATE TABLE strategy_reports (report_id TEXT PRIMARY KEY, report BLOB NOT NULL);
CREATE TABLE strategy_active (baseline_id TEXT PRIMARY KEY, spec_id TEXT NOT NULL, report_id TEXT NOT NULL);
CREATE TABLE strategy_events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, event_type TEXT NOT NULL, payload BLOB NOT NULL, checksum TEXT NOT NULL, occurred_at TEXT NOT NULL);
CREATE TABLE strategy_quarantine (baseline_id TEXT NOT NULL, spec_id TEXT NOT NULL, reason TEXT NOT NULL, PRIMARY KEY(baseline_id,spec_id));
`},
	{version: 34, ddl: `
ALTER TABLE agents ADD COLUMN workspace_request TEXT NOT NULL DEFAULT 'auto';
ALTER TABLE agents ADD COLUMN workspace_mode TEXT NOT NULL DEFAULT '';
`},
	{version: 35, ddl: `
ALTER TABLE merge_queue ADD COLUMN kind TEXT NOT NULL DEFAULT 'git-patch';
ALTER TABLE merge_queue ADD COLUMN candidate_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE merge_queue ADD COLUMN plan_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE merge_queue ADD COLUMN supersedes TEXT NOT NULL DEFAULT '';
ALTER TABLE merge_queue ADD COLUMN superseded_by TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_merge_queue_view ON merge_queue(view_id, state);
CREATE INDEX idx_merge_queue_task ON merge_queue(task_id, state);
CREATE TABLE merge_apply_journal (
    merge_id TEXT PRIMARY KEY REFERENCES merge_queue(merge_id),
    plan_digest TEXT NOT NULL,
    plan_json BLOB NOT NULL CHECK(length(plan_json) <= 16777216),
    state TEXT NOT NULL CHECK(state IN ('started', 'finished')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE merge_apply_progress (
    merge_id TEXT NOT NULL REFERENCES merge_apply_journal(merge_id),
    path TEXT NOT NULL,
    applied_at TEXT NOT NULL,
    PRIMARY KEY(merge_id, path)
);
`},
	{version: 36, ddl: `
ALTER TABLE mailbox_messages ADD COLUMN consumer_turn_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mailbox_messages ADD COLUMN consumer_request INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mailbox_messages ADD COLUMN bound_at TEXT;
CREATE INDEX idx_mailbox_consumer
    ON mailbox_messages(recipient_agent_id, consumer_turn_id)
    WHERE consumer_turn_id <> '';
`},
	{version: 37, ddl: `
CREATE TABLE merge_finalizations (
    merge_id TEXT PRIMARY KEY REFERENCES merge_queue(merge_id),
    binding_digest TEXT NOT NULL,
    workspace_digest TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK(state IN ('prepared', 'dispatched', 'completed', 'unknown')),
    target_state TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    settled INTEGER NOT NULL DEFAULT 0 CHECK(settled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`},
	{version: 38, ddl: `
ALTER TABLE merge_finalizations ADD COLUMN verification_digest TEXT NOT NULL DEFAULT '';
`},
	{version: 39, ddl: `
CREATE TABLE agent_completions (
    agent_id TEXT NOT NULL REFERENCES agents(agent_id),
    run_id TEXT NOT NULL,
    result BLOB NOT NULL,
    result_sha TEXT NOT NULL,
    settled INTEGER NOT NULL DEFAULT 0 CHECK(settled IN (0, 1)),
    created_at TEXT NOT NULL,
    settled_at TEXT,
    PRIMARY KEY(agent_id, run_id)
);
CREATE INDEX idx_agent_completions_pending ON agent_completions(agent_id) WHERE settled = 0;
`},
	{version: 40, ddl: `
CREATE TABLE task_refinement_members (
    parent_task_id TEXT NOT NULL REFERENCES tasks(task_id),
    child_task_id TEXT NOT NULL UNIQUE REFERENCES tasks(task_id),
    created_at TEXT NOT NULL,
    PRIMARY KEY(parent_task_id, child_task_id),
    CHECK(parent_task_id <> child_task_id)
);
`},
	{version: 41, ddl: `
CREATE TABLE goal_admissions (
    token TEXT PRIMARY KEY,
    goal_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    team_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    input_digest TEXT NOT NULL,
    contract_digest TEXT NOT NULL,
    config_digest TEXT NOT NULL,
    turn_id TEXT NOT NULL DEFAULT '',
    round INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL CHECK(state IN ('reserved', 'turn_started', 'round_started', 'consumed', 'unavailable')),
    version INTEGER NOT NULL DEFAULT 1,
    reason TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_goal_admissions_goal ON goal_admissions(goal_id);
CREATE INDEX idx_goal_admissions_session_state ON goal_admissions(session_id, state, updated_at);
CREATE INDEX idx_goal_admissions_turn ON goal_admissions(session_id, turn_id) WHERE turn_id <> '';
`},
}
