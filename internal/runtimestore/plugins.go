package runtimestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
)

type PluginCatalog struct{ db *sql.DB }

var _ plugin.Catalog = (*PluginCatalog)(nil)

func (database *Database) Plugins() *PluginCatalog { return &PluginCatalog{db: database.db} }

func (catalog *PluginCatalog) Save(ctx context.Context, record plugin.RevisionRecord, source []byte) error {
	if err := record.ValidateSource(source); err != nil {
		return err
	}
	encoded, err := record.Encode()
	if err != nil {
		return err
	}
	tx, err := catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	name, revision := record.Manifest.Tool.Name, record.Manifest.Tool.Revision
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT tool_name,revision FROM plugin_revisions UNION ALL SELECT tool_name,revision FROM plugin_revision_tombstones) WHERE tool_name = ? AND revision = ?`, name, revision).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return plugin.ErrArtifactExists
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT tool_name,plugin_id FROM plugin_revisions UNION ALL SELECT tool_name,plugin_id FROM plugin_revision_tombstones) WHERE tool_name = ? AND plugin_id <> ?`, name, record.Manifest.PluginID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return plugin.ErrToolOwned
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(b.source)), 0) FROM plugin_revisions r JOIN plugin_blobs b USING(content_sha)`).Scan(&total); err != nil {
		return err
	}
	if total+int64(len(source)) > plugin.MaxCatalogBytes {
		return errors.New("plugin catalog retained artifact limit exceeded (128 MiB)")
	}
	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT source FROM plugin_blobs WHERE content_sha = ?`, record.ContentSHA256).Scan(&existing)
	if err == nil && !bytes.Equal(existing, source) {
		return plugin.ErrCatalogInvalid
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_blobs(content_sha, source) VALUES(?, ?)`, record.ContentSHA256, source); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_revisions(tool_name, revision, plugin_id, record, record_sha, content_sha, state) VALUES(?, ?, ?, ?, ?, ?, 'staged')`, name, revision, record.Manifest.PluginID, encoded, sha256Hex(encoded), record.ContentSHA256); err != nil {
		return err
	}
	return tx.Commit()
}

func (catalog *PluginCatalog) Load(ctx context.Context) ([]plugin.StoredArtifact, error) {
	rows, err := catalog.db.QueryContext(ctx, `SELECT r.tool_name, r.revision, r.plugin_id, r.record, r.record_sha, r.content_sha, r.state, b.source FROM plugin_revisions r LEFT JOIN plugin_blobs b USING(content_sha) ORDER BY r.tool_name, r.revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []plugin.StoredArtifact
	var total int64
	for rows.Next() {
		var item plugin.StoredArtifact
		var name, revision, pluginID, recordSHA, contentSHA string
		var encoded []byte
		if err := rows.Scan(&name, &revision, &pluginID, &encoded, &recordSHA, &contentSHA, &item.State, &item.Source); err != nil {
			return nil, err
		}
		if sha256Hex(encoded) != recordSHA {
			return nil, fmt.Errorf("%w: manifest checksum mismatch", plugin.ErrCatalogInvalid)
		}
		item.Record, err = plugin.DecodeRecord(encoded)
		if err != nil {
			return nil, err
		}
		if item.Record.Manifest.Tool.Name != name || item.Record.Manifest.Tool.Revision != revision || item.Record.Manifest.PluginID != pluginID || item.Record.ContentSHA256 != contentSHA {
			return nil, fmt.Errorf("%w: catalog identity mismatch", plugin.ErrCatalogInvalid)
		}
		if err := item.Record.ValidateSource(item.Source); err != nil {
			return nil, err
		}
		total += int64(len(item.Source))
		if total > plugin.MaxCatalogBytes {
			return nil, errors.New("plugin catalog retained artifact limit exceeded")
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (catalog *PluginCatalog) SetActive(ctx context.Context, name, expected, next string) error {
	tx, err := catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	err = tx.QueryRowContext(ctx, `SELECT revision FROM plugin_revisions WHERE tool_name = ? AND state = 'active'`, name).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current != expected {
		return plugin.ErrCatalogConflict
	}
	if next == expected {
		return errors.New("plugin publication must change the active revision")
	}
	if next != "" {
		var encoded, source []byte
		var checksum, pluginID, contentSHA string
		if err := tx.QueryRowContext(ctx, `SELECT r.record, r.record_sha, r.plugin_id, r.content_sha, b.source FROM plugin_revisions r JOIN plugin_blobs b USING(content_sha) WHERE r.tool_name = ? AND r.revision = ?`, name, next).Scan(&encoded, &checksum, &pluginID, &contentSHA, &source); err != nil {
			return fmt.Errorf("load plugin revision %s/%s: %w", name, next, err)
		}
		if sha256Hex(encoded) != checksum {
			return plugin.ErrCatalogInvalid
		}
		record, err := plugin.DecodeRecord(encoded)
		if err != nil {
			return err
		}
		if record.Manifest.Tool.Name != name || record.Manifest.Tool.Revision != next || record.Manifest.PluginID != pluginID || record.ContentSHA256 != contentSHA {
			return plugin.ErrCatalogInvalid
		}
		if err := record.ValidateSource(source); err != nil {
			return err
		}
	}
	if current != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_revisions SET state = 'retired' WHERE tool_name = ? AND revision = ? AND state = 'active'`, name, current); err != nil {
			return err
		}
	}
	if next != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_revisions SET state = 'active' WHERE tool_name = ? AND revision = ?`, name, next); err != nil {
			return err
		}
	}
	return tx.Commit()
}
