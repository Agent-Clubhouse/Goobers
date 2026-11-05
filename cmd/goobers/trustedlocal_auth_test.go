package main

import (
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

func TestTrustedLocalAuthenticationBindsExactParentRef(t *testing.T) {
	grants := []credentials.Grant{{Goober: "parent", Capability: "agent:model", Ref: "selected-parent-key"}, {Goober: "same", Capability: "agent:model", Ref: "selected-parent-key"}, {Goober: "other", Capability: "agent:model", Ref: "another-stored-key"}}
	spec := apiv1.GooberSpec{Harness: apiv1.HarnessClaudeCode}
	binding, err := trustedLocalAuthenticationFor("parent", spec, grants)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ModelRef != "selected-parent-key" || binding.Ambient != nil {
		t.Fatalf("unexpected parent binding: %+v", binding)
	}
	if err := binding.admitChild("same", spec, grants); err != nil {
		t.Fatalf("parent ref refused: %v", err)
	}
	for _, name := range []string{"other", "ambient"} {
		if err := binding.admitChild(name, spec, grants); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
			t.Fatalf("child %q widened credential: %v", name, err)
		}
	}
}

func TestTrustedLocalAmbientAuthenticationKeepsSelectedHarness(t *testing.T) {
	for _, selected := range []apiv1.Harness{apiv1.HarnessClaudeCode, apiv1.HarnessCopilot} {
		t.Run(string(selected), func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("COPILOT_HOME", t.TempDir())
			spec := apiv1.GooberSpec{Harness: selected}
			binding, err := trustedLocalAuthenticationFor("parent", spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := binding.admitChild("child", spec, nil); err != nil {
				t.Fatalf("same ambient login refused: %v", err)
			}
			other := apiv1.HarnessClaudeCode
			if selected == other {
				other = apiv1.HarnessCopilot
			}
			if err := binding.admitChild("child", apiv1.GooberSpec{Harness: other}, nil); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
				t.Fatal("another stored harness login admitted")
			}
			if err := binding.admitChild("child", spec, []credentials.Grant{{Capability: "agent:model", Ref: "stored-explicit-key"}}); !errors.Is(err, credentials.ErrChildAuthenticationIsolation) {
				t.Fatal("ambient parent delegated unrelated explicit key")
			}
		})
	}
}
