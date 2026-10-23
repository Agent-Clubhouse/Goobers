package temporalops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workerrouting"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workflow/ir"
)

func testFlags() *flag.FlagSet { return flag.NewFlagSet("test", flag.ContinueOnError) }

func TestContractCommandInspectsExactSourceAndEmitsRefusals(t *testing.T) {
	doc, err := ir.Normalize(workflow.Definition{Name: "sample", Version: 1, DSLVersion: "2.0", Spec: apiv1.WorkflowSpec{
		Gaggle: "g", Start: "task", Tasks: []apiv1.Task{{Name: "task", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "work"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, withSource := range []bool{true, false} {
		if !withSource {
			doc.SourceDefinition = nil
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "ir.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		code := runContract([]string{"--workflow-ir", path}, &out, &stderr, func() (string, error) { return "sha256:binary", nil }, testFlags())
		var report ContractReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("output %q: %v; stderr %s", out.String(), err, stderr.String())
		}
		if withSource && (code != 0 || report.Inspection.State != "compatible") {
			t.Fatalf("code %d report %+v", code, report)
		}
		if !withSource && (code != 1 || report.Inspection.State != "unverifiable") {
			t.Fatalf("code %d report %+v", code, report)
		}
		if report.Inspection.Enforced || report.Manifest.ExecutableDigest != "sha256:binary" {
			t.Fatalf("report %+v", report)
		}
	}
}

func TestContractCommandBoundsAndRejectsUnknownInputBeforeHashing(t *testing.T) {
	for _, raw := range []string{strings.Repeat(" ", maxIRBytes+1), `{"futureField":true}`, `{} {}`} {
		path := filepath.Join(t.TempDir(), "ir.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		called := false
		var out, stderr bytes.Buffer
		code := runContract([]string{"--workflow-ir", path}, &out, &stderr, func() (string, error) { called = true; return "digest", nil }, testFlags())
		if code != 2 || called || out.Len() > 0 {
			t.Fatalf("code=%d hash=%v output=%q error=%q", code, called, out.String(), stderr.String())
		}
	}
}

func TestRolloutCommandPassesConfiguredTargetAndWholeCallDeadline(t *testing.T) {
	root := t.TempDir()
	path := instance.NewLayout(root).ConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("engine:\n  hostPort: temporal.example:7233\n  namespace: staged\n  taskQueue: workflows\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	probe := func(ctx context.Context, cfg *instance.Config) (workerrouting.RolloutSnapshot, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second || cfg.EffectiveEngineConfig().Namespace != "staged" || cfg.EffectiveEngineConfig().HostPort != "temporal.example:7233" {
			t.Fatalf("deadline %v, config %+v", deadline, cfg.EffectiveEngineConfig())
		}
		return workerrouting.RolloutSnapshot{Deployment: "goobers"}, nil
	}
	var out, stderr bytes.Buffer
	if code := runRollout([]string{"--timeout", "1s", root}, &out, &stderr, probe, testFlags()); code != 0 || !called {
		t.Fatalf("code=%d called=%v stderr=%s", code, called, stderr.String())
	}
	for _, timeout := range []string{"0", "-1s", "6m"} {
		called = false
		if code := runRollout([]string{"--timeout", timeout, root}, io.Discard, io.Discard, probe, testFlags()); code != 2 || called {
			t.Fatalf("timeout %s: code %d called %v", timeout, code, called)
		}
	}
	out.Reset()
	code := runRollout([]string{root}, &out, &stderr, func(context.Context, *instance.Config) (workerrouting.RolloutSnapshot, error) {
		return workerrouting.RolloutSnapshot{Deployment: "partial"}, errors.New("unavailable")
	}, testFlags())
	if code != 1 || out.Len() > 0 {
		t.Fatalf("partial evidence escaped: %d %s", code, out.String())
	}
}
