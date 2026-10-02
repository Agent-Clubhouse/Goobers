package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
)

const configTransactionName = ".config-transaction"

// Snapshots are immutable until retirement. Recovery can therefore restart even
// after a second crash halfway through restoring a file or directory.
type configIntent struct {
	Schema       int  `json:"schema"`
	WithInstance bool `json:"withInstance"`
	Committed    bool `json:"committed"`
}

type configTransaction struct {
	layout Layout
	dir    string
	intent configIntent
	// boundary is supplied only by fault-injection tests, after durable operations.
	boundary func(string)
}

func (t *configTransaction) checkpoint(name string) {
	if t.boundary != nil {
		t.boundary(name)
	}
}

func lockConfigTransaction(layout Layout) (*lock.Handle, error) {
	if err := RequireCurrentRoot(layout.Root); err != nil {
		return nil, err
	}
	path := filepath.Join(layout.Root, configTransactionName+".lock")
	if err := checkConfigTree(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return lock.TryAcquire(path)
}

// RecoverConfigTransaction restores an interrupted replacement before callers
// read instance.yaml. A pending decision rolls back; a durable commit rolls forward.
func RecoverConfigTransaction(layout Layout) error {
	// Do not create locks or mutate roots when there is nothing to recover.
	if _, err := os.Lstat(filepath.Join(layout.Root, configTransactionName)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	held, err := lockConfigTransaction(layout)
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()
	return recoverConfigTransaction(layout, nil)
}

func recoverConfigTransaction(layout Layout, boundary func(string)) error {
	t := &configTransaction{layout: layout, dir: filepath.Join(layout.Root, configTransactionName), boundary: boundary}
	if err := checkConfigTree(t.dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "old", "new", "install", "intent", "intent.tmp":
		default:
			return fmt.Errorf("unexpected config transaction entry %q", entry.Name())
		}
	}
	file, err := os.Open(filepath.Join(t.dir, "intent"))
	if errors.Is(err, os.ErrNotExist) {
		return t.retire()
	} // No live writes precede intent publication.
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 257))
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if len(data) > 256 {
		return fmt.Errorf("oversized config transaction intent")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&t.intent); err != nil {
		return fmt.Errorf("invalid config transaction intent: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || t.intent.Schema != 1 {
		return fmt.Errorf("invalid config transaction intent schema")
	}
	canonical, err := json.Marshal(t.intent)
	if err != nil || !bytes.Equal(data, canonical) {
		return fmt.Errorf("noncanonical config transaction intent")
	}
	generation := "old"
	if t.intent.Committed {
		generation = "new"
	}
	// Validate the entire selected generation before touching any live path.
	for _, name := range t.names() {
		path := filepath.Join(t.dir, generation, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if (name == ConfigDirName && !info.IsDir()) || (name == ConfigFileName && !info.Mode().IsRegular()) {
			return fmt.Errorf("invalid config transaction snapshot %s", name)
		}
	}
	release, err := gaggletemplate.LockConfig(layout.ConfigDir(), filepath.Join(t.dir, "old", ConfigDirName), filepath.Join(t.dir, "new", ConfigDirName))
	if err != nil {
		return err
	}
	defer func() { _ = release() }()
	if err := t.install(generation); err != nil {
		return err
	}
	return t.retire()
}

func (t *configTransaction) names() []string {
	if t.intent.WithInstance {
		return []string{ConfigFileName, ConfigDirName}
	}
	return []string{ConfigDirName}
}

func prepareConfigTransaction(layout Layout, stagedConfig, stagedInstance string, boundary func(string)) (*PreparedConfigSwap, error) {
	held, err := lockConfigTransaction(layout)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = held.Release()
		}
	}()
	if err := recoverConfigTransaction(layout, nil); err != nil {
		return nil, err
	}
	release, err := gaggletemplate.LockConfig(layout.ConfigDir(), stagedConfig)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !transferred {
			_ = release()
		}
	}()
	if err := gaggletemplate.GuardReplacement(layout.ConfigDir(), stagedConfig); err != nil {
		return nil, err
	}
	t := &configTransaction{layout: layout, dir: filepath.Join(layout.Root, configTransactionName), intent: configIntent{Schema: 1, WithInstance: stagedInstance != ""}, boundary: boundary}
	if err := os.Mkdir(t.dir, 0700); err != nil {
		return nil, err
	}
	for _, generation := range []string{"old", "new"} {
		dir := filepath.Join(t.dir, generation)
		if err := os.Mkdir(dir, 0700); err != nil {
			return nil, err
		}
		for _, name := range t.names() {
			source := filepath.Join(layout.Root, name)
			if generation == "new" {
				source = stagedConfig
				if name == ConfigFileName {
					source = stagedInstance
				}
			}
			if err := copyConfigSnapshot(source, filepath.Join(dir, name)); err != nil {
				return nil, err
			}
		}
		if err := durability.SyncDir(dir); err != nil {
			return nil, err
		}
	}
	// Persist the snapshot links before publishing intent: a crash during the
	// intent rename must never expose a decision whose backups are not durable.
	if err := errors.Join(durability.SyncDir(t.dir), durability.SyncDir(layout.Root)); err != nil {
		return nil, err
	}
	t.checkpoint("snapshots")
	if err := t.writeIntent(); err != nil {
		return nil, err
	}
	t.checkpoint("prepared")
	if err := t.install("new"); err != nil {
		return nil, errors.Join(err, t.install("old"))
	}
	transferred = true
	return &PreparedConfigSwap{transaction: t, release: func() error { return errors.Join(release(), held.Release()) }}, nil
}

func (t *configTransaction) writeIntent() error {
	data, err := json.Marshal(t.intent)
	if err != nil {
		return err
	}
	path := filepath.Join(t.dir, "intent.tmp")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	if err := syncConfigFile(path); err != nil {
		return err
	}
	if err := durability.ReplaceFile(path, filepath.Join(t.dir, "intent")); err != nil {
		return err
	}
	return durability.SyncDir(t.dir)
}

func (t *configTransaction) install(generation string) error {
	for _, name := range t.names() {
		live := filepath.Join(t.layout.Root, name)
		if err := checkConfigTree(live); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		staged := filepath.Join(t.dir, "install")
		if err := os.RemoveAll(staged); err != nil {
			return err
		}
		if err := copyConfigSnapshot(filepath.Join(t.dir, generation, name), staged); err != nil {
			return err
		}
		if err := durability.SyncDir(t.dir); err != nil {
			return err
		}
		t.checkpoint(generation + "-staged-" + name)
		if err := os.RemoveAll(live); err != nil {
			return err
		}
		if err := durability.SyncDir(t.layout.Root); err != nil {
			return err
		}
		t.checkpoint(generation + "-removed-" + name)
		if err := durability.Move(staged, live); err != nil {
			return err
		}
		if err := errors.Join(durability.SyncDir(t.dir), durability.SyncDir(t.layout.Root)); err != nil {
			return err
		}
		t.checkpoint(generation + "-installed-" + name)
	}
	return nil
}

func (t *configTransaction) retire() error {
	garbage := t.dir + ".garbage"
	if err := checkConfigTree(garbage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.RemoveAll(garbage); err != nil {
		return err
	}
	if err := durability.Move(t.dir, garbage); err != nil {
		return err
	}
	if err := durability.SyncDir(t.layout.Root); err != nil {
		return err
	}
	t.checkpoint("retired")
	if err := os.RemoveAll(garbage); err != nil {
		return err
	}
	if err := durability.SyncDir(t.layout.Root); err != nil {
		return err
	}
	t.checkpoint("cleaned")
	return nil
}

// Reject symlinks and special files, including at the root of a snapshot.
func checkConfigTree(path string) error {
	return filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unsafe config transaction path %s", path)
		}
		return nil
	})
}

func copyConfigSnapshot(source, destination string) error {
	if err := checkConfigTree(source); err != nil {
		return err
	}
	var directories []string
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, target)
			return os.Mkdir(target, info.Mode().Perm()|0700)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = input.Close() }()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, output.Sync(), output.Close())
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := durability.SyncDir(directories[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncConfigFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}
