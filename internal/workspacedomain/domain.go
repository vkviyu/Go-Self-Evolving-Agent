// Package workspacedomain coordinates every runtime instance that operates on
// one workspace directory. Instances that share a GSEA_HOME share one
// coordination database and see each other's leases; a user-level rendezvous
// lock rejects instances of a different GSEA_HOME on the same workspace.
package workspacedomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	workspacesDirectoryName = "workspaces"
	domainsMutexName        = "domains.mutex"
	// domainsMutexWait bounds setup; holders keep the mutex for a few
	// file and schema operations.
	domainsMutexWait = 30 * time.Second
	// teardownMutexWait is short because Close only needs the mutex to
	// remove an unused directory; a busy mutex just leaves it for reuse.
	teardownMutexWait = 2 * time.Second
)

var (
	// ErrForeignDomain reports a workspace coordinated by another GSEA_HOME.
	ErrForeignDomain = errors.New("workspace is coordinated by another GSEA_HOME")

	errLocked                = errors.New("lock is held by another file description")
	errRendezvousUnavailable = errors.New("rendezvous directory is unavailable")

	// retained keeps domains reachable until process exit so finalizers never
	// close descriptors whose locks still guard running workers.
	retained struct {
		sync.Mutex
		domains []*Domain
	}
)

type (
	// Options locates the coordination state of one workspace.
	Options struct {
		// HomeDirectory is the GSEA_HOME that owns the coordination database.
		HomeDirectory string
		// RendezvousDirectory holds cross-home rendezvous locks. Empty skips
		// cross-home exclusion.
		RendezvousDirectory string
		WorkspaceRoot       string
		// DataDirectory is recorded for diagnostics only.
		DataDirectory string
		// ValidateOwner, when set, rejects lease requests from unknown Agents.
		ValidateOwner OwnerValidator
	}

	// Status reports the identity and guarantees of an open domain.
	Status struct {
		Identity   Identity
		Directory  string
		InstanceID string
		// CrossHomeExclusion is false when the rendezvous lock was skipped;
		// Degraded then explains why.
		CrossHomeExclusion bool
		Degraded           string
	}

	// Domain is this process's membership in one workspace coordination
	// domain. It holds the instance lock that keeps its leases alive.
	Domain struct {
		// workspaces is the home-wide directory holding every domain and the
		// mutex that serializes their setup and teardown.
		workspaces string
		status     Status
		member     *membership
		locks      *instanceLocks
		db         *sql.DB
		leases     *LeaseStore
		closeOnce  sync.Once
		closeErr   error
	}
)

// Open joins the workspace's coordination domain and registers this process
// as a live instance. It fails closed when another GSEA_HOME coordinates the
// workspace, and degrades to same-home coordination when the rendezvous
// directory is unusable.
func Open(ctx context.Context, options Options) (*Domain, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(options.HomeDirectory) == "" {
		return nil, errors.New("workspace coordination requires a home directory")
	}
	identity, err := Identify(options.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	home, err := canonicalDirectory(options.HomeDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve coordination home: %w", err)
	}
	domain := &Domain{
		workspaces: filepath.Join(home, workspacesDirectoryName),
		status:     Status{Identity: identity, Directory: filepath.Join(home, workspacesDirectoryName, identity.ID)},
	}
	if strings.TrimSpace(options.RendezvousDirectory) == "" {
		domain.status.Degraded = "no rendezvous directory is configured"
	} else {
		domain.member, err = joinRendezvous(ctx, options.RendezvousDirectory, identity, home)
		switch {
		case errors.Is(err, errRendezvousUnavailable):
			domain.status.Degraded = err.Error()
		case err != nil:
			return nil, err
		default:
			domain.status.CrossHomeExclusion = true
		}
	}
	// Close takes the home-wide mutex, so failed setup is undone only after
	// start has released it.
	if err := domain.start(ctx, options); err != nil {
		_ = domain.Close()
		return nil, err
	}
	return domain, nil
}

// start creates or reuses the domain directory, opens the shared database
// and registers this instance. The home-wide mutex serializes it with the
// teardown in Close, which removes the directory after the last instance.
func (domain *Domain) start(ctx context.Context, options Options) error {
	if err := os.MkdirAll(domain.workspaces, 0o700); err != nil {
		return fmt.Errorf("create workspace coordination directory: %w", err)
	}
	mutex, err := acquireMutex(ctx, filepath.Join(domain.workspaces, domainsMutexName), domainsMutexWait)
	if err != nil {
		return fmt.Errorf("workspace coordination mutex: %w", err)
	}
	defer mutex.Close()
	if err := os.MkdirAll(domain.status.Directory, 0o700); err != nil {
		return fmt.Errorf("create workspace coordination directory: %w", err)
	}
	locksDirectory := filepath.Join(domain.status.Directory, instancesDirectoryName)
	if domain.locks, err = createInstanceLocks(locksDirectory); err != nil {
		return err
	}
	domain.status.InstanceID = domain.locks.id
	if domain.db, err = openDatabase(ctx, filepath.Join(domain.status.Directory, databaseName)); err != nil {
		return err
	}
	domain.leases = &LeaseStore{db: domain.db, instanceID: domain.locks.id, locks: locksDirectory, validate: options.ValidateOwner}
	hostname, _ := os.Hostname()
	if err := registerInstance(ctx, domain.db, domain.locks.id, hostname, options.DataDirectory, domain.status.Identity.Root, os.Getpid()); err != nil {
		return err
	}
	return pruneHistory(ctx, domain.db, time.Now())
}

// Leases returns the lease repository acting for this instance.
func (domain *Domain) Leases() *LeaseStore {
	if domain == nil {
		return nil
	}
	return domain.leases
}

// Status returns the identity and coordination guarantees of this domain.
func (domain *Domain) Status() Status {
	if domain == nil {
		return Status{}
	}
	return domain.status
}

// DescendantLock returns a descriptor to pass to a child process. While any
// process holds it, this instance counts as live and keeps its leases, even
// after the instance itself has died. The caller closes its copy after the
// child has been reaped.
func (domain *Domain) DescendantLock() (*os.File, error) {
	if domain == nil || domain.locks == nil {
		return nil, errors.New("workspace coordination domain is closed")
	}
	return domain.locks.descendantLock()
}

// Close releases this instance's leases, unregisters it, removes the domain
// directory when no other instance remains and leaves the rendezvous,
// exactly once.
func (domain *Domain) Close() error {
	if domain == nil {
		return nil
	}
	domain.closeOnce.Do(func() {
		var errs []error
		if domain.db != nil {
			errs = append(errs, closeInstance(context.Background(), domain.db, domain.locks.id), domain.db.Close())
		}
		errs = append(errs, domain.locks.close())
		if domain.locks != nil {
			domain.removeIfUnused()
		}
		errs = append(errs, domain.member.leave())
		domain.closeErr = errors.Join(errs...)
	})
	return domain.closeErr
}

// removeIfUnused deletes the domain directory after its last instance, so
// short-lived workspaces such as evaluation copies leave nothing behind. An
// instance that crashed keeps the directory while it or a descendant process
// is alive, because its leases still guard paths. The home-wide mutex keeps
// a concurrent start from reusing the directory during removal.
func (domain *Domain) removeIfUnused() {
	mutex, err := acquireMutex(context.Background(), filepath.Join(domain.workspaces, domainsMutexName), teardownMutexWait)
	if err != nil {
		return
	}
	defer mutex.Close()
	locks := filepath.Join(domain.status.Directory, instancesDirectoryName)
	entries, err := os.ReadDir(locks)
	if err != nil {
		return
	}
	for _, entry := range entries {
		id, _, _ := strings.Cut(entry.Name(), ".")
		if alive, err := probeInstance(locks, id); alive || err != nil {
			return
		}
	}
	_ = os.RemoveAll(domain.status.Directory)
}

// RetainUntilExit keeps this instance's leases and locks until the process
// exits, for callers whose workers may still mutate the workspace. The
// operating system releases the locks at exit; other instances then reclaim
// the leases.
func (domain *Domain) RetainUntilExit() {
	if domain == nil {
		return
	}
	retained.Lock()
	retained.domains = append(retained.domains, domain)
	retained.Unlock()
}

// canonicalDirectory creates a private directory if needed and resolves
// symlinks, so aliases of one GSEA_HOME name the same coordination domain.
func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// acquireMutex opens a mutex file, creating it if needed, and holds it.
// Mutex files are never unlinked; replacing an inode would admit two holders.
func acquireMutex(ctx context.Context, path string, wait time.Duration) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := holdMutex(ctx, file, wait); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// holdMutex polls for an exclusive lock on file. Holders keep these mutexes
// for a few file or schema operations, so exceeding wait means a stuck process.
func holdMutex(ctx context.Context, file *os.File, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for delay := 5 * time.Millisecond; ; delay = min(2*delay, 100*time.Millisecond) {
		err := tryLock(file, true)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errLocked) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("held by another process for over %s", wait)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
