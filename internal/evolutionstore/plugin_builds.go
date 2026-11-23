package evolutionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
	"github.com/vkviyu/go-self-evolving-agent/internal/pluginbuild"
	"github.com/vkviyu/go-self-evolving-agent/internal/retention"
)

const (
	maxBuildResultBytes = 256 << 10
	maxBuildStoreBytes  = 64 << 20
)

type buildRecord struct {
	Request     document  `json:"request"`
	Result      *document `json:"result,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

func decodeBuild(id string, record buildRecord) (pluginbuild.Request, *pluginbuild.Result, error) {
	var request pluginbuild.Request
	if record.Request.Digest != id || retention.Digest(record.Request.Data) != id {
		return request, nil, errors.New("build request checksum mismatch")
	}
	if err := json.Unmarshal(record.Request.Data, &request); err != nil {
		return request, nil, err
	}
	if record.Result == nil {
		return request, nil, nil
	}
	if retention.Digest(record.Result.Data) != record.Result.Digest {
		return request, nil, errors.New("build result checksum mismatch")
	}
	var result pluginbuild.Result
	err := json.Unmarshal(record.Result.Data, &result)
	return request, &result, err
}

// BeginPluginBuild durably reserves the request before any compiler work. An
// incomplete reservation remains non-replayable even after replacing the runtime database.
func (s *Store) BeginPluginBuild(ctx context.Context, request pluginbuild.Request) (string, *pluginbuild.Result, error) {
	if err := request.Validate(); err != nil {
		return "", nil, err
	}
	doc, err := encode(request)
	if err != nil {
		return "", nil, err
	}
	var completed *pluginbuild.Result
	err = s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		// A proven root conflict may run this callback again.
		completed = nil
		key := recordKey("plugin-build", doc.Digest)
		var record buildRecord
		found, err := tx.Get(key, &record)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(record.Request.Data, doc.Data) || record.Request.Digest != doc.Digest {
				return errors.New("plugin build request integrity mismatch")
			}
			_, result, err := decodeBuild(doc.Digest, record)
			if err != nil {
				return err
			}
			if result == nil {
				return errors.New("build was interrupted or is running; inspect its evidence and submit a new revision instead of replaying it")
			}
			completed = result
			return nil
		}
		used, err := buildBytes(tx)
		if err != nil {
			return err
		}
		if used+int64(len(doc.Data))+maxBuildResultBytes > maxBuildStoreBytes {
			return errors.New("build evidence store reached 64 MiB; preserve/export evidence before further proposals")
		}
		if err := tx.Create(key, buildRecord{Request: doc, CreatedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return tx.Put(recordKey("plugin-build-bytes"), used+int64(len(doc.Data)))
	})
	if err != nil {
		return "", nil, err
	}
	return doc.Digest, completed, nil
}

// The counter replaces SUM(length(request)+length(result)), and changes in the
// same CAS transaction as each build. It contains no copied business payload.
func buildBytes(tx *ateauthority.Tx) (int64, error) {
	var used int64
	_, err := tx.Get(recordKey("plugin-build-bytes"), &used)
	if err == nil && (used < 0 || used > int64(^uint64(0)>>1)-maxBuildStoreBytes) {
		err = errors.New("invalid build evidence byte count")
	}
	return used, err
}

func (s *Store) FinishPluginBuild(ctx context.Context, result pluginbuild.Result) error {
	doc, err := encode(result)
	if err != nil {
		return err
	}
	if len(doc.Data) > maxBuildResultBytes {
		return errors.New("build evidence exceeds 256 KiB")
	}
	return s.authority.Update(ctx, func(tx *ateauthority.Tx) error {
		key := recordKey("plugin-build", result.BuildID)
		var record buildRecord
		found, err := tx.Get(key, &record)
		if err != nil {
			return err
		}
		if !found {
			return missing("plugin build", result.BuildID)
		}
		if record.Result != nil {
			return fmt.Errorf("%w: plugin build result is immutable", orchestration.ErrIdempotencyConflict)
		}
		if _, _, err := decodeBuild(result.BuildID, record); err != nil {
			return err
		}
		used, err := buildBytes(tx)
		if err != nil {
			return err
		}
		record.Result, record.CompletedAt = &doc, time.Now().UTC()
		if err := tx.Put(key, record); err != nil {
			return err
		}
		return tx.Put(recordKey("plugin-build-bytes"), used+int64(len(doc.Data)))
	})
}

func (s *Store) PluginBuild(ctx context.Context, id string) (request pluginbuild.Request, result *pluginbuild.Result, err error) {
	err = s.authority.View(ctx, func(tx *ateauthority.Tx) error {
		var record buildRecord
		found, err := tx.Get(recordKey("plugin-build", id), &record)
		if err != nil {
			return err
		}
		if !found {
			return missing("plugin build", id)
		}
		request, result, err = decodeBuild(id, record)
		return err
	})
	return
}
