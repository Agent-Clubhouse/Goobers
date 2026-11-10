package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxTLCInput = 1 << 20

func tlcDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func readTLCFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("checker input must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("checker input exceeds byte limit")
	}
	return data, err
}

func readTLCPinned(path, digest string, limit int64) ([]byte, error) {
	data, err := readTLCFile(path, limit)
	if err != nil {
		return nil, err
	}
	if tlcDigest(data) != digest {
		return nil, fmt.Errorf("checker input digest mismatch: %s", filepath.Base(path))
	}
	return data, nil
}

type tlcRuntimeFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// snapshotTLCRuntime copies a closed runtime tree and hashes the copied bytes,
// not a separately reread source. The directory must be operator-owned.
func snapshotTLCRuntime(root, destination string) ([]tlcRuntimeFile, error) {
	var files []tlcRuntimeFile
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := readTLCFile(path, 256<<20)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > 512<<20 || len(files) >= 4096 {
			return errors.New("java runtime exceeds snapshot budget")
		}
		if err := os.WriteFile(target, data, 0o700); err != nil {
			return err
		}
		files = append(files, tlcRuntimeFile{Path: filepath.ToSlash(relative), Digest: tlcDigest(data)})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func tlcRuntimeDigest(files []tlcRuntimeFile) string {
	data, _ := json.Marshal(files)
	return tlcDigest(data)
}

func tlcWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func canonicalTLCPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("checker directory is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
