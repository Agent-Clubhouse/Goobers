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

func TestWebhookRecipientsAreOnePerWorkflow(t *testing.T) {
	w := eligible()
	w.Spec.Triggers = []apiv1.Trigger{
		{Type: apiv1.TriggerWebhook, Events: []string{"pull_request"}},
		{Type: apiv1.TriggerWebhook, Events: []string{"pull_request", "issues"}},
		{Type: apiv1.TriggerWebhook, Events: []string{"issues"}, Enabled: new(false)},
	}
	set := workflowSet(w)
	set.Gaggles[0].Spec.Project = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Acme", Name: "App"}
	cfg, err := cloudconfig.NewConfiguration(readiness.Assess, &recordingActivator{},
		func(context.Context) (cloudconfig.Candidate, error) {
			return cloudconfig.Candidate{Digest: "v1", Config: denyConfig(), Set: set}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.WebhookRecipients("pull_request", "acme", "app"); !errors.Is(err, cloudconfig.ErrNoGeneration) {
		t.Fatalf("before the first apply = %v, want ErrNoGeneration", err)
	}
	if err := cfg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	digest, subs, err := cfg.WebhookRecipients("pull_request", "acme", "app")
	if err != nil || digest != "v1" || len(subs) != 1 {
		t.Fatalf("pull_request recipients = %q %+v %v; want exactly one for the workflow", digest, subs, err)
	}
	if _, subs, _ := cfg.WebhookRecipients("issues", "acme", "app"); len(subs) != 1 {
		t.Fatalf("issues recipients = %d, want 1 (the disabled trigger adds none)", len(subs))
	}
	if _, subs, _ := cfg.WebhookRecipients("pull_request", "acme", "other"); len(subs) != 0 {
		t.Fatalf("other repository recipients = %d, want 0", len(subs))
	}
}
