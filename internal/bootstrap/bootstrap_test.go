package bootstrap

import "testing"

const fixtureRoot = "../../test/fixtures/e2e/walking-skeleton"

func TestLoadAndRegisterFixture(t *testing.T) {
	loaded, err := LoadAndRegister(fixtureRoot, "")
	if err != nil {
		t.Fatalf("LoadAndRegister: %v", err)
	}
	if loaded.Manifest == nil {
		t.Fatal("expected a manifest")
	}
	if !loaded.Registry.PreviewFeaturesEnabled() {
		t.Fatal("expected manifest preview-feature acknowledgement to reach the registry")
	}
	if len(loaded.Gaggles) == 0 || len(loaded.Workflows) == 0 {
		t.Fatalf("expected gaggles + workflows, got %d gaggles, %d workflows", len(loaded.Gaggles), len(loaded.Workflows))
	}
	// Every workflow is registered and resolvable.
	for _, w := range loaded.Workflows {
		def, ok := loaded.Registry.Latest(w.Name)
		if !ok {
			t.Errorf("workflow %q was not registered", w.Name)
			continue
		}
		if _, err := loaded.Registry.Compile(def); err != nil {
			t.Errorf("registered workflow %q does not compile: %v", w.Name, err)
		}
	}
}

func TestLoadAndRegisterBadDirErrors(t *testing.T) {
	if _, err := LoadAndRegister("does-not-exist", ""); err == nil {
		t.Error("expected an error for a missing config dir")
	}
}
