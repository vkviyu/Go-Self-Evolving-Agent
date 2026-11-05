package sqlitestore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

var _ pluginbuild.BuildStore = (*Database)(nil)

// BeginPluginBuild reserves a content-addressed immutable request before the
// compiler is dispatched. An interrupted reservation is not silently rerun.
func (database *Database) BeginPluginBuild(ctx context.Context, request pluginbuild.Request) (string, *pluginbuild.Result, error) {
	if err := request.Validate(); err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return "", nil, err
	}
	id := retention.Digest(raw)
	tx, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	var existing, result []byte
	var requestSHA string
	var resultSHA sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT request,request_sha,result,result_sha FROM plugin_builds WHERE build_id=?`, id).Scan(&existing, &requestSHA, &result, &resultSHA)
	if err == nil {
		if !bytes.Equal(raw, existing) || requestSHA != id {
			return "", nil, errors.New("plugin build request integrity mismatch")
		}
		if len(result) == 0 {
			return "", nil, errors.New("build was interrupted or is running; inspect its evidence and submit a new revision instead of replaying it")
		}
		if !resultSHA.Valid || retention.Digest(result) != resultSHA.String {
			return "", nil, errors.New("build result integrity mismatch")
		}
		var completed pluginbuild.Result
		if err := json.Unmarshal(result, &completed); err != nil {
			return "", nil, err
		}
		return id, &completed, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	var retained int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(request)+COALESCE(length(result),0)),0) FROM plugin_builds`).Scan(&retained); err != nil {
		return "", nil, err
	}
	if retained+int64(len(raw))+(256<<10) > 64<<20 {
		return "", nil, errors.New("build evidence store reached 64 MiB; preserve/export evidence before further proposals")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_builds(build_id,request,request_sha,created_at) VALUES(?,?,?,?)`, id, raw, id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", nil, err
	}
	return id, nil, tx.Commit()
}

func (database *Database) FinishPluginBuild(ctx context.Context, result pluginbuild.Result) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(raw) > 256<<10 {
		return errors.New("build evidence exceeds 256 KiB")
	}
	updated, err := database.db.ExecContext(ctx, `UPDATE plugin_builds SET result=?,result_sha=?,completed_at=? WHERE build_id=? AND result IS NULL`, raw, retention.Digest(raw), time.Now().UTC().Format(time.RFC3339Nano), result.BuildID)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		var id string
		if err := database.db.QueryRowContext(ctx, `SELECT build_id FROM plugin_builds WHERE build_id=?`, result.BuildID).Scan(&id); err != nil {
			return recordError(err, "plugin build", result.BuildID)
		}
		return fmt.Errorf("%w: plugin build result is immutable", orchestration.ErrIdempotencyConflict)
	}
	return nil
}

func (database *Database) PluginBuild(ctx context.Context, id string) (pluginbuild.Request, *pluginbuild.Result, error) {
	var request pluginbuild.Request
	var raw, result []byte
	var requestSHA string
	var resultSHA sql.NullString
	if err := database.db.QueryRowContext(ctx, `SELECT request,request_sha,result,result_sha FROM plugin_builds WHERE build_id=?`, id).Scan(&raw, &requestSHA, &result, &resultSHA); err != nil {
		return request, nil, recordError(err, "plugin build", id)
	}
	if retention.Digest(raw) != id || requestSHA != id {
		return request, nil, errors.New("build request checksum mismatch")
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, nil, err
	}
	if len(result) == 0 {
		return request, nil, nil
	}
	if !resultSHA.Valid || retention.Digest(result) != resultSHA.String {
		return request, nil, errors.New("build result checksum mismatch")
	}
	var completed pluginbuild.Result
	err := json.Unmarshal(result, &completed)
	return request, &completed, err
}
