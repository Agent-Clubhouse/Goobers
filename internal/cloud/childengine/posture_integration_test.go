//go:build cloudintegration

package childengine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/engine"
)

// The manifest binds permission once. A retry with a different bit cannot
// change it, and Load does not consult an applied generation to recreate it.
func TestChildAdmissionFreezesParentPosture(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			f := newFamily(t)
			in := acceptChild(t, f, "parent-posture", "call-1")
			in.ChildWorkflowsEnabled = enabled
			service := f.service(time.Hour)
			binding, err := service.Admit(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			in.ChildWorkflowsEnabled = !enabled
			again, err := service.Admit(t.Context(), in)
			if err != nil || again.ManifestDigest != binding.ManifestDigest {
				t.Fatalf("retry changed manifest: %+v %v", again, err)
			}
			data, err := service.Load(t.Context(), again)
			if err != nil {
				t.Fatal(err)
			}
			var manifest engine.FrozenChildManifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Input.ChildWorkflowsEnabled != enabled {
				t.Fatal("manifest did not retain the parent's original permission")
			}
		})
	}
}
