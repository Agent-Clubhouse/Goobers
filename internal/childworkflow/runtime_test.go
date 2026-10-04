package childworkflow

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func runtimeFixture(t *testing.T) (*Runtime, *authorityFixture, apiv1.InvocationEnvelope) {
	t.Helper()
	f := newAuthorityFixture(t)
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	key, err := podauth.NewSignedKey(bytes.Repeat([]byte("x"), 32))
	if err != nil {
		t.Fatal(err)
	}
	definitions := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{f.pinned.Admission.Gaggle}}
	runtime, err := NewRuntime(RuntimeConfig{Queue: queue, Grants: key, Endpoint: "http://localhost:8080", Definitions: definitions,
		OpenJournal: f.resolver.OpenJournal,
		LoadPinnedStage: func(ctx context.Context, _ *instance.ConfigSet, id journal.RunIdentity, stage string) (PinnedStageAdmission, error) {
			return f.resolver.LoadPinnedStage(ctx, id, stage)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, f, f.start(t, 1, 1, false)
}

func TestRuntimeConnectsJournalGrantSubmissionAndPolicyReload(t *testing.T) {
	r, _, env := runtimeFixture(t)
	access, closeAccess, err := r.Acquire(t.Context(), env, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeAccess() })
	result, err := r.HTTPService().StartChildWorkflow(t.Context(), access.BearerToken, env.RunID, "plan-child", []byte(validProposal))
	if err != nil || result.RunID == "" {
		t.Fatalf("journal-backed start = %+v, %v", result, err)
	}
	unchanged := copyRuntimeDefinitions(r.definitions)
	if err := r.ApplyDefinitions(t.Context(), unchanged, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.HTTPService().ChildWorkflowStatus(t.Context(), access.BearerToken, env.RunID, "plan-child"); err != nil {
		t.Fatalf("identical reload revoked a live grant: %v", err)
	}
	changed := copyRuntimeDefinitions(r.definitions)
	changed.Gaggles[0].Annotations = map[string]string{"changed-policy": "true"}
	published := false
	if err := r.ApplyDefinitions(t.Context(), changed, func() error {
		row, err := r.queue.ChildAuthority(t.Context(), triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}, env.ChildWorkflowOrigin.StageOccurrence)
		if err != nil || !row.Revoked {
			t.Fatalf("publication preceded grant revocation: %v", err)
		}
		published = true
		return nil
	}); err != nil || !published {
		t.Fatalf("reload failed: %v", err)
	}
	if _, err := r.HTTPService().ChildWorkflowStatus(t.Context(), access.BearerToken, env.RunID, "plan-child"); err == nil {
		t.Fatal("revoked token retained HTTP access")
	}
	if _, _, err := r.Acquire(t.Context(), env, journal.NewRegistryScrubber()); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("revoked attempt reminted on reload: %v", err)
	}
	child, err := r.queue.GetChild(t.Context(), triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: env.Gaggle, ParentRunID: env.RunID}, StageOccurrence: env.ChildWorkflowOrigin.StageOccurrence, InvocationKey: "plan-child"})
	if err != nil || child.RunID != result.RunID {
		t.Fatalf("policy change destroyed accepted custody: %v", err)
	}
}

func TestRuntimeFailedPolicyPublicationKeepsOldSnapshot(t *testing.T) {
	r, _, env := runtimeFixture(t)
	access, closeAccess, err := r.Acquire(t.Context(), env, journal.NewRegistryScrubber())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeAccess() })
	changed := copyRuntimeDefinitions(r.definitions)
	changed.Gaggles[0].Annotations = map[string]string{"policy": "new"}
	refused := errors.New("scheduler refused definitions")
	if err := r.ApplyDefinitions(t.Context(), changed, func() error { return refused }); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if r.definitions.Gaggles[0].Annotations["policy"] != "" {
		t.Fatal("failed publication installed authority")
	}
	grant, err := r.issuer.Grants.VerifyChildWorkflowGrant(access.BearerToken)
	if err != nil {
		t.Fatal(err)
	}
	origin := Origin{GrantID: grant.ID, Gaggle: grant.Gaggle, RunID: grant.RunID, StageOccurrence: grant.StageOccurrence, AttemptID: grant.AttemptID, ConfigDigest: grant.ConfigDigest, PolicyDigest: grant.PolicyDigest}
	if err := r.queue.CheckChildAuthority(t.Context(), origin.Binding(grant.ExpiresAt), time.Now()); !errors.Is(err, triggerqueue.ErrChildAuthorityChanged) {
		t.Fatalf("failed publication resurrected a revoked grant: %v", err)
	}
}

func TestRuntimeDefinitionsAreIndependentAndGaggleScoped(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	other := r.definitions.Gaggles[0].DeepCopy()
	other.Name = "other"
	next := copyRuntimeDefinitions(r.definitions)
	next.Gaggles = append(next.Gaggles, *other)
	if err := r.ApplyDefinitions(t.Context(), next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	next.Gaggles[0].Name = "mutated-outside"
	if r.definitions.Gaggles[0].Name != "web" {
		t.Fatal("authority aliases caller catalog")
	}
	before := r.policies["web"]
	next = copyRuntimeDefinitions(r.definitions)
	next.Gaggles[1].Annotations = map[string]string{"policy": "changed"}
	if err := r.ApplyDefinitions(t.Context(), next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if r.policies["web"] != before {
		t.Fatal("other gaggle changed authority digest")
	}
}
