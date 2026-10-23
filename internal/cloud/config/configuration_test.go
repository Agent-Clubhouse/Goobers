package config_test

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/config/readiness"
)

// The seam p3-startfence (#7850) consumes; declared here until it lands.
type generationAdmission interface {
	Admit(digest, gaggle, workflow string) error
}
type configuration interface {
	generationAdmission
	Reload(context.Context) error
}

var _ configuration = (*cloudconfig.Configuration)(nil)

func TestConfigurationSeam(t *testing.T) {
	next := cloudconfig.Candidate{Digest: "v1", Config: denyConfig(), Set: workflowSet(eligible())}
	var loadErr error
	cfg, err := cloudconfig.NewConfiguration(readiness.Assess, &recordingActivator{},
		func(context.Context) (cloudconfig.Candidate, error) { return next, loadErr })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Fails closed before anything is applied.
	if err := cfg.Admit("v1", "g", "ok"); !errors.Is(err, cloudconfig.ErrIneligible) {
		t.Fatalf("Admit before Reload = %v, want ErrIneligible", err)
	}
	if err := cfg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Admit("v1", "g", "ok"); err != nil {
		t.Fatalf("eligible workflow refused: %v", err)
	}
	// An ineligible workflow is refused before any dispatch.
	if err := cfg.Admit("v1", "g", "outbox"); !errors.Is(err, cloudconfig.ErrIneligible) {
		t.Fatalf("unassessed workflow admitted: %v", err)
	}

	// A hot reload to an unsupported workflow is refused atomically: the
	// running generation keeps admitting, the refused one admits nothing.
	bad := eligible()
	bad.Spec.Tasks[0].Experiment = &apiv1.BanditExperiment{}
	next = cloudconfig.Candidate{Digest: "v2", Config: denyConfig(), Set: workflowSet(bad)}
	if err := cfg.Reload(ctx); !errors.Is(err, cloudconfig.ErrIneligible) {
		t.Fatalf("unsupported reload = %v, want ErrIneligible", err)
	}
	if err := cfg.Admit("v2", "g", "ok"); err == nil {
		t.Fatal("refused generation admits")
	}
	if err := cfg.Admit("v1", "g", "ok"); err != nil {
		t.Fatalf("running generation lost after refused reload: %v", err)
	}
	if st := cfg.Status(); st.Applied != "v1" || st.RejectedDigest != "v2" {
		t.Fatalf("status = %+v", st)
	}

	// A loader failure changes nothing.
	loadErr = errors.New("object store unreachable")
	if err := cfg.Reload(ctx); err == nil {
		t.Fatal("loader failure reported success")
	}
	if err := cfg.Admit("v1", "g", "ok"); err != nil {
		t.Fatalf("running generation lost after loader failure: %v", err)
	}
}

func TestConfigurationPersistsAcrossLeaseEpochs(t *testing.T) {
	cand := cloudconfig.Candidate{Digest: "v1", Config: denyConfig(), Set: workflowSet(eligible())}
	cfg, err := cloudconfig.NewConfiguration(readiness.Assess, &recordingActivator{},
		func(context.Context) (cloudconfig.Candidate, error) { return cand, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Each epoch reuses the one Configuration: nothing to rebuild, nothing lost.
	for epoch := 0; epoch < 3; epoch++ {
		if err := cfg.Admit("v1", "g", "ok"); err != nil {
			t.Fatalf("epoch %d: %v", epoch, err)
		}
	}
}

func TestNewConfigurationRequiresDependencies(t *testing.T) {
	if _, err := cloudconfig.NewConfiguration(nil, &recordingActivator{}, nil); err == nil {
		t.Fatal("missing dependencies accepted")
	}
}

func TestPinForServesOnlyTheAppliedGenerationsDigests(t *testing.T) {
	pins := map[string]cloudconfig.Pins{"g/ok": {WorkflowDigest: "wf", GooberDigest: "goober"}, "g/half": {WorkflowDigest: "wf"}}
	cfg, err := cloudconfig.NewConfiguration(readiness.Assess, &recordingActivator{},
		func(context.Context) (cloudconfig.Candidate, error) {
			return cloudconfig.Candidate{Digest: "v1", Config: denyConfig(), Set: workflowSet(eligible()), Pins: pins}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := cfg.PinFor(ctx, "v1", "g", "ok"); !errors.Is(err, cloudconfig.ErrPinUnavailable) {
		t.Fatalf("before the first apply = %v, want ErrPinUnavailable", err)
	}
	if err := cfg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if wf, goober, err := cfg.PinFor(ctx, "v1", "g", "ok"); err != nil || wf != "wf" || goober != "goober" {
		t.Fatalf("PinFor = %q %q %v", wf, goober, err)
	}
	for name, args := range map[string][3]string{
		"stale generation": {"v0", "g", "ok"},
		"unknown workflow": {"v1", "g", "missing"},
		"half a pin":       {"v1", "g", "half"},
	} {
		if _, _, err := cfg.PinFor(ctx, args[0], args[1], args[2]); !errors.Is(err, cloudconfig.ErrPinUnavailable) {
			t.Errorf("%s: %v, want ErrPinUnavailable", name, err)
		}
	}
}

func TestAppliedPinsDoNotAliasTheCandidatesMap(t *testing.T) {
	pins := map[string]cloudconfig.Pins{"g/ok": {WorkflowDigest: "wf", GooberDigest: "goober"}}
	cfg, err := cloudconfig.NewConfiguration(readiness.Assess, &recordingActivator{},
		func(context.Context) (cloudconfig.Candidate, error) {
			return cloudconfig.Candidate{Digest: "v1", Config: denyConfig(), Set: workflowSet(eligible()), Pins: pins}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	pins["g/ok"] = cloudconfig.Pins{WorkflowDigest: "mutated", GooberDigest: "mutated"}
	delete(pins, "g/other")
	if wf, _, err := cfg.PinFor(context.Background(), "v1", "g", "ok"); err != nil || wf != "wf" {
		t.Fatalf("applied pins changed with the candidate's map: %q, %v", wf, err)
	}
}

func TestInstanceConfigIsTheAppliedCandidatesPolicy(t *testing.T) {
	policy := denyConfig()
	cfg, err := cloudconfig.NewConfiguration(readiness.Assess, &recordingActivator{},
		func(context.Context) (cloudconfig.Candidate, error) {
			return cloudconfig.Candidate{Digest: "v1", Config: policy, Set: workflowSet(eligible())}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceConfig() != nil {
		t.Fatal("instance config served before the first apply")
	}
	if err := cfg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceConfig() != policy {
		t.Fatal("instance config is not the applied candidate's")
	}
}
