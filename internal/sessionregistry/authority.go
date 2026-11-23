package sessionregistry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/ateauthority"
)

// Catalog maps stable user-facing session identities to their runtime resources.
// Conversation events are stored separately in native ATE sessions.
type Catalog interface {
	Create(context.Context, Session) error
	Get(context.Context, string) (Session, error)
	List(context.Context) ([]Session, error)
	UpdateState(context.Context, string, State, int) error
	ReconcileStopped(context.Context, string) (Session, error)
	Remove(context.Context, string) error
	Close() error
}

const catalogPrefix = "gsea/v1/session-catalog/"

// Authority retains no local business-state projection. Each Session is an
// independent ATE record in the host's operational authority namespace.
type Authority struct {
	records  *ateauthority.Store
	closeFn  func() error
	once     sync.Once
	closed   atomic.Bool
	closeErr error
}

// NewAuthority takes ownership of closeFn on success. It may be nil when the
// caller owns the underlying process and pending-request store.
func NewAuthority(records *ateauthority.Store, closeFn func() error) (*Authority, error) {
	if records == nil {
		return nil, errors.New("session authority is required")
	}
	return &Authority{records: records, closeFn: closeFn}, nil
}

func (r *Authority) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.closed.Store(true)
		if r.closeFn != nil {
			r.closeErr = r.closeFn()
		}
	})
	return r.closeErr
}

func (r *Authority) available() error {
	if r == nil || r.records == nil || r.closed.Load() {
		return errors.New("session registry is unavailable")
	}
	return nil
}

func catalogKey(id string) string {
	return catalogPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func catalogSession(tx *ateauthority.Tx, id string) (Session, error) {
	var item Session
	found, err := tx.Get(catalogKey(id), &item)
	if err != nil {
		return Session{}, err
	}
	if !found {
		return Session{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if item.ID != id {
		return Session{}, fmt.Errorf("%w: session catalog identity", ateauthority.ErrCorrupt)
	}
	return item, nil
}

func (r *Authority) Create(ctx context.Context, item Session) error {
	if err := r.available(); err != nil {
		return err
	}
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Name) == "" {
		return errors.New("session ID and name are required")
	}
	if strings.TrimSpace(item.Workspace) == "" || strings.TrimSpace(item.Runtime) == "" {
		return errors.New("session workspace and runtime directory are required")
	}
	if item.State == "" {
		item.State = StateCreated
	}
	if !validState(item.State) || item.PID < 0 || (item.State != StateRunning && item.PID != 0) {
		return errors.New("invalid session state or PID")
	}
	if _, err := r.Get(ctx, item.ID); err == nil {
		return fmt.Errorf("create session %q: %w", item.ID, ateauthority.ErrExists)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	var created []string
	for _, directory := range []string{item.Runtime, item.DataDirectory, item.TempDirectory, item.ToolState} {
		if strings.TrimSpace(directory) == "" {
			continue
		}
		if err := createResourceDirectory(directory, &created); err != nil {
			for i := len(created) - 1; i >= 0; i-- {
				_ = os.Remove(created[i])
			}
			return fmt.Errorf("create session resources: %w", err)
		}
	}
	// Filesystem work must stay outside retryable authority callbacks. Once a
	// submission starts, retain directories even on an unknown commit outcome.
	return r.records.Update(ctx, func(tx *ateauthority.Tx) error {
		return tx.Create(catalogKey(item.ID), item)
	})
}

func (r *Authority) Get(ctx context.Context, id string) (Session, error) {
	if err := r.available(); err != nil {
		return Session{}, err
	}
	result := make(chan Session, 1)
	err := r.records.View(ctx, func(tx *ateauthority.Tx) error {
		item, err := catalogSession(tx, id)
		if err == nil {
			result <- item
		}
		return err
	})
	if err != nil {
		return Session{}, err
	}
	select {
	case item := <-result:
		return item, nil
	default:
		return Session{}, errors.New("session authority read did not complete")
	}
}

func (r *Authority) List(ctx context.Context) ([]Session, error) {
	if err := r.available(); err != nil {
		return nil, err
	}
	result := make(chan []Session, 1)
	err := r.records.View(ctx, func(tx *ateauthority.Tx) error {
		items := make([]Session, 0)
		err := tx.List(catalogPrefix, func(key string, raw json.RawMessage) error {
			var item Session
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if item.ID == "" || key != catalogKey(item.ID) {
				return fmt.Errorf("%w: session catalog identity", ateauthority.ErrCorrupt)
			}
			items = append(items, item)
			return nil
		})
		if err == nil {
			result <- items
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	var items []Session
	select {
	case items = <-result:
	default:
		return nil, errors.New("session authority list did not complete")
	}
	slices.SortFunc(items, func(a, b Session) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return items, nil
}

func (r *Authority) UpdateState(ctx context.Context, id string, state State, pid int) error {
	if err := r.available(); err != nil {
		return err
	}
	if !validState(state) || pid < 0 || (state != StateRunning && pid != 0) {
		return errors.New("invalid session state or PID")
	}
	now := time.Now().UTC()
	return r.records.Update(ctx, func(tx *ateauthority.Tx) error {
		item, err := catalogSession(tx, id)
		if err != nil {
			return err
		}
		if state == StateCreated && item.State != StateCreated {
			return errors.New("cannot reset an existing session to created")
		}
		item.State, item.PID, item.UpdatedAt = state, pid, now
		return tx.Put(catalogKey(id), item)
	})
}

// Remove unregisters a stopped session. It never deletes conversation history
// or any runtime directory.
func (r *Authority) Remove(ctx context.Context, id string) error {
	if err := r.available(); err != nil {
		return err
	}
	return r.records.Update(ctx, func(tx *ateauthority.Tx) error {
		item, err := catalogSession(tx, id)
		if err != nil {
			return err
		}
		if item.State == StateRunning {
			return errors.New("cannot remove a running Session")
		}
		return tx.Delete(catalogKey(id))
	})
}

func (r *Authority) ReconcileStopped(ctx context.Context, id string) (Session, error) {
	item, err := r.Get(ctx, id)
	if err != nil || item.State != StateRunning {
		return item, err
	}
	alive, err := processAlive(item.PID)
	if err != nil || alive {
		return item, err
	}
	now := time.Now().UTC()
	err = r.records.Update(ctx, func(tx *ateauthority.Tx) error {
		current, err := catalogSession(tx, id)
		if err != nil {
			return err
		}
		if current.State != StateRunning || current.PID != item.PID || !current.UpdatedAt.Equal(item.UpdatedAt) {
			return nil
		}
		current.State, current.PID, current.UpdatedAt = StateStopped, 0, now
		return tx.Put(catalogKey(id), current)
	})
	if err != nil {
		return Session{}, err
	}
	return r.Get(ctx, id)
}

var _ Catalog = (*Authority)(nil)
