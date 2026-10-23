package cutover

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/config/readiness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
)

type fakeInstall struct {
	schema, bucket error
	lease, paused  bool
}

func (f fakeInstall) SchemaCompatible(context.Context) error         { return f.schema }
func (f fakeInstall) LeaseRowPresent(context.Context) (bool, error)  { return f.lease, nil }
func (f fakeInstall) AdmissionsPaused(context.Context) (bool, error) { return f.paused, nil }
func (f fakeInstall) BucketReachable(context.Context) error          { return f.bucket }

func config() *instance.Config {
	return &instance.Config{
		Placement: &instance.PlacementConfig{SelfExecution: "deny"},
		Runners: []instance.RunnerEntry{{
			Name: "worker", Host: "ghcr.io/example/worker:v1",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Shell: true},
		}},
	}
}

func set(dsl string) *instance.ConfigSet {
	return &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "w"}, DSLVersion: dsl,
			Spec: apiv1.WorkflowSpec{Gaggle: "g", Start: "t", Tasks: []apiv1.Task{{
				Name: "t", Type: apiv1.TaskDeterministic, Goal: "run",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}},
			}}},
		}},
	}
}

func TestCutoverCheckBlocksOnIneligibleWorkflow(t *testing.T) {
	ready := fakeInstall{lease: true, paused: true}
	report := Run(context.Background(), readiness.Assess, config(), set(supportmatrix.V2DSLVersion), ready)
	if report.Ready || report.Config.Eligible {
		t.Fatalf("DSL 2.0 workflow passed cutover check: %+v", report)
	}
	var text bytes.Buffer
	if err := report.WriteText(&text); err != nil || !strings.Contains(text.String(), "RUNTIME003") || !strings.Contains(text.String(), "cutover check: blocked") {
		t.Fatalf("text = %q (%v)", text.String(), err)
	}
	if ok := Run(context.Background(), readiness.Assess, config(), set(supportmatrix.V3DSLVersion), ready); !ok.Ready {
		t.Fatalf("eligible config with a ready install blocked: %+v", ok)
	}
}

func TestCutoverCheckInstallFailuresBlock(t *testing.T) {
	for name, install := range map[string]fakeInstall{
		"schema":  {schema: errors.New("incompatible"), lease: true, paused: true},
		"lease":   {paused: true},
		"unpause": {lease: true},
		"bucket":  {lease: true, paused: true, bucket: errors.New("unreachable")},
	} {
		if Run(context.Background(), readiness.Assess, config(), set(supportmatrix.V3DSLVersion), install).Ready {
			t.Errorf("%s failure did not block", name)
		}
	}
	if Run(context.Background(), readiness.Assess, config(), set(supportmatrix.V3DSLVersion), nil).Ready {
		t.Error("missing database did not block")
	}
}

func TestCutoverCheckUnavailableBackendsFailClosed(t *testing.T) {
	var pg PGInstall
	_, pausedErr := pg.AdmissionsPaused(context.Background())
	bucketErr := pg.BucketReachable(context.Background())
	for _, err := range []error{pausedErr, bucketErr} {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "not available until #") {
			t.Fatalf("err = %v, want UnavailableError", err)
		}
	}
	report := Run(context.Background(), readiness.Assess, config(), set(supportmatrix.V3DSLVersion),
		fakeInstall{lease: true, paused: true, bucket: bucketErr})
	if report.Ready || report.Install[3].Status != StatusUnavailable {
		t.Fatalf("report = %+v, want unavailable bucket to block", report)
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	created := false
	steps := []Step{{Name: "lease", Run: func(context.Context) (bool, error) {
		changed := !created
		created = true
		return changed, nil
	}}}
	first, err := Bootstrap(context.Background(), steps)
	if err != nil || !first[0].Changed {
		t.Fatalf("first run = %+v, %v", first, err)
	}
	second, err := Bootstrap(context.Background(), steps)
	if err != nil || second[0].Changed {
		t.Fatalf("second run = %+v, %v; want unchanged", second, err)
	}
}

func TestBootstrapRefusesBeforeChangingAnything(t *testing.T) {
	ran := false
	steps := []Step{
		{Name: "schema", Run: func(context.Context) (bool, error) { ran = true; return true, nil }},
		{Name: "bucket", Issue: 7813},
	}
	_, err := Bootstrap(context.Background(), steps)
	if err == nil || ran || !strings.Contains(err.Error(), "bucket: not available until #7813") {
		t.Fatalf("err = %v ran = %v", err, ran)
	}
}

func TestBootstrapPrecheckRefusesBeforeAnyMutation(t *testing.T) {
	ran := false
	steps := []Step{
		{Name: "schema", Run: func(context.Context) (bool, error) { ran = true; return true, nil }},
		{Name: "bucket", Check: func(context.Context) error { return errors.New("endpoint down") },
			Run: func(context.Context) (bool, error) { ran = true; return true, nil }},
	}
	_, err := Bootstrap(context.Background(), steps)
	if err == nil || ran || !strings.Contains(err.Error(), "bucket: endpoint down") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v ran = %v", err, ran)
	}
}

func TestShippedPGStepsHaveChecks(t *testing.T) {
	for _, s := range PGSteps(nil, "i", ControllerLeaseName, "b", "h") {
		if s.Issue == 0 && s.Check == nil {
			t.Errorf("shipped step %q has no Check; bootstrap could mutate before probing it", s.Name)
		}
	}
}
