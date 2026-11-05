package workspacedomain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/orchestration"
)

const (
	leaseColumns            = `lease_id, instance_id, team_id, agent_id, run_id, path, mode, state, epoch, correlation_id, idempotency_key, release_reason, created_at, updated_at, released_at`
	abandonedReason         = "holder instance exited"
	initialLeaseRetryDelay  = 10 * time.Millisecond
	maximumLeaseRetryDelay  = 250 * time.Millisecond
	conflictingLeaseColumns = `l.lease_id, l.instance_id, l.agent_id, l.path, l.mode, i.pid, i.hostname`
)

type (
	// OwnerValidator confirms that a lease requester is a registered Agent of
	// its Team. Agents live in per-Session databases, outside this store.
	OwnerValidator func(context.Context, orchestration.TeamID, orchestration.AgentID) error

	// LeaseStore grants workspace-wide leases on behalf of one runtime
	// instance. Conflicts are checked against every instance of the domain.
	LeaseStore struct {
		db         *sql.DB
		instanceID string
		locks      string
		validate   OwnerValidator
	}

	// heldLease is an active lease that overlaps a request.
	heldLease struct {
		leaseID    string
		instanceID string
		agentID    string
		path       string
		mode       orchestration.LeaseMode
		pid        int
		hostname   string
	}

	// holderProbe memoizes liveness probes within one transaction and records
	// exited instances whose lock files can be removed after commit.
	holderProbe struct {
		store  *LeaseStore
		alive  map[string]bool
		exited []string
	}

	rowScanner interface {
		Scan(...any) error
	}
)

// AcquireLease grants a lease when no live holder has an overlapping one.
// Leases of exited holders are reclaimed on the way. While live holders
// conflict, the request polls with backoff for at most spec.Wait.
func (store *LeaseStore) AcquireLease(ctx context.Context, spec orchestration.LeaseSpec) (orchestration.WorkspaceLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	// Validate checks the trimmed path, so store exactly that path.
	spec.Path = strings.TrimSpace(spec.Path)
	if spec.Path != orchestration.WorkspaceRootLeasePath {
		spec.Path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.ReplaceAll(spec.Path, `\`, "/"))))
	}
	if store.validate != nil {
		if err := store.validate(ctx, spec.TeamID, spec.AgentID); err != nil {
			return orchestration.WorkspaceLease{}, err
		}
	}
	requestHash := hashLeaseRequest(spec)
	deadline := time.Now().Add(spec.Wait)
	delay := initialLeaseRetryDelay
	for {
		lease, err := store.tryAcquire(ctx, spec, requestHash)
		if !errors.Is(err, orchestration.ErrLeaseConflict) {
			return lease, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return orchestration.WorkspaceLease{}, err
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return orchestration.WorkspaceLease{}, errors.Join(ctx.Err(), err)
		case <-timer.C:
		}
		delay = min(2*delay, maximumLeaseRetryDelay)
	}
}

// tryAcquire makes one attempt inside a single write transaction, so the
// conflict check, reclamation and grant are atomic across instances.
func (store *LeaseStore) tryAcquire(ctx context.Context, spec orchestration.LeaseSpec, requestHash string) (orchestration.WorkspaceLease, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("begin workspace lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if spec.IdempotencyKey != "" {
		// A key names one request. Leases are never shared between attempts:
		// a replay after a crash acquires a fresh lease.
		var storedHash string
		err := tx.QueryRowContext(ctx, `SELECT request_hash FROM leases WHERE team_id = ? AND idempotency_key = ? AND idempotency_key <> '' ORDER BY created_at DESC LIMIT 1`,
			string(spec.TeamID), spec.IdempotencyKey).Scan(&storedHash)
		if err == nil && storedHash != requestHash {
			return orchestration.WorkspaceLease{}, fmt.Errorf("%w: lease key %q", orchestration.ErrIdempotencyConflict, spec.IdempotencyKey)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return orchestration.WorkspaceLease{}, fmt.Errorf("inspect lease idempotency key: %w", err)
		}
	}
	probe := &holderProbe{store: store, alive: make(map[string]bool)}
	conflict, err := store.liveConflict(ctx, tx, spec, probe)
	if err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	if conflict != nil {
		// Keep reclaimed leases of exited holders even though a live
		// conflict remains.
		if err := tx.Commit(); err != nil {
			return orchestration.WorkspaceLease{}, fmt.Errorf("commit lease reclamation: %w", err)
		}
		probe.removeExited()
		return orchestration.WorkspaceLease{}, conflict.err(spec.Path)
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, `UPDATE meta SET value = value + 1 WHERE key = 'lease_epoch' RETURNING value`).Scan(&epoch); err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("advance lease epoch: %w", err)
	}
	leaseID, err := orchestration.NewLeaseID()
	if err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	now := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO leases(lease_id, instance_id, team_id, agent_id, run_id, path, path_key, mode, state, epoch, correlation_id, idempotency_key, request_hash, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`,
		string(leaseID), store.instanceID, string(spec.TeamID), string(spec.AgentID), string(spec.RunID), spec.Path, pathKey(spec.Path), string(spec.Mode),
		epoch, spec.CorrelationID, spec.IdempotencyKey, requestHash, now, now); err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("insert workspace lease: %w", err)
	}
	lease, err := leaseByID(ctx, tx, leaseID)
	if err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("commit workspace lease: %w", err)
	}
	probe.removeExited()
	return lease, nil
}

// liveConflict returns the first overlapping lease whose holder is alive.
// Overlapping leases of exited holders are reclaimed inside tx.
func (store *LeaseStore) liveConflict(ctx context.Context, tx *sql.Tx, spec orchestration.LeaseSpec, probe *holderProbe) (*heldLease, error) {
	key := pathKey(spec.Path)
	rows, err := tx.QueryContext(ctx, `SELECT `+conflictingLeaseColumns+` FROM leases l JOIN instances i ON i.instance_id = l.instance_id
WHERE l.state = 'active' AND (l.path_key = ? OR ? = ? OR l.path_key = ?
    OR substr(l.path_key, 1, length(?) + 1) = ? || '/'
    OR substr(?, 1, length(l.path_key) + 1) = l.path_key || '/')
ORDER BY l.created_at, l.lease_id`,
		orchestration.WorkspaceRootLeasePath, key, orchestration.WorkspaceRootLeasePath, key, key, key, key)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace lease conflicts: %w", err)
	}
	var overlapping []heldLease
	for rows.Next() {
		var held heldLease
		var mode string
		if err := rows.Scan(&held.leaseID, &held.instanceID, &held.agentID, &held.path, &mode, &held.pid, &held.hostname); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan workspace lease conflict: %w", err)
		}
		held.mode = orchestration.LeaseMode(mode)
		overlapping = append(overlapping, held)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close workspace lease conflicts: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace lease conflicts: %w", err)
	}
	for index := range overlapping {
		held := &overlapping[index]
		if spec.Mode == orchestration.LeaseRead && held.mode == orchestration.LeaseRead {
			continue
		}
		alive, err := probe.holderAlive(ctx, tx, held.instanceID)
		if err != nil {
			return nil, err
		}
		if alive {
			return held, nil
		}
	}
	return nil, nil
}

// err describes a live conflict so the model and the user can see who holds
// the path.
func (held *heldLease) err(requested string) error {
	return fmt.Errorf("%w: path %q conflicts with %s lease %s on %q held by Agent %s (instance %s, pid %d on %s)",
		orchestration.ErrLeaseConflict, requested, held.mode, held.leaseID, held.path, held.agentID, held.instanceID, held.pid, held.hostname)
}

// ReleaseLease ends a lease held by agentID. The holder instance releases its
// own leases; another instance may only reclaim one whose holder has exited.
func (store *LeaseStore) ReleaseLease(ctx context.Context, leaseID orchestration.LeaseID, agentID orchestration.AgentID, reason string) (orchestration.WorkspaceLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := leaseID.Validate(); err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("invalid lease ID: %w", err)
	}
	if err := agentID.Validate(); err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("invalid lease Agent: %w", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("begin lease release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	lease, err := leaseByID(ctx, tx, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WorkspaceLease{}, fmt.Errorf("%w: lease %s", orchestration.ErrNotFound, leaseID)
	}
	if err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	if lease.AgentID != agentID {
		return orchestration.WorkspaceLease{}, fmt.Errorf("%w: lease is owned by %s", orchestration.ErrInvalidEvent, lease.AgentID)
	}
	if lease.State != orchestration.LeaseActive {
		return lease, tx.Commit()
	}
	probe := &holderProbe{store: store, alive: make(map[string]bool)}
	if lease.InstanceID != store.instanceID {
		alive, err := probe.holderAlive(ctx, tx, lease.InstanceID)
		if err != nil {
			return orchestration.WorkspaceLease{}, err
		}
		if alive {
			return orchestration.WorkspaceLease{}, fmt.Errorf("%w: lease %s is held by live instance %s", orchestration.ErrInvalidEvent, leaseID, lease.InstanceID)
		}
	} else {
		now := formatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'released', release_reason = ?, updated_at = ?, released_at = ? WHERE lease_id = ? AND state = 'active'`,
			reason, now, now, string(leaseID)); err != nil {
			return orchestration.WorkspaceLease{}, fmt.Errorf("release workspace lease: %w", err)
		}
	}
	updated, err := leaseByID(ctx, tx, leaseID)
	if err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	if err := tx.Commit(); err != nil {
		return orchestration.WorkspaceLease{}, fmt.Errorf("commit lease release: %w", err)
	}
	probe.removeExited()
	return updated, nil
}

// ListLeases returns leases of this workspace in grant order.
func (store *LeaseStore) ListLeases(ctx context.Context, query orchestration.LeaseQuery) ([]orchestration.WorkspaceLease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var conditions []string
	var args []any
	if query.TeamID != "" {
		if err := query.TeamID.Validate(); err != nil {
			return nil, fmt.Errorf("lease query team: %w", err)
		}
		conditions = append(conditions, "team_id = ?")
		args = append(args, string(query.TeamID))
	}
	if query.AgentID != "" {
		if err := query.AgentID.Validate(); err != nil {
			return nil, fmt.Errorf("lease query Agent: %w", err)
		}
		conditions = append(conditions, "agent_id = ?")
		args = append(args, string(query.AgentID))
	}
	if len(query.States) > 0 {
		placeholders := make([]string, len(query.States))
		for index, state := range query.States {
			placeholders[index] = "?"
			args = append(args, string(state))
		}
		conditions = append(conditions, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	statement := `SELECT ` + leaseColumns + ` FROM leases`
	if len(conditions) > 0 {
		statement += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	rows, err := store.db.QueryContext(ctx, statement+` ORDER BY created_at ASC, lease_id ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace leases: %w", err)
	}
	defer rows.Close()
	leases := make([]orchestration.WorkspaceLease, 0)
	for rows.Next() {
		lease, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

// ReapAbandonedLeases reclaims the leases of every exited instance. Probing
// happens before the write transaction; an exited instance cannot revive.
func (store *LeaseStore) ReapAbandonedLeases(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := store.db.QueryContext(ctx, `SELECT instance_id FROM instances WHERE state = 'live' AND instance_id <> ? ORDER BY started_at`, store.instanceID)
	if err != nil {
		return 0, fmt.Errorf("list coordination instances: %w", err)
	}
	probe := &holderProbe{store: store}
	var exited []string
	for rows.Next() {
		var instanceID string
		if err := rows.Scan(&instanceID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan coordination instance: %w", err)
		}
		if !probe.probe(instanceID) {
			exited = append(exited, instanceID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("list coordination instances: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close coordination instances: %w", err)
	}
	if len(exited) == 0 {
		return 0, nil
	}
	count := 0
	err = withTx(ctx, store.db, "lease reclamation", func(tx *sql.Tx) error {
		for _, instanceID := range exited {
			reclaimed, err := reclaimInstance(ctx, tx, instanceID)
			if err != nil {
				return err
			}
			count += reclaimed
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	probe.exited = exited
	probe.removeExited()
	return count, nil
}

// holderAlive reports whether instanceID may still act on its leases and
// reclaims everything it holds once it has exited.
func (probe *holderProbe) holderAlive(ctx context.Context, tx *sql.Tx, instanceID string) (bool, error) {
	if alive, known := probe.alive[instanceID]; known {
		return alive, nil
	}
	alive := probe.probe(instanceID)
	probe.alive[instanceID] = alive
	if alive {
		return true, nil
	}
	if _, err := reclaimInstance(ctx, tx, instanceID); err != nil {
		return false, err
	}
	probe.exited = append(probe.exited, instanceID)
	return false, nil
}

// probe treats this instance and probe failures as alive: reclaiming a lease
// of a live holder would admit concurrent writers. The lock files are the only
// authority; registered host names are diagnostic and may change over time.
func (probe *holderProbe) probe(instanceID string) bool {
	if instanceID == probe.store.instanceID {
		return true
	}
	alive, err := probeInstance(probe.store.locks, instanceID)
	return alive || err != nil
}

// removeExited deletes lock files of reclaimed instances after commit.
func (probe *holderProbe) removeExited() {
	for _, instanceID := range probe.exited {
		removeInstanceFiles(probe.store.locks, instanceID)
	}
	probe.exited = nil
}

// reclaimInstance marks an exited instance dead and abandons its leases.
func reclaimInstance(ctx context.Context, tx *sql.Tx, instanceID string) (int, error) {
	now := formatTime(time.Now())
	result, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'abandoned', release_reason = ?, updated_at = ?, released_at = ? WHERE instance_id = ? AND state = 'active'`,
		abandonedReason, now, now, instanceID)
	if err != nil {
		return 0, fmt.Errorf("reclaim leases of instance %s: %w", instanceID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE instances SET state = 'dead', ended_at = ? WHERE instance_id = ? AND state = 'live'`, now, instanceID); err != nil {
		return 0, fmt.Errorf("mark instance %s exited: %w", instanceID, err)
	}
	count, err := result.RowsAffected()
	return int(count), err
}

// hashLeaseRequest identifies the request an idempotency key names. Wait is
// excluded because it does not change what the lease covers.
func hashLeaseRequest(spec orchestration.LeaseSpec) string {
	request := struct {
		TeamID  orchestration.TeamID    `json:"team_id"`
		AgentID orchestration.AgentID   `json:"agent_id"`
		RunID   orchestration.RunID     `json:"run_id"`
		Path    string                  `json:"path"`
		Mode    orchestration.LeaseMode `json:"mode"`
	}{TeamID: spec.TeamID, AgentID: spec.AgentID, RunID: spec.RunID, Path: spec.Path, Mode: spec.Mode}
	encoded, _ := json.Marshal(request)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// pathKey folds case so that paths naming one file on a case-insensitive
// filesystem conflict; on case-sensitive filesystems this only adds waits.
func pathKey(path string) string {
	return strings.ToLower(path)
}

// leaseByID loads one lease inside the caller's transaction.
func leaseByID(ctx context.Context, tx *sql.Tx, leaseID orchestration.LeaseID) (orchestration.WorkspaceLease, error) {
	return scanLease(tx.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM leases WHERE lease_id = ?`, string(leaseID)))
}

// scanLease decodes one row selected with leaseColumns.
func scanLease(scanner rowScanner) (orchestration.WorkspaceLease, error) {
	var lease orchestration.WorkspaceLease
	var leaseID, teamID, agentID, runID, mode, state, createdAt, updatedAt string
	var epoch int64
	var releasedAt sql.NullString
	if err := scanner.Scan(&leaseID, &lease.InstanceID, &teamID, &agentID, &runID, &lease.Path, &mode, &state, &epoch,
		&lease.CorrelationID, &lease.IdempotencyKey, &lease.ReleaseReason, &createdAt, &updatedAt, &releasedAt); err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	lease.ID = orchestration.LeaseID(leaseID)
	lease.TeamID = orchestration.TeamID(teamID)
	lease.AgentID = orchestration.AgentID(agentID)
	lease.RunID = orchestration.RunID(runID)
	lease.Mode = orchestration.LeaseMode(mode)
	lease.State = orchestration.LeaseState(state)
	lease.Epoch = uint64(epoch)
	var err error
	if lease.CreatedAt, err = parseTime(createdAt); err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	if lease.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return orchestration.WorkspaceLease{}, err
	}
	if releasedAt.Valid && releasedAt.String != "" {
		value, err := parseTime(releasedAt.String)
		if err != nil {
			return orchestration.WorkspaceLease{}, err
		}
		lease.ReleasedAt = &value
	}
	return lease, nil
}
