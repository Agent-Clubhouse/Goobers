package gitexclude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
)

// AddScoped installs an independently owned exclusion block. Distinct IDs may
// contain identical patterns; removing one never removes another's exclusions.
func AddScoped(ctx context.Context, dir, id string, patterns ...Pattern) error {
	return editScoped(ctx, dir, id, patterns)
}

// RemoveScoped removes only the named block, preserving all original bytes and
// other invocations' blocks. It is idempotent, including after process death.
func RemoveScoped(ctx context.Context, dir, id string) error {
	return editScoped(ctx, dir, id, nil)
}

func editScoped(ctx context.Context, dir, id string, patterns []Pattern) error {
	if id == "" || strings.ContainsAny(id, "\r\n") {
		return fmt.Errorf("gitexclude: invalid scope ID")
	}
	path, err := excludeFilePath(ctx, dir)
	if err != nil {
		return err
	}
	held, err := acquireExclude(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	begin, end := "\n# goobers scoped "+id+" begin\n", "# goobers scoped "+id+" end\n"
	content := string(data)
	if start := strings.Index(content, begin); start >= 0 {
		finish := strings.Index(content[start+len(begin):], end)
		if finish < 0 {
			return fmt.Errorf("gitexclude: incomplete scope %q", id)
		}
		prefix, suffix := content[:start], content[start+len(begin)+finish+len(end):]
		// An append made while the block existed may rely on its final newline.
		// Retain a separator only when neither surviving side supplies one.
		if prefix != "" && suffix != "" && !strings.HasSuffix(prefix, "\n") && !strings.HasPrefix(suffix, "\n") {
			prefix += "\n"
		}
		content = prefix + suffix
	}
	if len(patterns) > 0 {
		content += begin
		for _, p := range patterns {
			if strings.ContainsAny(p.Line, "\r\n") {
				return fmt.Errorf("gitexclude: invalid pattern")
			}
			content += p.Line + "\n"
		}
		content += end
	}
	if content == string(data) {
		return nil
	}
	return replaceExclude(path, []byte(content))
}

func acquireExclude(ctx context.Context, path string) (*lock.Handle, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	for {
		held, err := lock.TryAcquire(path + ".goobers-lock")
		if !errors.Is(err, lock.ErrHeld) {
			return held, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type excludeTempFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

var (
	createExcludeTemp  = func(dir, pattern string) (excludeTempFile, error) { return os.CreateTemp(dir, pattern) }
	replaceExcludeFile = durability.ReplaceFile
)

func replaceExclude(path string, data []byte) error {
	file, err := createExcludeTemp(filepath.Dir(path), "exclude-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return replaceExcludeFile(file.Name(), path)
}
