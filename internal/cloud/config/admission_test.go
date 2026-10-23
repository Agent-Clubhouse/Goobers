package config_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/config/readiness"
	"github.com/goobers/goobers/internal/cloudreadiness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func denyConfig() *instance.Config {
	return &instance.Config{
		Placement: &instance.PlacementConfig{SelfExecution: "deny"},
		Runners: []instance.RunnerEntry{{
			Name: "worker", Host: "ghcr.io/example/worker:v1",
			Provides: instance.RunnerProvides{OS: instance.RunnerOSLinux, Shell: true},
		}},
	}
}

func workflowSet(w apiv1.Workflow) *instance.ConfigSet {
	w.Spec.Gaggle = "g"
	return &instance.ConfigSet{
		Gaggles:   []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}}},
		Workflows: []apiv1.Workflow{w},
		Goobers: []apiv1.Goober{{
			ObjectMeta: metav1.ObjectMeta{Name: "coder"},
			Spec:       apiv1.GooberSpec{Gaggle: "g", Harness: apiv1.HarnessCopilot},
		}},
	}
}

func task() apiv1.Task {
	return apiv1.Task{
		Name: "t", Type: apiv1.TaskDeterministic, Goal: "run",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}},
	}
}

func eligible() apiv1.Workflow {
	return apiv1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "ok"}, DSLVersion: supportmatrix.V3DSLVersion,
		Spec: apiv1.WorkflowSpec{Start: "t", Tasks: []apiv1.Task{task()}},
	}
}

// ineligible returns one fixture per RUNTIME code.
func ineligible() map[string]apiv1.Workflow {
	r001 := eligible()
	r001.Name = "r001"
	r001.Spec.Tasks[0].Experiment = &apiv1.BanditExperiment{}
	r002 := eligible()
	r002.Name = "r002"
	r002.Spec.Tasks[0].RunsOn = &apiv1.RunsOn{Capabilities: []string{"no-such-capability"}}
	r003 := eligible()
	r003.Name = "r003"
	r003.DSLVersion = supportmatrix.V2DSLVersion
	r004 := eligible()
	r004.Name = "r004"
	r004.Spec.Readiness.ClaimVisibility = "shared"
	return map[string]apiv1.Workflow{
		cloudreadiness.CodeFeature: r001, cloudreadiness.CodePlacement: r002, cloudreadiness.CodeDSL: r003,
		cloudreadiness.CodeControlPlane: r004,
	}
}

type recordingActivator struct {
	mu      sync.Mutex
	digests []string
}

func (a *recordingActivator) Activate(_ context.Context, candidate cloudconfig.Candidate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.digests = append(a.digests, candidate.Digest)
	return nil
}

func apply(a *cloudconfig.Applier, digest string, cfg *instance.Config, set *instance.ConfigSet) error {
	return a.Apply(context.Background(), cloudconfig.Candidate{Digest: digest, Config: cfg, Set: set})
}

func hasCode(refusal *cloudconfig.Refusal, code string) bool {
	for _, findings := range refusal.Workflows {
		for _, f := range findings {
			if f.Code == code {
				return true
			}
		}
	}
	return false
}

func TestCloudConfigApplyRefusesIneligibleWorkflow(t *testing.T) {
	act := &recordingActivator{}
	applier := cloudconfig.NewApplier(readiness.Assess, act)
	if err := apply(applier, "sha256:good", denyConfig(), workflowSet(eligible())); err != nil {
		t.Fatalf("eligible generation refused: %v", err)
	}
	for code, w := range ineligible() {
		t.Run(code, func(t *testing.T) {
			err := apply(applier, "sha256:bad-"+code, denyConfig(), workflowSet(w))
			var refusal *cloudconfig.Refusal
			if !errors.As(err, &refusal) || !errors.Is(err, cloudconfig.ErrIneligible) {
				t.Fatalf("Apply err = %v, want *Refusal", err)
			}
			if !hasCode(refusal, code) {
				t.Fatalf("refusal lacks %s blocker: %+v", code, refusal.Workflows)
			}
			var text strings.Builder
			if err := refusal.WriteText(&text); err != nil || !strings.Contains(text.String(), code) || !strings.Contains(text.String(), "unblock:") {
				t.Fatalf("status text missing %s or unblocking action: %q (%v)", code, text.String(), err)
			}
			st := applier.Status()
			if st.Applied != "sha256:good" || st.RejectedDigest != "sha256:bad-"+code || st.Refusal == nil {
				t.Fatalf("status = %+v, want running generation kept and rejection named", st)
			}
			if err := applier.Guard().Admit("sha256:good", "g", "ok"); err != nil {
				t.Fatalf("refused candidate unbound the running generation: %v", err)
			}
		})
	}
	if len(act.digests) != 1 || act.digests[0] != "sha256:good" {
		t.Fatalf("activated %v, want only the eligible generation", act.digests)
	}
}

func TestCloudConfigNamesEveryBlocker(t *testing.T) {
	set := workflowSet(eligible())
	set.Workflows = nil
	for _, w := range ineligible() {
		w.Spec.Gaggle = "g"
		set.Workflows = append(set.Workflows, w)
	}
	refusal := cloudconfig.Admit(readiness.Assess, "d", denyConfig(), set)
	if refusal == nil || len(refusal.Workflows) != len(ineligible()) {
		t.Fatalf("Admit = %+v, want every ineligible fixture blocked", refusal)
	}
}

func TestEngineOnlyAdmissionGuard(t *testing.T) {
	set := workflowSet(eligible())
	for _, w := range ineligible() {
		w.Spec.Gaggle = "g"
		set.Workflows = append(set.Workflows, w)
	}
	// A generation applied by an older binary with looser rules: the guard
	// still admits only what this binary assesses eligible.
	gen := cloudconfig.Evaluate(readiness.Assess, "d1", denyConfig(), set)
	guard := cloudconfig.NewGuard()
	guard.Replace(gen)
	if err := guard.Admit("d1", "g", "ok"); err != nil {
		t.Fatalf("eligible workflow refused: %v", err)
	}
	for _, name := range []string{"r001", "r002", "r003", "never-assessed"} {
		if err := guard.Admit("d1", "g", name); !errors.Is(err, cloudconfig.ErrIneligible) {
			t.Errorf("Admit(%s) = %v, want ErrIneligible", name, err)
		}
	}
	if err := guard.Admit("stale", "g", "ok"); !errors.Is(err, cloudconfig.ErrIneligible) {
		t.Errorf("stale digest admitted: %v", err)
	}
	if err := cloudconfig.NewGuard().Admit("d1", "g", "ok"); !errors.Is(err, cloudconfig.ErrIneligible) {
		t.Errorf("unbound guard admitted: %v", err)
	}
}

func TestGuardReloadReplacesGenerationAtomically(t *testing.T) {
	act := &recordingActivator{}
	applier := cloudconfig.NewApplier(readiness.Assess, act)
	set := workflowSet(eligible())
	if err := apply(applier, "v1", denyConfig(), set); err != nil {
		t.Fatal(err)
	}
	// Same name gains an unsupported feature: refused, old generation stays.
	reloaded := workflowSet(eligible())
	reloaded.Workflows[0].Spec.Tasks[0].Experiment = &apiv1.BanditExperiment{}
	if err := apply(applier, "v2", denyConfig(), reloaded); err == nil {
		t.Fatal("ineligible reload applied")
	}
	if err := applier.Guard().Admit("v2", "g", "ok"); err == nil {
		t.Fatal("guard admits under a refused generation")
	}
	if err := applier.Guard().Admit("v1", "g", "ok"); err != nil {
		t.Fatalf("running generation lost: %v", err)
	}
	// Reload to selfExecution=allow is refused too.
	allow := denyConfig()
	allow.Placement.SelfExecution = "allow"
	if err := apply(applier, "v3", allow, workflowSet(eligible())); err == nil {
		t.Fatal("selfExecution=allow reload applied")
	}
	// A clean reload with a new workflow replaces the whole set.
	next := workflowSet(eligible())
	extra := eligible()
	extra.Name = "extra"
	extra.Spec.Gaggle = "g"
	next.Workflows = append(next.Workflows, extra)
	if err := apply(applier, "v4", denyConfig(), next); err != nil {
		t.Fatal(err)
	}
	if err := applier.Guard().Admit("v4", "g", "extra"); err != nil {
		t.Fatalf("new generation's workflow refused: %v", err)
	}
	if err := applier.Guard().Admit("v1", "g", "ok"); err == nil {
		t.Fatal("old generation still admitted after replacement")
	}
}

func TestCloudConfigRejectsSelfExecutionAllow(t *testing.T) {
	for _, cfg := range []*instance.Config{
		{Placement: &instance.PlacementConfig{SelfExecution: "allow"}},
		{}, // the local default resolves to allow
	} {
		refusal := cloudconfig.Admit(readiness.Assess, "d", cfg, workflowSet(eligible()))
		if refusal == nil || !strings.Contains(refusal.Policy, "selfExecution") {
			t.Fatalf("Admit(%+v) = %+v, want selfExecution policy refusal", cfg, refusal)
		}
		if !errors.Is(refusal, cloudconfig.ErrIneligible) {
			t.Fatal("policy refusal not classified as ErrIneligible")
		}
	}
}

func TestMissingConfigurationFailsClosed(t *testing.T) {
	act := &recordingActivator{}
	applier := cloudconfig.NewApplier(readiness.Assess, act)
	for name, c := range map[string]cloudconfig.Candidate{
		"nil set":    {Digest: "d", Config: denyConfig()},
		"nil config": {Digest: "d", Set: workflowSet(eligible())},
		"no digest":  {Config: denyConfig(), Set: workflowSet(eligible())},
	} {
		if err := applier.Apply(context.Background(), c); err == nil {
			t.Errorf("%s activated", name)
		}
	}
	if err := cloudconfig.NewApplier(nil, act).Apply(context.Background(),
		cloudconfig.Candidate{Digest: "d", Config: denyConfig(), Set: workflowSet(eligible())}); err == nil {
		t.Error("nil assessor activated")
	}
	if len(act.digests) != 0 {
		t.Fatalf("activated %v, want nothing", act.digests)
	}
}

type orderedActivator struct {
	first, release chan struct{}
	active         string
}

func (a *orderedActivator) Activate(_ context.Context, candidate cloudconfig.Candidate) error {
	digest := candidate.Digest
	a.active = digest
	if digest == "A" {
		close(a.first)
		<-a.release
	}
	return nil
}

func TestConcurrentApplyReportsActiveGeneration(t *testing.T) {
	act := &orderedActivator{first: make(chan struct{}), release: make(chan struct{})}
	applier := cloudconfig.NewApplier(readiness.Assess, act)
	errs := make(chan error, 2)
	go func() { errs <- apply(applier, "A", denyConfig(), workflowSet(eligible())) }()
	<-act.first
	go func() { errs <- apply(applier, "B", denyConfig(), workflowSet(eligible())) }()
	close(act.release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := applier.Status().Applied; got != act.active {
		t.Fatalf("reported generation=%s actual activated=%s", got, act.active)
	}
	if err := applier.Guard().Admit(act.active, "g", "ok"); err != nil {
		t.Fatalf("guard not bound to the active generation: %v", err)
	}
}

func TestConfigPackageClosureExcludesRunner(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/goobers/goobers/internal/cloud/config").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "github.com/goobers/goobers/internal/runner" || dep == "github.com/goobers/goobers/internal/bootstrap" {
			t.Fatalf("internal/cloud/config depends on %s", dep)
		}
	}
}
