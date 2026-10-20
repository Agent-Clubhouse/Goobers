// Package diagnostictmp owns private raw diagnostics outside workspaces.
// Each scope holds an OS lease for its lifetime. A serialized sweep reclaims
// only unlocked scopes, including those abandoned by another crashed daemon.
package diagnostictmp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/safepath"
)

const (
	namespaceName        = "goobers-diagnostics"
	namespaceLockName    = ".lock"
	scopePrefix          = "scope-"
	leaseSuffix          = ".lease"
	namespaceLockTimeout = 2 * time.Second
	lockRetryInterval    = 10 * time.Millisecond
)

var errNamespaceBusy = errors.New("diagnostics namespace lock timed out")

// Scope owns one private diagnostic directory. The lease is a sibling file,
// outside Dir, so granting a harness write access to Dir cannot alter it.
type Scope struct {
	mu    sync.Mutex
	root  string
	dir   string
	lease *lock.Handle
}

// Establish sweeps abandoned scopes and creates a new leased directory. Every
// capture reaches the sweep, bounding crash leftovers even without a daemon
// restart. Other processes' active scopes remain locked and are skipped.
func Establish(root string) (*Scope, error) {
	namespace, guard, err := acquireNamespace(root, namespaceLockTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = guard.Release() }()
	if _, err := sweepLocked(namespace); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(namespace, scopePrefix)
	if err != nil {
		return nil, err
	}
	leasePath := dir + leaseSuffix
	if err := createLockFile(namespace, filepath.Base(leasePath)); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	lease, err := lock.TryAcquireExisting(leasePath)
	if err != nil {
		_ = os.RemoveAll(dir)
		_ = os.Remove(leasePath)
		return nil, err
	}
	return &Scope{root: filepath.Dir(namespace), dir: dir, lease: lease}, nil
}

// Dir returns the directory exclusively owned by this scope.
func (s *Scope) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// Reclaim closes the lease before removing files, while still holding the
// namespace lock. This works on Windows and prevents another sweeper from
// confusing the closed lease with a scope whose cleanup is still in progress.
// If the namespace lock cannot be obtained, releasing the lease leaves an
// abandoned scope for the next sweep without leaking a process-owned handle.
func (s *Scope) Reclaim() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	namespace, guard, err := acquireNamespace(s.root, namespaceLockTimeout)
	if err != nil {
		return errors.Join(err, s.lease.Release())
	}
	defer func() { _ = guard.Release() }()
	if err := s.lease.Release(); err != nil {
		return err
	}
	if err := removeScope(namespace, filepath.Base(s.dir)); err != nil {
		return err
	}
	s.dir = ""
	return nil
}

// Sweep removes abandoned scopes only. Unlike ephemeral stage temp sweeping,
// this is safe while other processes are actively capturing diagnostics.
func Sweep(root string) (int, error) {
	namespace, guard, err := acquireNamespace(root, namespaceLockTimeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = guard.Release() }()
	return sweepLocked(namespace)
}

func acquireNamespace(root string, timeout time.Duration) (string, *lock.Handle, error) {
	if root == "" {
		root = os.TempDir()
	}
	name, err := namespaceDirectoryName()
	if err != nil {
		return "", nil, err
	}
	namespace, err := safepath.MkdirLeaf(root, name, 0o700)
	if err != nil {
		return "", nil, fmt.Errorf("prepare diagnostics namespace: %w", err)
	}
	info, err := os.Stat(namespace)
	if err != nil {
		return "", nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", nil, errors.New("diagnostics namespace must be owner-private")
	}
	if err := createLockFile(namespace, namespaceLockName); err != nil {
		return "", nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		guard, err := lock.TryAcquireExisting(filepath.Join(namespace, namespaceLockName))
		if !errors.Is(err, lock.ErrHeld) {
			return namespace, guard, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", nil, errNamespaceBusy
		}
		time.Sleep(min(lockRetryInterval, remaining))
	}
}

// Unix temp roots are shared between users; a single mode-0700 namespace
// would prevent every user after the first from starting a capture. Numeric
// UIDs work even in containers without passwd entries; Windows uses its SID.
func namespaceDirectoryName() (string, error) {
	identity := strconv.Itoa(os.Getuid())
	if runtime.GOOS == "windows" {
		owner, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("resolve diagnostics owner: %w", err)
		}
		identity = owner.Uid
	}
	return namespaceForUserID(identity), nil
}

func namespaceForUserID(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%s-%x", namespaceName, digest[:16])
}

// The namespace and lock leaves must not be symlinks or special files. Lock
// files are never removed while an owner might be waiting on their inode.
func createLockFile(namespace, name string) error {
	path, err := safepath.Resolve(namespace, name, false)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		return file.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("diagnostics lock must be a regular file")
	}
	return nil
}

func sweepLocked(namespace string) (int, error) {
	entries, err := os.ReadDir(namespace)
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, scopePrefix) {
			continue
		}
		// Also collect a lease left behind after its directory was removed.
		if !entry.IsDir() {
			if !entry.Type().IsRegular() || !strings.HasSuffix(name, leaseSuffix) {
				continue
			}
			name = strings.TrimSuffix(name, leaseSuffix)
		}
		didRemove, err := sweepScope(namespace, name)
		if err != nil {
			failures = append(failures, err)
		} else if didRemove {
			removed++
		}
	}
	return removed, errors.Join(failures...)
}

func sweepScope(namespace, name string) (bool, error) {
	leasePath, err := safepath.Resolve(namespace, name+leaseSuffix, false)
	if err != nil {
		return false, err
	}
	info, statErr := os.Lstat(leasePath)
	if statErr == nil && !info.Mode().IsRegular() {
		return false, errors.New("diagnostics lease must be a regular file")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	if statErr == nil {
		lease, err := lock.TryAcquireExisting(leasePath)
		if errors.Is(err, lock.ErrHeld) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := lease.Release(); err != nil {
			return false, err
		}
	}
	_, dirErr := os.Lstat(filepath.Join(namespace, name))
	if errors.Is(statErr, os.ErrNotExist) && errors.Is(dirErr, os.ErrNotExist) {
		return false, nil
	}
	return true, removeScope(namespace, name)
}

func removeScope(namespace, name string) error {
	dir, err := safepath.Resolve(namespace, name, false)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(namespace, name+leaseSuffix))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
