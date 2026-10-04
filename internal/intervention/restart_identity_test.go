package intervention

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestRestartCanonicalIdentityRecoversExactLegacyAcceptance(t *testing.T) {
	runs := t.TempDir()
	source, key := "original-run", "human-scoped-key"
	fresh, err := restartEpochForCommand(runs, source, key)
	if err != nil || len(fresh) != 32 {
		t.Fatal(fresh, err)
	}
	if _, err = hex.DecodeString(fresh); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(source + "\x00" + key))
	legacy := "human-restart-" + hex.EncodeToString(sum[:])
	run, err := journal.Create(runs, journal.RunIdentity{RunID: legacy, Workflow: "pinned-workflow", Gaggle: "own"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	selected, err := restartEpochForCommand(runs, source, key)
	if err != nil || selected != legacy {
		t.Fatal("legacy receipt created a second identity", selected, err)
	}
	if _, err = os.Stat(filepath.Join(runs, fresh)); !os.IsNotExist(err) {
		t.Fatal("canonical duplicate published", err)
	}
	other, err := restartEpochForCommand(runs, source, "different-human-key")
	if err != nil || other == fresh || other == legacy {
		t.Fatal(other, err)
	}
	if err = os.Mkdir(filepath.Join(runs, fresh), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = restartEpochForCommand(runs, source, key); err == nil {
		t.Fatal("ambiguous dual custody accepted")
	}
}
func TestRestartIdentityCannotEvadeDamagedLegacyCustody(t *testing.T) {
	runs := t.TempDir()
	source, key := "source", "key"
	sum := sha256.Sum256([]byte(source + "\x00" + key))
	legacy := "human-restart-" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(runs, legacy), []byte("invalid journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restartEpochForCommand(runs, source, key); err == nil {
		t.Fatal("invalid custody became new admission")
	}
}
