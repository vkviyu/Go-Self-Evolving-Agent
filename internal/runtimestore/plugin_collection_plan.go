package runtimestore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
	"github.com/vkviyu/go-self-evolving-agent/internal/tool"
)

// Collection audits frozen toolsets and runtime references in one transaction.
func planPluginCollectionResources(ctx context.Context, query readQuery, name, revision string) (retention.CollectionPlan, error) {
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
	sort.Strings(plan.Blockers)
	plan.ID = plan.Digest()
	return plan, nil
}
