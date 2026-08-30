package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const (
	associationFileName = "association.json"
	privateKeyFileName  = "private-key.bin"
	credentialFileName  = "credential.bin"
	disabledFileName    = "disabled"
	lockFileName        = ".lock"
)

// Storage persists a Fleet association, private key, and credential for an
// instance, keyed by the instance's root directory.
type Storage interface {
	LoadAssociation(instanceRoot string) (Association, error)
	Load(instanceRoot string) (Record, error)
	Save(instanceRoot string, record Record) error
	Update(instanceRoot string, update func(*Association) error) error
	Delete(instanceRoot string) error
}

// FileStorage is a Storage implementation that persists Fleet association
// state to files outside the instance root, keyed by a canonicalized hash of
// the instance root path.
type FileStorage struct {
	baseDir string
	mu      sync.Mutex
}

// NewFileStorage creates a FileStorage rooted at baseDir. If baseDir is
// empty, a platform-appropriate default user storage directory is used.
func NewFileStorage(baseDir string) (*FileStorage, error) {
	if strings.TrimSpace(baseDir) == "" {
		var err error
		baseDir, err = defaultStorageBaseDir()
		if err != nil {
			return nil, err
		}
	}
	absolute, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("fleet: resolve storage directory: %w", err)
	}
	return &FileStorage{baseDir: filepath.Clean(absolute)}, nil
}

func defaultStorageBaseDir() (string, error) {
	base, err := platformStorageBaseDir()
	if err != nil {
		return "", fmt.Errorf("fleet: resolve user storage directory: %w", err)
	}
	if strings.Contains(strings.ToLower(base), "onedrive") {
		base, err = os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("fleet: resolve non-synced user cache directory: %w", err)
		}
	}
	return filepath.Join(base, "goobers", "fleet", "instances"), nil
}

// CanonicalInstanceRoot resolves instanceRoot to an absolute, symlink-free,
// platform-normalized path suitable for use as a Storage key.
func CanonicalInstanceRoot(instanceRoot string) (string, error) {
	absolute, err := filepath.Abs(instanceRoot)
	if err != nil {
		return "", fmt.Errorf("fleet: resolve instance root: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if resolved, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
		absolute = filepath.Clean(resolved)
	} else if !errors.Is(evalErr, fs.ErrNotExist) {
		return "", fmt.Errorf("fleet: canonicalize instance root: %w", evalErr)
	}
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	return absolute, nil
}

// InstanceDirectory returns the directory under s.baseDir that holds the
// Fleet association state for instanceRoot.
func (s *FileStorage) InstanceDirectory(instanceRoot string) (string, error) {
	canonical, err := CanonicalInstanceRoot(instanceRoot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(s.baseDir, hex.EncodeToString(sum[:])), nil
}

// LoadAssociation reads only the nonsecret Fleet association metadata for
// instanceRoot. It does not access or decrypt the private key or credential.
func (s *FileStorage) LoadAssociation(instanceRoot string) (Association, error) {
	var association Association
	err := s.withDirectoryLock(instanceRoot, false, func(dir string) error {
		var err error
		association, err = s.loadAssociation(dir)
		return err
	})
	return association, err
}

// Load reads the Fleet association, private key, and credential for
// instanceRoot. It returns ErrNotAssociated if no association exists.
func (s *FileStorage) Load(instanceRoot string) (Record, error) {
	var record Record
	err := s.withDirectoryLock(instanceRoot, false, func(dir string) error {
		var err error
		record, err = s.load(dir)
		return err
	})
	return record, err
}

func (s *FileStorage) load(dir string) (Record, error) {
	association, err := s.loadAssociation(dir)
	if err != nil {
		return Record{}, err
	}
	keyCiphertext, err := os.ReadFile(filepath.Join(dir, privateKeyFileName))
	if err != nil {
		return Record{}, fmt.Errorf("fleet: read protected private key: %w", err)
	}
	privateKey, err := unprotect(keyCiphertext)
	if err != nil {
		return Record{}, fmt.Errorf("fleet: unprotect private key: %w", err)
	}
	credentialCiphertext, err := os.ReadFile(filepath.Join(dir, credentialFileName))
	if err != nil {
		return Record{}, fmt.Errorf("fleet: read protected credential: %w", err)
	}
	credential, err := unprotect(credentialCiphertext)
	if err != nil {
		return Record{}, fmt.Errorf("fleet: unprotect credential: %w", err)
	}
	return Record{
		Association: association,
		PrivateKey:  privateKey,
		Credential:  string(credential),
	}, nil
}

func (s *FileStorage) loadAssociation(dir string) (Association, error) {
	if _, err := os.Stat(filepath.Join(dir, disabledFileName)); err == nil {
		return Association{}, ErrNotAssociated
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Association{}, fmt.Errorf("fleet: inspect association disable marker: %w", err)
	}
	metadata, err := os.ReadFile(filepath.Join(dir, associationFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Association{}, ErrNotAssociated
	}
	if err != nil {
		return Association{}, fmt.Errorf("fleet: read association metadata: %w", err)
	}
	var association Association
	if err := json.Unmarshal(metadata, &association); err != nil {
		return Association{}, fmt.Errorf("fleet: decode association metadata: %w", err)
	}
	if association.SchemaVersion != ProtocolVersion {
		return Association{}, fmt.Errorf("fleet: unsupported association schema version %q", association.SchemaVersion)
	}
	return association, nil
}

// Save persists record as the Fleet association, private key, and credential
// for instanceRoot. It refuses to overwrite an active association.
func (s *FileStorage) Save(instanceRoot string, record Record) error {
	if strings.TrimSpace(record.Credential) == "" {
		return fmt.Errorf("fleet: credential must not be empty")
	}
	if len(record.PrivateKey) == 0 {
		return fmt.Errorf("fleet: private key must not be empty")
	}
	record.Association.SchemaVersion = ProtocolVersion
	return s.withDirectoryLock(instanceRoot, true, func(dir string) error {
		disabledPath := filepath.Join(dir, disabledFileName)
		if _, err := os.Stat(filepath.Join(dir, associationFileName)); err == nil {
			if _, disabledErr := os.Stat(disabledPath); errors.Is(disabledErr, fs.ErrNotExist) {
				return fmt.Errorf("fleet: instance is already associated")
			} else if disabledErr != nil {
				return fmt.Errorf("fleet: inspect association disable marker: %w", disabledErr)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("fleet: inspect association metadata: %w", err)
		}
		if err := atomicWrite(disabledPath, nil); err != nil {
			return fmt.Errorf("fleet: disable association during save: %w", err)
		}
		saved := false
		defer func() {
			if saved {
				return
			}
			for _, name := range []string{associationFileName, privateKeyFileName, credentialFileName} {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}()
		protectedKey, err := protect(record.PrivateKey)
		if err != nil {
			return fmt.Errorf("fleet: protect private key: %w", err)
		}
		protectedCredential, err := protect([]byte(record.Credential))
		if err != nil {
			return fmt.Errorf("fleet: protect credential: %w", err)
		}
		metadata, err := json.MarshalIndent(record.Association, "", "  ")
		if err != nil {
			return fmt.Errorf("fleet: encode association metadata: %w", err)
		}
		metadata = append(metadata, '\n')
		for _, file := range []struct {
			name string
			data []byte
		}{
			{privateKeyFileName, protectedKey},
			{credentialFileName, protectedCredential},
			{associationFileName, metadata},
		} {
			if err := atomicWrite(filepath.Join(dir, file.name), file.data); err != nil {
				return err
			}
		}
		if err := os.Remove(disabledPath); err != nil {
			return fmt.Errorf("fleet: activate association: %w", err)
		}
		if err := durability.SyncDir(dir); err != nil {
			return fmt.Errorf("fleet: sync activated association directory: %w", err)
		}
		saved = true
		return nil
	})
}

// Update loads the Association for instanceRoot, applies update to it, and
// persists the result. It returns ErrNotAssociated if no association exists.
func (s *FileStorage) Update(instanceRoot string, update func(*Association) error) error {
	return s.withDirectoryLock(instanceRoot, false, func(dir string) error {
		if _, err := os.Stat(filepath.Join(dir, disabledFileName)); err == nil {
			return ErrNotAssociated
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("fleet: inspect association disable marker: %w", err)
		}
		path := filepath.Join(dir, associationFileName)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotAssociated
		}
		if err != nil {
			return fmt.Errorf("fleet: read association metadata: %w", err)
		}
		var association Association
		if err := json.Unmarshal(data, &association); err != nil {
			return fmt.Errorf("fleet: decode association metadata: %w", err)
		}
		if err := update(&association); err != nil {
			return err
		}
		data, err = json.MarshalIndent(association, "", "  ")
		if err != nil {
			return fmt.Errorf("fleet: encode association metadata: %w", err)
		}
		return atomicWrite(path, append(data, '\n'))
	})
}

// Delete removes the Fleet association, private key, and credential for
// instanceRoot. It returns ErrNotAssociated if no association exists.
func (s *FileStorage) Delete(instanceRoot string) error {
	return s.withDirectoryLock(instanceRoot, false, func(dir string) error {
		if _, err := os.Stat(filepath.Join(dir, associationFileName)); errors.Is(err, fs.ErrNotExist) {
			return ErrNotAssociated
		} else if err != nil {
			return fmt.Errorf("fleet: inspect association metadata: %w", err)
		}
		if err := atomicWrite(filepath.Join(dir, disabledFileName), nil); err != nil {
			return fmt.Errorf("fleet: disable association: %w", err)
		}
		for _, name := range []string{associationFileName, privateKeyFileName, credentialFileName} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("fleet: remove %s: %w", name, err)
			}
		}
		if err := durability.SyncDir(dir); err != nil {
			return fmt.Errorf("fleet: sync removed association: %w", err)
		}
		return nil
	})
}

func (s *FileStorage) withDirectoryLock(instanceRoot string, create bool, operation func(string) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.InstanceDirectory(instanceRoot)
	if err != nil {
		return err
	}
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("fleet: create association directory: %w", err)
		}
	} else if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrNotAssociated
	} else if err != nil {
		return fmt.Errorf("fleet: inspect association directory: %w", err)
	}
	lockPath := filepath.Join(dir, lockFileName)
	deadline := time.Now().Add(2 * time.Second)
	var handle *platformlock.Handle
	for {
		handle, err = platformlock.TryAcquire(lockPath)
		if err == nil {
			break
		}
		if !errors.Is(err, platformlock.ErrHeld) || time.Now().After(deadline) {
			return fmt.Errorf("fleet: acquire association lock: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	operationErr := operation(dir)
	if releaseErr := handle.Release(); releaseErr != nil {
		return errors.Join(operationErr, fmt.Errorf("fleet: release association lock: %w", releaseErr))
	}
	return operationErr
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("fleet: create temporary %s: %w", filepath.Base(path), err)
	}
	tempPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tempPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fleet: protect temporary %s: %w", filepath.Base(path), err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fleet: write temporary %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fleet: sync temporary %s: %w", filepath.Base(path), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fleet: close temporary %s: %w", filepath.Base(path), err)
	}
	if err := durability.ReplaceFile(tempPath, path); err != nil {
		return fmt.Errorf("fleet: publish %s: %w", filepath.Base(path), err)
	}
	remove = false
	if err := durability.SyncDir(dir); err != nil {
		return fmt.Errorf("fleet: sync association directory: %w", err)
	}
	return nil
}
