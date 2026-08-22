package instance

import (
	"errors"
	"reflect"
	"testing"
)

// TestPlacementRunnersLegacySynthesizedSelf: a zero-declaration instance
// resolves to the implicit self entry — legacy runner.capabilities carried
// verbatim, the host OS substituted (a process fact, not a claim), no
// ceilings, no restrictions.
func TestPlacementRunnersLegacySynthesizedSelf(t *testing.T) {
	cfg := &Config{Runner: RunnerConfig{Capabilities: []string{"go@1.26", "os=linux"}}}
	runners := cfg.PlacementRunners("linux")
	if len(runners) != 1 {
		t.Fatalf("runners = %v, want the one synthesized self entry", runners)
	}
	self := runners[0]
	if !self.Self || self.Name != "self" || self.OS != "linux" {
		t.Fatalf("self entry = %+v, want Self=true Name=self OS=linux", self)
	}
	if !reflect.DeepEqual(self.Capabilities, []string{"go@1.26", "os=linux"}) {
		t.Fatalf("capabilities = %v, want the legacy claims verbatim", self.Capabilities)
	}
	if self.CPU != nil || self.Memory != nil || self.Disk != nil || self.Restrictions != nil {
		t.Fatalf("legacy self must declare no ceilings or restrictions: %+v", self)
	}
}

// TestPlacementRunnersDeclaredInventory: declared entries convert claims
// verbatim — quantities parsed as ceilings, restrictions carried, a declared
// self OS kept over the host substitution, an os-less self entry defaulted.
func TestPlacementRunnersDeclaredInventory(t *testing.T) {
	cfg := &Config{Runners: []RunnerEntry{
		{
			Name: "self",
			Host: "self",
			Provides: RunnerProvides{
				CPU:          "8000m",
				Capabilities: []string{"go@1.26"},
			},
		},
		{
			Name: "win-ci",
			Host: "ghcr.io/example/win:v1",
			Provides: RunnerProvides{
				OS:     RunnerOSWindows,
				Memory: "16Gi",
			},
			Restrictions: []RunnerRestriction{RunnerRestrictionTmpEphemeral},
		},
	}}
	runners := cfg.PlacementRunners("macOS")
	if len(runners) != 2 {
		t.Fatalf("runners = %v, want both declared entries", runners)
	}
	self, win := runners[0], runners[1]
	if !self.Self || self.OS != "macOS" {
		t.Fatalf("os-less self entry must take the host OS: %+v", self)
	}
	if self.CPU == nil || self.CPU.String() != "8" {
		t.Fatalf("cpu ceiling = %v, want parsed 8000m (String \"8\")", self.CPU)
	}
	if win.Self {
		t.Fatal("an image-hosted runner must not be self")
	}
	if win.OS != "windows" || win.Memory == nil || win.Memory.String() != "16Gi" {
		t.Fatalf("win entry = %+v, want declared os and memory ceiling", win)
	}
	if !reflect.DeepEqual(win.Restrictions, []string{"tmp:ephemeral"}) {
		t.Fatalf("restrictions = %v, want the declared effect", win.Restrictions)
	}

	// A self entry that DECLARES provides.os keeps its declaration.
	cfg.Runners[0].Provides.OS = RunnerOSLinux
	if got := cfg.PlacementRunners("macOS")[0].OS; got != "linux" {
		t.Fatalf("declared self OS = %q, want the declaration kept", got)
	}
}

// TestRunnerEngineMissingErrorType: the RNR002 condition is a typed error so
// `goobers validate` can attribute the stable code through LoadConfig's
// wrap chain.
func TestRunnerEngineMissingErrorType(t *testing.T) {
	entry := RunnerEntry{Name: "ci", Host: "ghcr.io/example/ci:v1"}
	err := entry.validate(0, map[string]bool{}, false)
	if err == nil {
		t.Fatal("a non-self host without engine config must be refused")
	}
	var engineMissing *RunnerEngineMissingError
	if !errors.As(err, &engineMissing) {
		t.Fatalf("error type = %T, want *RunnerEngineMissingError", err)
	}
}
