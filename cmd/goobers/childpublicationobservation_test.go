package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

func TestChildPublicationObservationDaemonCustodyAndCurrentPolicy(t *testing.T) {
	t.Setenv("PUBLICATION_HUMAN_TOKEN", "human-only-observation-secret")
	p := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	repo := apiv1.InteractiveRepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}, InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: p.Issuer, Subject: p.Subject}}}, Actions: []apiv1.InteractiveAction{"run.intervene", "repository.read"}, Credentials: apiv1.InteractiveCredentialBindings{Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: repo, CredentialRef: "observation"}}}}}}
	u := &upSession{}
	u.l = instance.NewLayout(t.TempDir())
	u.setup = &schedulerSetup{Config: &instance.Config{InteractiveCredentials: []instance.InteractiveCredential{{Name: "observation", Provider: "github", Owner: "acme", Repository: "web", Token: instance.TokenRef{Env: "PUBLICATION_HUMAN_TOKEN"}}}}, Definitions: &instance.ConfigSet{Gaggles: []apiv1.Gaggle{gaggle}}, SharedRegistry: journal.NewRegistryScrubber(), RunnerRegistry: newDaemonRunnerRegistry()}
	if err := u.configureInteractiveAccess(); err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{RunID: strings.Repeat("a", 32), Gaggle: "web", Workflow: "generated", WorkflowVersion: 1}
	writer, err := journal.Create(u.l.ForGaggle("web").RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		t.Fatal(err)
	}
	id, err = reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	target := childpublication.ObservationTarget{Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}, ChildRunID: id.RunID}
	called := 0
	use := func(_ context.Context, observer childpublication.EffectObserver, held *journal.Run, events []journal.Event) error {
		called++
		if observer == nil || held.Phase() != journal.PhaseFailed || len(events) == 0 {
			t.Fatal("missing verified observation custody")
		}
		if strings.Contains(string(u.setup.SharedRegistry.Scrub([]byte("human-only-observation-secret"))), "human-only-observation-secret") {
			t.Fatal("credential was not registered before callback")
		}
		if release, ok := u.setup.RunnerRegistry.acquireChildCustody(id.RunID); ok {
			release()
			t.Fatal("overlapping custody")
		}
		if other, _, err := journal.TryRecover(held.Dir()); err == nil {
			_ = other.Close()
			t.Fatal("overlapping journal writer")
		}
		return nil
	}
	observe := u.childPublicationObservation()
	release := u.setup.RunnerRegistry.Track(id.RunID, id.Workflow, &runner.Runner{})
	if err = observe(t.Context(), p, id, target, use); err == nil || called != 0 {
		t.Fatal("active owner was bypassed", err)
	}
	release()
	if err = observe(t.Context(), p, id, target, use); err != nil || called != 1 {
		t.Fatal("stopped run refused", err)
	}
	changed := id
	changed.Workflow = "different"
	if err = observe(t.Context(), p, changed, target, use); err == nil || called != 1 {
		t.Fatal("identity mismatch was accepted", err)
	}
	gaggle.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	if err = u.setup.InteractiveAccess.Apply([]apiv1.Gaggle{gaggle}, nil); err != nil {
		t.Fatal(err)
	}
	if err = observe(t.Context(), p, id, target, use); err == nil || called != 1 {
		t.Fatal("revoked intervention still observed", err)
	}
}

func TestChildPublicationObservationRefusesUnrecognizedPhase(t *testing.T) {
	for _, phase := range []journal.RunPhase{"", "unknown", journal.PhaseRunning} {
		if publicationObservationStopped(phase) {
			t.Fatalf("accepted %q", phase)
		}
	}
}
