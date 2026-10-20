package main

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

func TestTrustedLocalActivationRequiresExplicitPreviewAndQualifiedPlatform(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, enabled := range []string{"", "false", "true"} {
			annotations := map[string]string{workflow.PreviewFeaturesAnnotation: enabled}
			err := trustedLocalParentActivation(annotations, platform)
			want := enabled == "true" && platform != "windows"
			if (err == nil) != want {
				t.Fatalf("platform=%s preview=%q err=%v", platform, enabled, err)
			}
			if err != nil && !errors.Is(err, workflow.ErrChildWorkflowExecutionUnsupported) {
				t.Fatalf("untyped activation refusal: %v", err)
			}
		}
	}
}

func TestTrustedLocalAvailabilityPreservesOrdinaryPlacementPolicy(t *testing.T) {
	for _, declared := range []bool{false, true} {
		rc := withSelfExecutionPolicy(runner.Config{RunnersDeclared: declared}, &instance.Config{}, nil)
		if rc.RunnersDeclared != declared {
			t.Fatal("local child availability changed ordinary workflow placement journals")
		}
	}
}
