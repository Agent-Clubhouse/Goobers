package sharedclaims

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

type pinnedJournal struct{ reader journal.RunReader }

func (j pinnedJournal) OpenRun(_ context.Context, ref journal.RunRef) (journal.RunReader, error) {
	id, err := j.reader.Identity()
	if err != nil {
		return nil, err
	}
	if id.RunID != ref.ID {
		return nil, errors.New("run not found")
	}
	return j.reader, nil
}

type pinnedRuns struct{ binding terminalwriter.Binding }

func (r pinnedRuns) Bind(context.Context, string) (terminalwriter.Binding, error) {
	return r.binding, nil
}

type resolverWriter struct{ calls int }

func (w *resolverWriter) SharedClaimStore(context.Context, string, providers.RepositoryRef) (sharedclaim.Store, error) {
	w.calls++
	return emptyRemote{}, nil
}
func (w *resolverWriter) SharedClaimVisibility(context.Context, string, providers.RepositoryRef) (sharedclaim.Visibility, error) {
	return nil, errors.New("labels unavailable")
}

type emptyRemote struct{}

func (emptyRemote) Read(context.Context, string) (sharedclaim.Observation, error) {
	return sharedclaim.Observation{Now: time.Now()}, nil
}
func (emptyRemote) CompareAndSwap(context.Context, string, string, sharedclaim.Record) error {
	return nil
}

func resolverFixture(t *testing.T, mode string) (Resolver, *journal.Run, *resolverWriter) {
	t.Helper()
	definition := workflow.Definition{Name: "claim", Version: 1, Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "claim", Readiness: apiv1.ReadinessConditions{ClaimVisibility: mode},
		Tasks: []apiv1.Task{{Name: "claim", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}}}}}
	machine, err := workflow.Compile(definition, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(machine.Def)
	if err != nil {
		t.Fatal(err)
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{InstanceID: "instance", RunID: "run-a", Workflow: "claim", WorkflowVersion: 1, Gaggle: "example", WorkflowDigest: machine.Digest(), ConfigGeneration: "generation", WorkspaceRepository: &repo},
		map[string][]byte{journal.PinnedWorkflowDefinitionInputName: raw}, journal.WithInputIntegrity(map[string]apiv1.Integrity{journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	writer := &resolverWriter{}
	return Resolver{InstanceID: "instance", Journal: pinnedJournal{reader}, Verify: func(context.Context, journal.RunIdentity) error { return nil }, Runs: pinnedRuns{terminalwriter.Binding{RunID: "run-a", Gaggle: "example", Workspace: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}}},
		Writer: writer, Register: func(context.Context, providers.RepositoryRef) error { return nil }}, run, writer
}

func TestResolverPinsModeAndOwnerAndAllowsTerminalRelease(t *testing.T) {
	r, run, _ := resolverFixture(t, "shared")
	ctx := t.Context()
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	binding, err := r.Admission(ctx, key, "run-a", "claim")
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Admission(ctx, key, "run-a", "claim")
	if err != nil || again.Owner != binding.Owner {
		t.Fatalf("restart owner=%+v, %v", again, err)
	}
	if mode, err := r.Visibility(ctx, "run-a"); err != nil || mode != "shared" {
		t.Fatalf("mode=%s, %v", mode, err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Admission(ctx, key, "run-a", "claim"); err == nil {
		t.Fatal("terminal admission accepted")
	}
	if mode, err := r.Visibility(ctx, "run-a"); mode != "" || !errors.Is(err, claimsclient.ErrSharedExecutionExpired) {
		t.Fatalf("terminal execution visibility=%q, %v", mode, err)
	}
	entry := claimsclient.Entry{Gaggle: key.Gaggle, Provider: key.Provider, ExternalID: key.ExternalID, RunID: "run-a", Workflow: "claim", SharedOwner: binding.Owner}
	if released, err := r.Release(ctx, entry); err != nil || released.Owner != binding.Owner {
		t.Fatalf("terminal release=%+v, %v", released, err)
	}
	entry.SharedOwner.Token = "forged"
	if _, err := r.Release(ctx, entry); err == nil {
		t.Fatal("forged owner accepted")
	}
}

type phaseFailureReader struct {
	journal.RunReader
	err error
}

func (r phaseFailureReader) PhaseBounded(context.Context) (journal.RunPhase, error) {
	return "", r.err
}

func TestSharedVisibilityFailsClosedOnPhaseReadError(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := resolverFixture(t, mode)
			reader, err := r.Journal.OpenRun(t.Context(), journal.RunRef{ID: "run-a"})
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("phase read unavailable")
			r.Journal = pinnedJournal{reader: phaseFailureReader{RunReader: reader, err: failure}}
			got, err := r.Visibility(t.Context(), "run-a")
			if mode == "shared" {
				if got != "" || !errors.Is(err, failure) {
					t.Fatalf("failed phase granted visibility=%q, %v", got, err)
				}
			} else if got != "local" || err != nil {
				t.Fatalf("local policy changed=%q, %v", got, err)
			}
		})
	}
}

func TestResolverRefusesUntrustedBindingsBeforeCredentials(t *testing.T) {
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			r, _, writer := resolverFixture(t, mode)
			ctx := t.Context()
			key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
			for _, bad := range []struct{ run, workflow, gaggle string }{{"missing", "claim", "example"}, {"run-a", "other", "example"}, {"run-a", "claim", "wrong"}} {
				key.Gaggle = bad.gaggle
				if _, err := r.Admission(ctx, key, bad.run, bad.workflow); err == nil {
					t.Fatalf("bad binding accepted: %+v", bad)
				}
			}
			if writer.calls != 0 {
				t.Fatal("credentials used before identity validation")
			}
		})
	}
	r, _, writer := resolverFixture(t, "shared")
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	r.InstanceID = "another-instance"
	if _, err := r.Admission(t.Context(), key, "run-a", "claim"); err == nil || writer.calls != 0 {
		t.Fatal("cross-instance identity accepted")
	}
}

func TestResolverLocalNeedsNoSharedCredentialsAndAuxiliaryOwnersStayDistinct(t *testing.T) {
	r, _, writer := resolverFixture(t, "local")
	key := claimsclient.Key{Gaggle: "example", Provider: "github", ExternalID: "42"}
	if binding, err := r.Admission(t.Context(), key, "run-a", "claim"); err != nil || binding != nil || writer.calls != 0 {
		t.Fatalf("local binding=%+v, %v", binding, err)
	}
	r, _, _ = resolverFixture(t, "shared")
	ordinary, err := r.Admission(t.Context(), key, "run-a", "claim")
	if err != nil {
		t.Fatal(err)
	}
	auxiliary, err := r.Admission(t.Context(), key, "run-a/backlog-reconcile/42/1", "backlog-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if auxiliary.RemoteKey != ordinary.RemoteKey || auxiliary.Owner == ordinary.Owner {
		t.Fatal("auxiliary owner overlaps ordinary run")
	}
	if _, err := r.Admission(t.Context(), key, "run-a/backlog-reconcile/42/1/extra", "backlog-reconcile"); err == nil {
		t.Fatal("malformed auxiliary accepted")
	}
}

func TestOwnerUsesCanonicalTimeAndCompleteIncarnation(t *testing.T) {
	identity := journal.RunIdentity{RunID: "run", Gaggle: "example", Workflow: "claim", WorkflowDigest: "sha256:digest", StartedAt: time.Now()}
	owner, err := ownerFor("instance", identity, "repo/42")
	if err != nil {
		t.Fatal(err)
	}
	identity.StartedAt = identity.StartedAt.In(time.FixedZone("offset", 3600))
	if got, err := ownerFor("instance", identity, "repo/42"); err != nil || got != owner {
		t.Fatalf("time-zone changed owner=%+v, %v", got, err)
	}
	identity.StartedAt = identity.StartedAt.Add(time.Nanosecond)
	if got, err := ownerFor("instance", identity, "repo/42"); err != nil || got == owner {
		t.Fatal("new incarnation reused owner")
	}
}
