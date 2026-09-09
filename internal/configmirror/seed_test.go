package configmirror

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedPublishesOnlyValidatedInstanceAndRetryPreservesGeneration(t *testing.T) {
	config, mirror := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(config, "instructions.md"), "first")
	if err := Publish(t.Context(), mirror, config, []byte("instance")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "worker")
	want := errors.New("invalid captured config")
	if err := Seed(t.Context(), mirror, destination, func(string) error { return want }); !errors.Is(err, want) {
		t.Fatalf("validation failure=%v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed seed exposed destination: %v", err)
	}
	validate := func(root string) error {
		data, err := os.ReadFile(filepath.Join(root, "config/instructions.md"))
		if err != nil {
			return err
		}
		if string(data) != "first" {
			return errors.New("wrong generation")
		}
		return nil
	}
	if err := Seed(t.Context(), mirror, destination, validate); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(config, "instructions.md"), "second")
	if err := Publish(t.Context(), mirror, config, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := Seed(t.Context(), mirror, destination, validate); err != nil {
		t.Fatalf("init retry replaced completed seed: %v", err)
	}
	if _, err := os.Stat(destination + ".seed.pending"); !os.IsNotExist(err) {
		t.Fatalf("seed staging leaked: %v", err)
	}
}

func TestSeedRefusesUnownedDestination(t *testing.T) {
	destination := t.TempDir()
	writeTestFile(t, filepath.Join(destination, "operator-data"), "preserve")
	if err := Seed(t.Context(), t.TempDir(), destination, func(string) error {
		t.Fatal("validated unowned destination")
		return nil
	}); err == nil {
		t.Fatal("accepted unowned destination")
	}
	data, err := os.ReadFile(filepath.Join(destination, "operator-data"))
	if err != nil || string(data) != "preserve" {
		t.Fatalf("changed unrelated data: %q %v", data, err)
	}
}
