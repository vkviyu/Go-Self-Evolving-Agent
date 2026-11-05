package sqlitestore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/session"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Collection audits all frozen toolsets and legacy consumers in the caller's
// transaction. Unknown identities remain blockers; a preview never fixes them.
func planPluginCollection(ctx context.Context, query readQuery, name, revision string) (retention.CollectionPlan, error) {
	plan := retention.CollectionPlan{Tool: name, Revision: revision, Blockers: []string{}}
	if name == "" || revision == "" {
		return plan, errors.New("collection requires exact tool name and revision")
	}
	var encoded, source []byte
	var sha, pluginID string
	err := query.QueryRowContext(ctx, `SELECT r.record,r.record_sha,r.plugin_id,r.content_sha,r.state,b.source FROM plugin_revisions r JOIN plugin_blobs b USING(content_sha) WHERE r.tool_name=? AND r.revision=?`, name, revision).Scan(&encoded, &sha, &pluginID, &plan.ContentSHA256, &plan.State, &source)
	if err != nil {
		return plan, err
	}
	record, err := plugin.DecodeRecord(encoded)
	if err != nil {
		return plan, err
	}
	if sha256Hex(encoded) != sha || record.Manifest.Tool.Name != name || record.Manifest.Tool.Revision != revision || record.Manifest.PluginID != pluginID || record.ContentSHA256 != plan.ContentSHA256 {
		return plan, plugin.ErrCatalogInvalid
	}
	if err := record.ValidateSource(source); err != nil {
		return plan, err
	}
	plan.ArtifactBytes = len(source)
	if plan.State == string(plugin.StateActive) {
		plan.Blockers = append(plan.Blockers, "revision is active; explicitly retire it first")
	}
	if plan.State != string(plugin.StateActive) && plan.State != string(plugin.StateRetired) && plan.State != string(plugin.StateStaged) {
		return plan, plugin.ErrCatalogInvalid
	}
	var shared int
	if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_revisions WHERE content_sha=?`, plan.ContentSHA256).Scan(&shared); err != nil {
		return plan, err
	}
	if shared == 1 {
		plan.ReclaimableBytes = len(source)
	}
	// Validate every stored manifest, not only a lossy string search. References
	// protect the whole frozen toolset, including plugins not yet invoked.
	rows, err := query.QueryContext(ctx, `SELECT snapshot_id,manifest FROM runtime_snapshots ORDER BY snapshot_id`)
	if err != nil {
		return plan, err
	}
	matching := map[string]bool{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return plan, err
		}
		manifest, err := tool.DecodeSnapshotManifest(raw)
		if err != nil {
			rows.Close()
			return plan, err
		}
		actual, err := manifest.ID()
		if err != nil || actual != id {
			rows.Close()
			return plan, tool.ErrSnapshotUnavailable
		}
		for _, entry := range manifest.Tools {
			if entry.Descriptor.Name == name && entry.Descriptor.Revision == revision {
				matching[id] = true
			}
		}
	}
	if err := closeRetentionRows(rows); err != nil {
		return plan, err
	}
	for id := range matching {
		var count int
		if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_snapshot_refs WHERE snapshot_id=? AND released_at IS NULL`, id).Scan(&count); err != nil {
			return plan, err
		}
		if count > 0 {
			plan.Blockers = append(plan.Blockers, fmt.Sprintf("snapshot %s has %d retained consumers", id, count))
		}
	}
	legacy, err := untrackedPluginConsumers(ctx, query, matching)
	if err != nil {
		return plan, err
	}
	plan.Blockers = append(plan.Blockers, legacy...)
	sort.Strings(plan.Blockers)
	plan.ID = plan.Digest()
	return plan, nil
}

// A legacy consumer without a manifest cannot be proven unrelated to this
// revision. It is a global retention root until explicitly closed; never guess
// old versions from today's active registry or fabricate a legacy manifest.
func untrackedPluginConsumers(ctx context.Context, query readQuery, matching map[string]bool) ([]string, error) {
	var blockers []string
	rows, err := query.QueryContext(ctx, `SELECT DISTINCT session_id FROM session_events ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	var sessions []session.ID
	for rows.Next() {
		var id session.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		sessions = append(sessions, id)
	}
	if err := closeRetentionRows(rows); err != nil {
		return nil, err
	}
	for _, id := range sessions {
		turns, err := retentionTurns(ctx, query, id)
		if err != nil {
			return nil, err
		}
		for turnID, state := range turns {
			if state.closed {
				continue
			}
			consumer := retention.Consumer{Reference: tool.SnapshotReference{Kind: "turn", Scope: string(id), ID: turnID}, SnapshotID: state.snapshot}
			closed, err := loadRetentionReference(ctx, query, &consumer)
			if err != nil {
				return nil, err
			}
			if closed {
				continue
			}
			var known int
			if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_snapshots WHERE snapshot_id=?`, consumer.SnapshotID).Scan(&known); err != nil {
				return nil, err
			}
			if known == 0 || matching[consumer.SnapshotID] {
				blockers = append(blockers, fmt.Sprintf("unclosed turn %s/%s needs this revision or has unknown legacy identity", id, turnID))
			}
		}
	}
	rows, err = query.QueryContext(ctx, `SELECT team_id,idempotency_key,runtime_snapshot_id FROM flow_runs ORDER BY flow_id`)
	if err != nil {
		return nil, err
	}
	var flows []retention.Consumer
	for rows.Next() {
		consumer := retention.Consumer{Reference: tool.SnapshotReference{Kind: "flow"}}
		if err := rows.Scan(&consumer.Reference.Scope, &consumer.Reference.ID, &consumer.SnapshotID); err != nil {
			rows.Close()
			return nil, err
		}
		flows = append(flows, consumer)
	}
	if err := closeRetentionRows(rows); err != nil {
		return nil, err
	}
	for _, consumer := range flows {
		closed, err := loadRetentionReference(ctx, query, &consumer)
		if err != nil {
			return nil, err
		}
		if closed {
			continue
		}
		var known int
		if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_snapshots WHERE snapshot_id=?`, consumer.SnapshotID).Scan(&known); err != nil {
			return nil, err
		}
		if known == 0 || matching[consumer.SnapshotID] {
			blockers = append(blockers, "unclosed Flow "+consumer.Reference.Scope+"/"+consumer.Reference.ID+" needs this revision or has unknown legacy identity")
		}
	}
	var orphans int
	if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_runs g WHERE state='running' AND NOT EXISTS(SELECT 1 FROM goal_rounds r JOIN session_events e ON e.session_id=g.session_id AND e.correlation_id=r.turn_id AND e.event_type='turn.started' WHERE r.goal_id=g.goal_id AND r.round_number=g.current_round)`).Scan(&orphans); err != nil {
		return nil, err
	}
	if orphans > 0 {
		blockers = append(blockers, "running Goal without a durable current turn; inspect and abandon or finish first")
	}
	return blockers, nil
}
