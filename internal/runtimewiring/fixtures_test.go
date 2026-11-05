package runtimewiring_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

func initDemo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestMain(m *testing.M) {
	runtimewiring.CopilotModelLister = testCopilotModelLister{}
	os.Exit(m.Run())
}

type testCopilotModelLister struct{}

func (testCopilotModelLister) ListModels(context.Context, []string, []string) ([]harness.CopilotModelInfo, error) {
	return []harness.CopilotModelInfo{
		{ID: "claude-fable-5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-sonnet-5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-sonnet-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-sonnet-4.5"},
		{ID: "claude-haiku-4.5"},
		{ID: "claude-opus-4.8-fast", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.8", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.7", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "claude-opus-4.6", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "max"}},
		{ID: "claude-opus-4.5"},
		{ID: "gpt-5.6-sol", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.6-terra", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.6-luna", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-5.5", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.4", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.3-codex", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.4-mini", SupportedReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh"}},
		{ID: "gpt-5-mini", SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}},
		{ID: "gemini-3.1-pro-preview", SupportedReasoningEfforts: []string{"none", "low", "medium", "high"}},
		{ID: "gemini-3.5-flash", SupportedReasoningEfforts: []string{"none", "minimal", "low", "medium", "high"}},
		{ID: "kimi-k2.7-code"},
		{ID: "mai-code-1-flash-picker"},
	}, nil
}
