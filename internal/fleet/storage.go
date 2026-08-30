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

	"github.com/goobers/goobers/internal/platform/durability"
)

const (
	associationFileName = "association.json"
	privateKeyFileName  = "private-key.bin"
	credentialFileName  = "credential.bin"
)

type Storage interface {
	Load(instanceRoot string) (Record, error)
	Save(instanceRoot string, record Record) error
	Update(instanceRoot string, update func(*Association) error) error
	Delete(instanceRoot string) error
}

type FileStorage struct {
	baseDir string
	mu      sync.Mutex
}

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

func (s *FileStorage) InstanceDirectory(instanceRoot string) (string, error) {
	canonical, err := CanonicalInstanceRoot(instanceRoot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(s.baseDir, hex.EncodeToString(sum[:])), nil
}

func (s *FileStorage) Load(instanceRoot string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(instanceRoot)
}

func (s *FileStorage) load(instanceRoot string) (Record, error) {
	dir, err := s.InstanceDirectory(instanceRoot)
	if err != nil {
		return Record{}, err
	}
	metadata, err := os.ReadFile(filepath.Join(dir, associationFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, ErrNotAssociated
	}
	if err != nil {
		return Record{}, fmt.Errorf("fleet: read association metadata: %w", err)
	}
	var association Association
	if err := json.Unmarshal(metadata, &association); err != nil {
		return Record{}, fmt.Errorf("fleet: decode association metadata: %w", err)
	}
	if association.SchemaVersion != ProtocolVersion {
		return Record{}, fmt.Errorf("fleet: unsupported association schema version %q", association.SchemaVersion)
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

func (s *FileStorage) Save(instanceRoot string, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(record.Credential) == "" {
		return fmt.Errorf("fleet: credential must not be empty")
	}
	if len(record.PrivateKey) == 0 {
		return fmt.Errorf("fleet: private key must not be empty")
	}
	record.Association.SchemaVersion = ProtocolVersion
	dir, err := s.InstanceDirectory(instanceRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("fleet: create association directory: %w", err)
	}
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
	return nil
}

func (s *FileStorage) Update(instanceRoot string, update func(*Association) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.InstanceDirectory(instanceRoot)
	if err != nil {
		return err
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
}

func (s *FileStorage) Delete(instanceRoot string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.InstanceDirectory(instanceRoot)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, associationFileName)); errors.Is(err, fs.ErrNotExist) {
		return ErrNotAssociated
	} else if err != nil {
		return fmt.Errorf("fleet: inspect association metadata: %w", err)
	}
	for _, name := range []string{associationFileName, privateKeyFileName, credentialFileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("fleet: remove %s: %w", name, err)
		}
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fleet: remove association directory: %w", err)
	}
	return nil
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
