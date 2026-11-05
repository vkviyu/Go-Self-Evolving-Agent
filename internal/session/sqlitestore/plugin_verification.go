package sqlitestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/plugin"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

var _ plugin.TestReportStore = (*Database)(nil)

func (database *Database) SavePluginTest(ctx context.Context, report plugin.TestReport) (string, error) {
	if report.Tool == "" || report.Revision == "" || len(report.Results) == 0 {
		return "", errors.New("invalid plugin test report")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	id := retention.Digest(encoded)
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_test_reports(report_id,tool_name,revision,content_sha,report,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(report_id) DO NOTHING`, id, report.Tool, report.Revision, report.ContentSHA256, encoded, report.TestedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	var stored []byte
	if err := tx.QueryRowContext(ctx, `SELECT report FROM plugin_test_reports WHERE report_id=?`, id).Scan(&stored); err != nil {
		return "", recordError(err, "plugin test report", id)
	}
	if !bytes.Equal(stored, encoded) {
		return "", errors.New("plugin test report integrity mismatch")
	}
	return id, tx.Commit()
}

func (database *Database) PluginTest(ctx context.Context, id string) (plugin.TestReport, error) {
	var report plugin.TestReport
	var encoded []byte
	if err := database.db.QueryRowContext(ctx, `SELECT report FROM plugin_test_reports WHERE report_id=?`, id).Scan(&encoded); err != nil {
		return report, recordError(err, "plugin test report", id)
	}
	if retention.Digest(encoded) != id {
		return report, errors.New("plugin test report checksum mismatch")
	}
	err := json.Unmarshal(encoded, &report)
	return report, err
}
