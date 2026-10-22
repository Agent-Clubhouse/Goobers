package configmirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

const (
	planePointerName = "current"
	planeRetained    = 4
)

// Plane stores sanitized worker snapshots by content digest and publishes one
// current pointer. Current plus three superseded snapshots remain fetchable.
type Plane struct {
	Root    string
	current atomic.Pointer[string]
}

// NewPlane opens a content-addressed worker configuration plane.
func NewPlane(root string) (*Plane, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("config mirror plane requires an absolute root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	p := &Plane{Root: root}
	data, err := os.ReadFile(filepath.Join(root, planePointerName))
	if err == nil {
		digest := strings.TrimSpace(string(data))
		file, err := p.Open(digest)
		if err != nil {
			return nil, fmt.Errorf("open current config snapshot: %w", err)
		}
		_ = file.Close()
		p.current.Store(&digest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return p, nil
}

// Current returns the current content digest, or empty before first publish.
func (p *Plane) Current() string {
	if p == nil {
		return ""
	}
	if digest := p.current.Load(); digest != nil {
		return *digest
	}
	return ""
}

// PublishValidated captures and validates one snapshot before atomically
// advancing Current. Publication is idempotent for identical content.
func (p *Plane) PublishValidated(ctx context.Context, configDir string, instanceDocument []byte, validate func(string) error) (string, error) {
	held, err := lock.TryAcquire(filepath.Join(p.Root, "publish.lock"))
	if err != nil {
		return "", err
	}
	defer func() { _ = held.Release() }()

	staging, err := os.MkdirTemp(p.Root, ".capture-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := PublishValidated(ctx, staging, configDir, instanceDocument, validate); err != nil {
		return "", err
	}
	source := filepath.Join(staging, SnapshotName)
	digest, err := snapshotFileDigest(source)
	if err != nil {
		return "", err
	}
	destination, err := p.path(digest)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(destination); errors.Is(err, fs.ErrNotExist) {
		if err := durability.Move(source, destination); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if existing, err := snapshotFileDigest(destination); err != nil || existing != digest {
		return "", errors.New("config mirror plane contains corrupt digest object")
	}
	if err := durability.WriteFileAtomic(filepath.Join(p.Root, planePointerName), []byte(digest+"\n"), 0o600); err != nil {
		return "", err
	}
	p.current.Store(&digest)
	if err := p.prune(digest); err != nil {
		return "", err
	}
	return digest, nil
}

// Open opens one exact snapshot after validating its digest spelling and file.
func (p *Plane) Open(digest string) (io.ReadCloser, error) {
	path, err := p.path(digest)
	if err != nil {
		return nil, err
	}
	file, err := safeopen.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxSnapshotBytes+(64<<20) {
		_ = file.Close()
		return nil, errors.New("invalid config snapshot file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, MaxSnapshotBytes+(64<<20)+1)); err != nil {
		_ = file.Close()
		return nil, err
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		_ = file.Close()
		return nil, errors.New("config snapshot digest verification failed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// ExtractFile validates and extracts a downloaded snapshot.
func ExtractFile(ctx context.Context, archivePath, destination string) error {
	snapshot, err := openSnapshot(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = snapshot.Close() }()
	return snapshot.Extract(ctx, destination)
}

func (p *Plane) path(digest string) (string, error) {
	name, err := planeDigestName(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(p.Root, name+".zip"), nil
}

func planeDigestName(digest string) (string, error) {
	name := strings.TrimPrefix(digest, "sha256:")
	decoded, err := hex.DecodeString(name)
	if err != nil || len(decoded) != sha256.Size || digest != "sha256:"+name || strings.ToLower(name) != name {
		return "", errors.New("invalid config snapshot digest")
	}
	return name, nil
}

func snapshotFileDigest(path string) (string, error) {
	file, err := safeopen.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, MaxSnapshotBytes+(64<<20)+1))
	if err != nil {
		return "", err
	}
	if n > MaxSnapshotBytes+(64<<20) {
		return "", errors.New("config snapshot exceeds stored size limit")
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func (p *Plane) prune(current string) error {
	entries, err := os.ReadDir(p.Root)
	if err != nil {
		return err
	}
	type candidate struct {
		name    string
		modTime int64
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{name: entry.Name(), modTime: info.ModTime().UnixNano()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modTime != candidates[j].modTime {
			return candidates[i].modTime > candidates[j].modTime
		}
		return candidates[i].name > candidates[j].name
	})
	currentName, _ := planeDigestName(current)
	keptOthers := 0
	for _, candidate := range candidates {
		if candidate.name == currentName+".zip" {
			continue
		}
		if keptOthers < planeRetained-1 {
			keptOthers++
			continue
		}
		if err := os.Remove(filepath.Join(p.Root, candidate.name)); err != nil {
			return err
		}
	}
	return durability.SyncDir(p.Root)
}
