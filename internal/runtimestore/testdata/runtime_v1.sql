
CREATE TABLE runtime_schema (
    version INTEGER PRIMARY KEY,
    checksum TEXT NOT NULL
);
CREATE TABLE runtime_authority (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    store_id TEXT NOT NULL
);
CREATE TABLE agent_checkpoints (
    checkpoint_id TEXT PRIMARY KEY,
    checkpoint BLOB NOT NULL,
    checksum TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE lsp_diagnostic_projections (
    workspace_root TEXT NOT NULL, path TEXT NOT NULL, revision TEXT NOT NULL,
    report BLOB NOT NULL, updated_at TEXT NOT NULL,
    PRIMARY KEY(workspace_root, path, revision)
);
CREATE INDEX idx_lsp_diagnostics_latest
    ON lsp_diagnostic_projections(workspace_root, path, updated_at DESC);
CREATE TABLE plugin_blobs (
    content_sha TEXT PRIMARY KEY,
    source BLOB NOT NULL CHECK(length(source) > 0 AND length(source) <= 33554432)
);
CREATE TABLE plugin_revisions (
    tool_name TEXT NOT NULL, revision TEXT NOT NULL, plugin_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) <= 65536),
    record_sha TEXT NOT NULL,
    content_sha TEXT NOT NULL REFERENCES plugin_blobs(content_sha),
    state TEXT NOT NULL CHECK(state IN ('staged', 'active', 'retired')),
    PRIMARY KEY(tool_name, revision)
);
CREATE UNIQUE INDEX plugin_one_active_revision ON plugin_revisions(tool_name) WHERE state = 'active';
CREATE TABLE runtime_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    manifest BLOB NOT NULL CHECK(length(manifest) > 0 AND length(manifest) <= 1048576)
);
CREATE TABLE runtime_snapshot_refs (
    kind TEXT NOT NULL CHECK(kind IN ('turn', 'flow')),
    scope TEXT NOT NULL, consumer_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL REFERENCES runtime_snapshots(snapshot_id),
    released_at TEXT, release_reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(kind, scope, consumer_id)
);
CREATE INDEX runtime_snapshot_ref_lookup ON runtime_snapshot_refs(snapshot_id);
CREATE TABLE runtime_consumer_closures (
    kind TEXT NOT NULL CHECK(kind IN ('turn', 'flow')),
    scope TEXT NOT NULL, consumer_id TEXT NOT NULL, plan_id TEXT NOT NULL,
    closed_at TEXT NOT NULL, PRIMARY KEY(kind, scope, consumer_id)
);
CREATE TABLE plugin_revision_tombstones (
    tool_name TEXT NOT NULL, revision TEXT NOT NULL, plugin_id TEXT NOT NULL,
    record BLOB NOT NULL, record_sha TEXT NOT NULL, content_sha TEXT NOT NULL,
    plan_id TEXT NOT NULL, collected_at TEXT NOT NULL,
    PRIMARY KEY(tool_name, revision)
);

CREATE TABLE ate_pending_requests (
    space TEXT NOT NULL, principal TEXT NOT NULL, request_id TEXT NOT NULL,
    store_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) > 0 AND length(record) <= 12582912),
    checksum TEXT NOT NULL,
    attempted INTEGER NOT NULL DEFAULT 0 CHECK(attempted IN (0,1)),
    PRIMARY KEY(space, principal, request_id)
);

CREATE TABLE ate_rejected_requests (
    space TEXT NOT NULL, principal TEXT NOT NULL, request_id TEXT NOT NULL,
    store_id TEXT NOT NULL,
    record BLOB NOT NULL CHECK(length(record) > 0 AND length(record) <= 65536),
    checksum TEXT NOT NULL,
    PRIMARY KEY(space, principal, request_id)
);
