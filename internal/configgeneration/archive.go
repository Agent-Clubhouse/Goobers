// Package configgeneration captures immutable config-as-code execution inputs.
// Mutable instance state and credential sources are deliberately excluded.
package configgeneration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/internal/configtree"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// Limits bound capture before allocation as well as validation of downloaded
// generations. An overlarge generation is refused before admission.
const (
	MaxFiles        = 4096
	MaxFileBytes    = 8 << 20
	MaxContentBytes = 32 << 20
	MaxArchiveBytes = 48 << 20
)

type file struct {
	Directory bool   `json:"directory,omitempty"`
	Path      string `json:"path"`
	Mode      uint32 `json:"mode"`
	Data      []byte `json:"data"`
}

// Archive is one complete immutable generation of configuration definitions,
// instructions, skills, and assets. Credentials and instance.yaml are absent.
type Archive struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instanceID,omitempty"`
	Files      []file `json:"files"`
}

// CaptureForInstance namespaces archive ownership by the admitting instance.
// Identical definitions from separate instances must not share a deletable pin.
func CaptureForInstance(ctx context.Context, configDir, instanceID string) ([]byte, string, error) {
	absolute, err := filepath.Abs(configDir)
	if err != nil {
		return nil, "", err
	}
	a := Archive{Version: 1, InstanceID: instanceID}
	total := int64(0)
	err = configtree.WalkDefinitionTrees(absolute, func(tree string) error {
		root, err := os.OpenRoot(tree)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		prefix := "config"
		if tree != absolute {
			prefix = "goobers"
		}
		return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Name() == ".git" {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if len(a.Files) >= MaxFiles {
				return errors.New("config generation exceeds file count limit")
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if entry.IsDir() {
				a.Files = append(a.Files, file{Path: path.Join(prefix, filepath.ToSlash(name)), Mode: uint32(info.Mode().Perm()), Directory: true})
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("config generation refuses non-regular file %q", name)
			}
			if info.Size() > MaxFileBytes || total+info.Size() > MaxContentBytes {
				return errors.New("config generation exceeds content size limit")
			}
			f, err := safeopen.OpenRegularInRoot(root, name)
			if err != nil {
				return err
			}
			opened, statErr := f.Stat()
			if statErr != nil || !opened.Mode().IsRegular() {
				_ = f.Close()
				return errors.New("config generation source changed during capture")
			}
			data, readErr := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
			closeErr := f.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			total += int64(len(data))
			if len(data) > MaxFileBytes || total > MaxContentBytes {
				return errors.New("config generation exceeds content size limit")
			}
			a.Files = append(a.Files, file{Path: path.Join(prefix, filepath.ToSlash(name)), Mode: uint32(opened.Mode().Perm()), Data: data})
			return nil
		})
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(a.Files, func(i, j int) bool { return a.Files[i].Path < a.Files[j].Path })
	data, err := json.Marshal(a)
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxArchiveBytes {
		return nil, "", errors.New("config generation exceeds archive size limit")
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Decode validates a content-addressed archive without touching disk.
func Decode(data []byte, digest string) (*Archive, error) {
	if len(data) > MaxArchiveBytes {
		return nil, errors.New("config generation archive exceeds size limit")
	}
	sum := sha256.Sum256(data)
	if digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, errors.New("config generation digest mismatch")
	}
	var a Archive
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("config generation contains trailing data")
	}
	if a.Version != 1 || len(a.Files) > MaxFiles {
		return nil, errors.New("unsupported config generation or file count")
	}
	seen := make(map[string]bool, len(a.Files))
	spelling := make(map[string]string, len(a.Files))
	total := 0
	for _, f := range a.Files {
		if !validPath(f.Path) || seen[strings.ToLower(f.Path)] {
			return nil, errors.New("config generation has unsafe or duplicate path")
		}
		seen[strings.ToLower(f.Path)] = true
		for part := f.Path; part != "."; part = path.Dir(part) {
			key := strings.ToLower(part)
			if previous, ok := spelling[key]; ok && previous != part {
				return nil, errors.New("config generation has case-colliding paths")
			}
			spelling[key] = part
		}
		if f.Directory && len(f.Data) != 0 {
			return nil, errors.New("config generation directory contains data")
		}
		if f.Mode & ^uint32(0777) != 0 {
			return nil, errors.New("config generation has unsafe file mode")
		}
		total += len(f.Data)
		if len(f.Data) > MaxFileBytes || total > MaxContentBytes {
			return nil, errors.New("config generation exceeds content size limit")
		}
	}
	return &a, nil
}

func validPath(name string) bool {
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\:*?<>|\"") || len(name) >= 4096 {
		return false
	}
	if name != "config" && name != "goobers" && !strings.HasPrefix(name, "config/") && !strings.HasPrefix(name, "goobers/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.TrimRight(part, ". ") != part || strings.IndexFunc(part, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		switch base {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return false
		}
	}
	return true
}

// Extract creates a private execution tree. The caller owns its lifetime and
// must never publish the directory until extraction and admission succeed.
func (a *Archive) Extract(ctx context.Context, destination string) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll("config", 0700); err != nil {
		return err
	}
	for _, f := range a.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.Directory {
			if err := root.MkdirAll(f.Path, 0700); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(path.Dir(f.Path), 0700); err != nil {
			return err
		}
		out, err := root.OpenFile(f.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := out.Write(f.Data)
		if writeErr == nil {
			writeErr = out.Sync()
		}
		closeErr := out.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := root.Chmod(f.Path, fs.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	for i := len(a.Files) - 1; i >= 0; i-- {
		f := a.Files[i]
		if f.Directory {
			if err := root.Chmod(f.Path, fs.FileMode(f.Mode)); err != nil {
				return err
			}
		}
	}
	return nil
}
