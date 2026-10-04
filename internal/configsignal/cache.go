// Package configsignal avoids reading unchanged configuration inputs on every
// poll. Metadata is a hint only; callers still validate with content digests.
package configsignal

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// AuditInterval bounds detection latency when a filesystem aliases timestamps
// or returns stale metadata. No notification delivery guarantees are required.
const AuditInterval = 30 * time.Second

// SettleDelay is how long an input must sit unmodified before its metadata can
// vouch for its content. Filesystems stamp mtime and ctime from a coarse
// kernel clock (4ms ticks on ext4 with HZ=250, whole seconds on others), so an
// edit made within one tick of an earlier change, or of the snapshot that
// observed it, carries identical metadata and is invisible to the comparison.
// Snapshots touching anything modified more recently than this are not cached
// and the next poll reads content again (the "racy timestamp" rule git uses).
const SettleDelay = 50 * time.Millisecond

// Cache is serialized by its caller. Its zero value is ready to use.
type Cache struct {
	digest  string
	checked time.Time
	files   map[string]os.FileInfo
}

// Invalidate forces the next check to read content, as required by explicit
// apply requests even when the filesystem cannot signal a change.
func (c *Cache) Invalidate() { c.files = nil }

// Digest returns a cached digest only while all previously discovered paths
// have unchanged metadata and the periodic content audit is not due. Roots
// include directories whose membership matters, including absent optional
// roots. hash must observe referenced paths outside those trees before reading
// them. Errors and concurrent edits never establish a new cached generation.
func (c *Cache) Digest(now time.Time, roots []string, hash func(observe func(string)) (string, error)) (string, error) {
	if c.files != nil && !now.Before(c.checked) && now.Sub(c.checked) < AuditInterval && unchanged(c.files) {
		return c.digest, nil
	}
	files := make(map[string]os.FileInfo)
	valid := true
	observe := func(path string) {
		if _, exists := files[path]; exists {
			return
		}
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			valid = false
		}
		files[path] = info
	}
	for _, root := range roots {
		observe(root)
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			observe(path)
			return nil
		}); err != nil {
			valid = false
		}
	}
	digest, err := hash(observe)
	c.files = nil
	if err == nil && valid && unchanged(files) && settled(files, time.Now().Add(-SettleDelay)) {
		c.digest, c.checked, c.files = digest, now, files
	}
	return digest, err
}

func unchanged(files map[string]os.FileInfo) bool {
	for path, previous := range files {
		current, err := os.Stat(path)
		if os.IsNotExist(err) && previous == nil {
			continue
		}
		if err != nil || previous == nil || !os.SameFile(previous, current) ||
			previous.Size() != current.Size() || previous.Mode() != current.Mode() ||
			!previous.ModTime().Equal(current.ModTime()) || changedTime(previous) != changedTime(current) {
			return false
		}
	}
	return true
}

// settled reports whether no input was modified after cutoff, so identical
// metadata later really does mean identical content.
func settled(files map[string]os.FileInfo, cutoff time.Time) bool {
	for _, info := range files {
		if info != nil && (info.ModTime().After(cutoff) || changedAt(info).After(cutoff)) {
			return false
		}
	}
	return true
}
