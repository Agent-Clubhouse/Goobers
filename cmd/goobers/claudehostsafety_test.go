package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

type warningHarnessAdapter struct{ harnesstest.FakeAdapter }

func (*warningHarnessAdapter) Preflight(context.Context) (harness.PreflightInfo, error) {
	return harness.PreflightInfo{Version: "older", Warnings: []string{"upgrade for the host-process safety hook"}}, nil
}

func TestCheckHarnessReportsNonBlockingHostSafetyWarning(t *testing.T) {
	withHarnessAdapter(t, func(apiv1.Harness, harness.EnvironmentConfig, map[string][]string, func(context.Context) (string, error)) (harness.Adapter, error) {
		return &warningHarnessAdapter{}, nil
	})
	var out, diagnostic strings.Builder
	if !checkHarnessesAtSources([]apiv1.Goober{{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessClaudeCode}}},
		&out, &diagnostic, nil, harness.EnvironmentConfig{}, nil, nil) {
		t.Fatalf("compatibility warning blocked harness: %s", &out)
	}
	if !strings.Contains(out.String(), "HARNESS claude-code WARNING: upgrade for the host-process safety hook") {
		t.Fatalf("preflight warning was lost: %s", &out)
	}
}
