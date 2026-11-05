package main

import (
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

func TestGaggleFingerprintTracksCapturedSkillBytes(t *testing.T) {
	packages := map[string][]workflow.SkillFile{"review": {{Path: "SKILL.md", Content: "original"}}}
	digest := func() string {
		t.Helper()
		value, err := gaggleConfigFingerprint(&instance.Config{}, &instance.ConfigSet{}, "team", nil, nil, packages)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := digest()
	packages["review"][0].Content = "changed"
	if after := digest(); before == after {
		t.Fatal("skill-only edit reused stale executor seams")
	}
}
