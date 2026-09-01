package fleet

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFileStorageLivesOutsideInstanceAndDoesNotExposeSecrets(t *testing.T) {
	base := t.TempDir()
	instanceRoot := filepath.Join(t.TempDir(), "instance")
	if err := os.MkdirAll(instanceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStorage(base)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		Association: Association{
			InstanceID:          "instance-1",
			DisplayName:         "development",
			FleetID:             "fleet-1",
			RegistrationID:      "registration-1",
			CanonicalURI:        "https://fleet.example",
			ConnectionEndpoint:  "wss://fleet.example/api/fleet/v1/connections",
			CredentialExpiresAt: time.Now().Add(time.Hour),
			ProtocolVersion:     ProtocolVersion,
			ACL:                 ACL{PolicyVersion: ProtocolVersion, Grants: []Grant{}},
		},
		PrivateKey: []byte("private-key-material"),
		Credential: "bearer-secret",
	}
	if err := store.Save(instanceRoot, record); err != nil {
		t.Fatal(err)
	}
	dir, err := store.InstanceDirectory(instanceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(strings.ToLower(dir), strings.ToLower(instanceRoot)) {
		t.Fatalf("association directory %q is inside instance root %q", dir, instanceRoot)
	}
	metadata, err := os.ReadFile(filepath.Join(dir, associationFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte(record.Credential)) || bytes.Contains(metadata, record.PrivateKey) {
		t.Fatal("association metadata exposes secret material")
	}
	for _, name := range []string{privateKeyFileName, credentialFileName} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS == "windows" &&
			(bytes.Contains(data, []byte(record.Credential)) || bytes.Contains(data, record.PrivateKey)) {
			t.Fatalf("%s exposes plaintext secret material", name)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %#o, want 0600", name, info.Mode().Perm())
		}
	}
	loaded, err := store.Load(instanceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Credential != record.Credential || !bytes.Equal(loaded.PrivateKey, record.PrivateKey) {
		t.Fatalf("loaded record did not round trip")
	}
}

func TestFileStorageDeleteRemovesAssociation(t *testing.T) {
	store, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := store.Save(root, Record{
		Association: Association{InstanceID: "instance", ProtocolVersion: ProtocolVersion},
		PrivateKey:  []byte("key"),
		Credential:  "credential",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(root); err != nil {
		t.Fatal(err)
	}
	dir, err := store.InstanceDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, disabledFileName)); err != nil {
		t.Fatalf("disabled tombstone after Delete: %v", err)
	}
	if _, err := store.Load(root); !errors.Is(err, ErrNotAssociated) {
		t.Fatalf("Load after Delete error = %v, want ErrNotAssociated", err)
	}
	if _, err := store.LoadAssociation(root); !errors.Is(err, ErrNotAssociated) {
		t.Fatalf("LoadAssociation after Delete error = %v, want ErrNotAssociated", err)
	}
	if err := store.Update(root, func(*Association) error { return nil }); !errors.Is(err, ErrNotAssociated) {
		t.Fatalf("Update after Delete error = %v, want ErrNotAssociated", err)
	}
	for _, name := range []string{associationFileName, privateKeyFileName, credentialFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after Delete: %v", name, err)
		}
	}
	if err := store.Save(root, Record{
		Association: Association{InstanceID: "instance-2", ProtocolVersion: ProtocolVersion},
		PrivateKey:  []byte("new-key"),
		Credential:  "new-credential",
	}); err != nil {
		t.Fatalf("Save after Delete: %v", err)
	}
	loaded, err := store.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Association.InstanceID != "instance-2" {
		t.Fatalf("rejoined association = %+v", loaded.Association)
	}
}

func TestFileStorageRejectsUnknownAssociationSchema(t *testing.T) {
	store, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := store.Save(root, Record{
		Association: Association{InstanceID: "instance", ProtocolVersion: ProtocolVersion},
		PrivateKey:  []byte("key"),
		Credential:  "credential",
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := store.InstanceDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, associationFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"schemaVersion": "1"`), []byte(`"schemaVersion": "999"`), 1)
	if err := atomicWrite(path, data); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAssociation(root); err == nil || !strings.Contains(err.Error(), "unsupported association schema version") {
		t.Fatalf("LoadAssociation error = %v", err)
	}
}
