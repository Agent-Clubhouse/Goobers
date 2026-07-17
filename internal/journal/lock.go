package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireJournalLock(dir, target string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, fileLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s lock: %w", target, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: acquire %s lock: %w", target, err)
	}
	return f, nil
}

func releaseJournalLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
