package configmirror

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationValidatesCapturedBytesAndPreservesPreviousOnFailure(t *testing.T) {
	root := t.TempDir()
	config, mirror := filepath.Join(root, "config"), filepath.Join(root, "mirror")
	source := filepath.Join(config, "instructions.md")
	writeTestFile(t, source, "first")
	err := PublishValidated(t.Context(), mirror, config, []byte("instance"), func(staged string) error {
		writeTestFile(t, source, "second")
		data, err := os.ReadFile(filepath.Join(staged, "config/instructions.md"))
		if err != nil || string(data) != "first" {
			t.Fatalf("validation did not pin captured bytes: %q %v", data, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("captured digest does not match applied generation")
	err = PublishValidated(t.Context(), mirror, config, []byte("replacement"), func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("validation error=%v", err)
	}
	snapshot, err := Open(mirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	dest := t.TempDir()
	if err := snapshot.Extract(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "config/instructions.md"))
	if err != nil || string(data) != "first" {
		t.Fatalf("previous snapshot lost: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(mirror, ".worker-config-validation")); !os.IsNotExist(err) {
		t.Fatalf("validation staging leaked: %v", err)
	}
}

func TestValidationReclaimsOnlyOwnedCrashLeftovers(t *testing.T) {
	for _, owned := range []bool{false, true} {
		name := "unowned"
		if owned {
			name = "owned"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			config, mirror := filepath.Join(root, "config"), filepath.Join(root, "mirror")
			writeTestFile(t, filepath.Join(config, "instructions.md"), "current")
			stale := filepath.Join(mirror, ".worker-config-validation")
			writeTestFile(t, filepath.Join(stale, "leftover"), "preserve unless owned")
			if owned {
				writeTestFile(t, filepath.Join(stale, ".owner"), validationOwner)
			}
			called := false
			err := PublishValidated(t.Context(), mirror, config, []byte("instance"), func(staged string) error {
				called = true
				if _, err := os.Stat(filepath.Join(staged, "leftover")); !os.IsNotExist(err) {
					t.Fatalf("old generation not reclaimed: %v", err)
				}
				return nil
			})
			if owned {
				if err != nil || !called {
					t.Fatalf("owned recovery: called=%v error=%v", called, err)
				}
			} else {
				if err == nil || called {
					t.Fatalf("unowned directory accepted: called=%v error=%v", called, err)
				}
				if _, err := os.Stat(filepath.Join(stale, "leftover")); err != nil {
					t.Fatalf("unowned data removed: %v", err)
				}
			}
		})
	}
}

func TestValidationReclaimsReadOnlySnapshot(t *testing.T) {
	config, mirror := t.TempDir(), t.TempDir()
	file := filepath.Join(config, "readonly/instructions.md")
	writeTestFile(t, file, "instructions")
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(file)
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(directory, 0o700)
		_ = os.Chmod(file, 0o600)
	})
	for range 2 {
		err := PublishValidated(t.Context(), mirror, config, []byte("instance"), func(staged string) error {
			_, err := os.ReadFile(filepath.Join(staged, "config/readonly/instructions.md"))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(mirror, ".worker-config-validation")); !os.IsNotExist(err) {
			t.Fatalf("read-only validation tree leaked: %v", err)
		}
	}
}
