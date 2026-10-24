package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/cloud/startpin"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestConfigurationConstructorFailsClosedWithoutItsDependencies(t *testing.T) {
	owner := func() *lease.Handle { return nil }
	manager := &lease.Manager{}
	for name, tc := range map[string]struct {
		db      databaseServices
		manager *lease.Manager
		owner   func() *lease.Handle
		want    string
	}{
		"no config directory": {db: databaseServices{InstanceID: "instance-a"}, manager: manager, owner: owner, want: "--config-dir"},
		"no object store":     {db: databaseServices{InstanceID: "instance-a", ConfigDir: t.TempDir()}, manager: manager, owner: owner, want: "--object-store-url"},
	} {
		t.Run(name, func(t *testing.T) {
			var planes controlPlanes
			err := constructConfiguration(t.Context(), tc.db, controlAuth{}, tc.manager, tc.owner, &planes)
			if err == nil || !strings.Contains(err.Error(), tc.want) || planes.Configuration != nil {
				t.Fatalf("construct = %v (configuration %v), want a refusal naming %q", err, planes.Configuration != nil, tc.want)
			}
		})
	}
}

func TestLeasedActivatorRefusesToPublishWithoutALiveHandle(t *testing.T) {
	activator := leasedActivator{manager: &lease.Manager{}, owner: func() *lease.Handle { return nil }}
	if err := activator.Activate(t.Context(), cloudconfig.Candidate{Digest: "sha256:x"}); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("Activate without a live handle = %v, want ErrStale", err)
	}
}

func TestControlConfigurationDirectoryFailsClosedAtBoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"unset": "", "absent": filepath.Join(t.TempDir(), "absent"), "a file": file} {
		var db databaseServices
		if err := bindControlConfiguration(&db, dir); err == nil || db.ConfigDir != "" {
			t.Fatalf("%s: bound %q (%v)", name, db.ConfigDir, err)
		}
	}
	var db databaseServices
	if err := bindControlConfiguration(&db, t.TempDir()); err != nil || db.ConfigDir == "" {
		t.Fatalf("a real directory was refused: %v", err)
	}
}

func TestConfigurationIsAControllerSideSeamWithAConstructor(t *testing.T) {
	for _, seam := range controlAssembly {
		if seam.field != "Configuration" {
			continue
		}
		if !seam.controller || seam.construct == nil {
			t.Fatalf("configuration seam = %+v, want a controller-side constructor", seam)
		}
		return
	}
	t.Fatal("no Configuration seam")
}

func TestControlCommandRequiresAConfigDirectory(t *testing.T) {
	t.Setenv("GOOBERS_CONFIG_DIR", "")
	for _, args := range [][]string{{"control", "--help"}} {
		code, stdout, _ := runForTest(args...)
		if code != 0 || !strings.Contains(stdout, "--config-dir") || !strings.Contains(stdout, "GOOBERS_CONFIG_DIR") {
			t.Fatalf("help omits the config directory: %d %s", code, stdout)
		}
	}
}

func TestLeasedGenerationsRefuseToPinWithoutALiveHandle(t *testing.T) {
	source := leasedGenerations{owner: func() *lease.Handle { return nil }}
	if _, err := source.Acquire(t.Context(), "sha256:x"); !errors.Is(err, lease.ErrStale) {
		t.Fatalf("Acquire without a live handle = %v, want ErrStale", err)
	}
}

// TestDirSourcePinsEveryWorkflowOfARealInstance runs the real pin computation
// through the loader on an initialized instance: a generation the control role
// admits must carry digests, or every start is refused as unpinnable.
func TestDirSourcePinsEveryWorkflowOfARealInstance(t *testing.T) {
	root := initDemo(t)
	policy := filepath.Join(root, "instance.yaml")
	data, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, append(data, []byte("placement:\n  selfExecution: deny\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := (cloudconfig.DirSource{Root: root, InstanceID: "instance-a", Pins: startpin.Compute}).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Pins) == 0 || len(candidate.Pins) != len(candidate.Set.Workflows) {
		t.Fatalf("pins = %v for %d workflows, want one per workflow", candidate.Pins, len(candidate.Set.Workflows))
	}
	for key, pins := range candidate.Pins {
		if pins.WorkflowDigest == "" || pins.GooberDigest == "" {
			t.Fatalf("%s has an incomplete pin %+v", key, pins)
		}
	}
}

// TestAnUnparseableTriggerRefusesTheGenerationAtApplyTime proves the composed
// assessor refuses a generation the cloud scheduler could not tick, in the
// config status shape: the offending workflow is named with its finding.
func TestAnUnparseableTriggerRefusesTheGenerationAtApplyTime(t *testing.T) {
	cfg := &instance.Config{Placement: &instance.PlacementConfig{SelfExecution: "deny"}}
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "g"}}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly"}, DSLVersion: supportmatrix.V3DSLVersion,
			Spec: apiv1.WorkflowSpec{
				Gaggle: "g", Start: "t",
				Tasks:    []apiv1.Task{{Name: "t", Type: apiv1.TaskDeterministic, Goal: "run", Run: &apiv1.DeterministicRun{Command: []string{"true"}}}},
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "not a cron"}},
			},
		}},
	}
	refusal := cloudconfig.Admit(configurationAssessor, "sha256:x", cfg, set)
	if refusal == nil {
		t.Fatal("a generation with an unparseable cron trigger was admitted")
	}
	findings := refusal.Workflows["g/nightly"]
	if len(findings) == 0 || findings[len(findings)-1].Code != schedentries.CodeTrigger {
		t.Fatalf("refusal = %+v, want TRIGGER001 on g/nightly", refusal.Workflows)
	}
}

type recordingActivator struct{ candidates []cloudconfig.Candidate }

func (a *recordingActivator) Activate(_ context.Context, c cloudconfig.Candidate) error {
	a.candidates = append(a.candidates, c)
	return nil
}

// referenceInstance mounts the reference workflows the way the control role's
// config volume does: instance.yaml beside a config/ definitions tree.
func referenceInstance(t *testing.T) string {
	t.Helper()
	reference := filepath.Join("..", "..", "reference-workflows")
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, "config"), os.DirFS(reference)); err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(filepath.Join(reference, "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	// The cloud refuses a placement that can execute stages.
	policy = append(policy, []byte("\nplacement:\n  selfExecution: deny\n")...)
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), policy, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// A generation without admission pins refuses every start (Admit finds no pin
// for any workflow), so the candidates the control role's source produces must
// carry them. The source is the one the control role builds, so a merge that
// drops the pin computation fails here instead of in production.
func TestControlSourceProducesCandidatesThatCarryAdmissionPins(t *testing.T) {
	candidate, err := controlDirSource(referenceInstance(t), "instance-a").Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Pins) == 0 || len(candidate.Pins) != len(candidate.Set.Workflows) {
		t.Fatalf("candidate carries %d pins for %d workflows", len(candidate.Pins), len(candidate.Set.Workflows))
	}
	for _, workflow := range candidate.Set.Workflows {
		pins := candidate.Pins[workflow.Spec.Gaggle+"/"+workflow.Name]
		if pins.WorkflowDigest == "" || pins.GooberDigest == "" {
			t.Fatalf("workflow %s/%s has no usable pins: %+v", workflow.Spec.Gaggle, workflow.Name, pins)
		}
	}
}

// The constructor the control role uses wires the real assessor: the reference workflows
// need local runners, so the cloud must refuse the whole generation rather than
// publish it.
func TestControlConfigurationRefusesAGenerationTheCloudCannotRun(t *testing.T) {
	activator := &recordingActivator{}
	configuration, err := newControlConfiguration(activator, controlDirSource(referenceInstance(t), "instance-a"))
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	configuration.SetReporter(func(err error) { reported = append(reported, err) })
	if err := configuration.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(activator.candidates) != 0 || len(reported) != 1 || !strings.Contains(reported[0].Error(), "not eligible") {
		t.Fatalf("activated %d candidates, reported %v; want a refusal and no publication", len(activator.candidates), reported)
	}
}

// The control role's configuration must refuse a generation whose triggers the
// cloud scheduler cannot tick, through the SAME constructor production uses. A
// seam that wired readiness.Assess alone would still pass the direct assessor
// test above, so this one pushes an unparseable cron through
// newControlConfiguration and looks for TRIGGER001 in the refusal it publishes.
// The loader validates triggers too, so a bad cron cannot be written to disk
// (the scheduler's own parse is the second line); the source hook plants it in
// the loaded set instead, after the pins are computed, exactly where a validator
// drift would let one through.
func TestControlConfigurationNamesAnUnparseableTriggerInItsRefusal(t *testing.T) {
	source := controlDirSource(referenceInstance(t), "instance-a")
	pins := source.Pins
	source.Pins = func(configDir string, cfg *instance.Config, set *instance.ConfigSet) (map[string]cloudconfig.Pins, error) {
		computed, err := pins(configDir, cfg, set)
		set.Workflows[0].Spec.Triggers = append(set.Workflows[0].Spec.Triggers,
			apiv1.Trigger{Type: apiv1.TriggerSchedule, Schedule: "61 4 * * *"})
		return computed, err
	}
	activator := &recordingActivator{}
	configuration, err := newControlConfiguration(activator, source)
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	configuration.SetReporter(func(err error) { reported = append(reported, err) })
	if err := configuration.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(activator.candidates) != 0 || len(reported) != 1 {
		t.Fatalf("activated %d candidates, reported %v; want one refusal and no publication", len(activator.candidates), reported)
	}
	var refusal *cloudconfig.Refusal
	if !errors.As(reported[0], &refusal) {
		t.Fatalf("reported %v, want a refusal", reported[0])
	}
	found := false
	for _, findings := range refusal.Workflows {
		for _, finding := range findings {
			found = found || finding.Code == schedentries.CodeTrigger
		}
	}
	if !found {
		t.Fatalf("refusal = %+v, want TRIGGER001: the schedule assessor is not wired into the control configuration", refusal.Workflows)
	}
}
