package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/sessionops"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

func repairHostFixture(t *testing.T) (*upSession, sessionops.SourceContext, triggerqueue.PRRepairCommandInput) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0700); err != nil {
		t.Fatal(err)
	}
	queue, err := triggerqueue.Open(filepath.Join(layout.SchedulerDir(), "commands.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	input, inputs := repairRetentionInput(t, queue, time.Now())
	o := input.Origin
	id := journal.RunIdentity{RunID: o.RunID, Gaggle: "gaggle", Workflow: "interactive-session", WorkflowDigest: o.GooberDigest, GooberDigest: o.GooberDigest, ConfigGeneration: o.ConfigGeneration, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:" + o.SessionID + ":" + o.TurnID}, Session: &journal.SessionLineage{Gaggle: "gaggle", SessionID: o.SessionID, TurnID: o.TurnID, MessageID: o.MessageID, AcceptanceID: inputs.AcceptanceID, EnvelopeDigest: o.EnvelopeDigest, InputDigest: o.InputDigest}}
	run, err := journal.Create(layout.ForGaggle("gaggle").RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	if err = queue.ObserveSessionRun(t.Context(), inputs.AcceptanceID, id.RunID, time.Now()); err != nil {
		t.Fatal(err)
	}
	repo := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}
	g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "gaggle"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo"}, Workbench: &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "code", Kind: "documents", Repository: &repo, Paths: []string{"plan.md"}}}}, InteractiveAccess: &apiv1.InteractiveAccessPolicy{Actions: []apiv1.InteractiveAction{"repository.read", "pr.repair", "session.message"}, Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: input.Scope.Actor.Issuer, Subject: input.Scope.Actor.Subject}}}, Credentials: apiv1.InteractiveCredentialBindings{Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: repo, CredentialRef: "human"}}}}}}
	t.Setenv("REPAIR_HUMAN_TOKEN", "host-human-token")
	registry := journal.NewRegistryScrubber()
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, []instance.InteractiveCredential{{Name: "human", Provider: "github", Owner: "org", Repository: "repo", Token: instance.TokenRef{Env: "REPAIR_HUMAN_TOKEN"}}}, interactiveaccess.Dependencies{Registrar: registry})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := permissions.BeginSessionExecution(t.Context(), httpapi.Principal{Issuer: input.Scope.Actor.Issuer, Subject: input.Scope.Actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	manager, err := worktree.NewManager(layout.ForGaggle(g.Name).WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	setup := &schedulerSetup{Config: &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "org", Name: "repo"}}}, Definitions: &instance.ConfigSet{Gaggles: []apiv1.Gaggle{g}}, RunnerRegistry: newDaemonRunnerRegistry(), WorktreesByGaggle: map[string]*worktree.Manager{g.Name: manager}, InteractiveAccess: permissions, SharedRegistry: registry}
	u := &upSession{}
	u.l = layout
	u.setup = setup
	u.durableTriggers = &durableTriggerService{queue: queue}
	u.credentialPlane = &daemonCredentialService{grants: &stageGrantIssuer{}}
	return u, sessionops.SourceContext{Identity: id, Actor: input.Scope.Actor, Lease: lease, RetainedGaggle: g}, input
}

type hostRepairClient struct {
	target  providers.RepairPullRequest
	apply   func(context.Context) error
	effects int
}

func (f *hostRepairClient) InspectRepairPullRequest(context.Context, providers.RepositoryRef, string) (providers.RepairPullRequest, error) {
	return f.target, nil
}
func (f *hostRepairClient) ReadRepairFile(context.Context, providers.RepairPullRequest, string) (providers.RepairFile, error) {
	return providers.RepairFile{}, errors.New("unused")
}
func (f *hostRepairClient) ApplyPullRequestRepair(ctx context.Context, _ providers.PullRequestRepair) (providers.PRRepairResult, error) {
	if err := f.apply(ctx); err != nil {
		return providers.PRRepairResult{}, err
	}
	f.effects++
	f.target.HeadSHA = strings.Repeat("c", 40)
	return providers.PRRepairResult{MutationAttempted: true, Acknowledged: true, CommitID: f.target.HeadSHA}, nil
}
func (f *hostRepairClient) ObservePullRequestRepair(context.Context, providers.PullRequestRepair) (providers.PRRepairObservation, error) {
	return providers.PRRepairObservation{Matches: true, CommitID: f.target.HeadSHA}, nil
}

func TestSessionPRRepairHostInstallsActualClaimsAdmissionAndWorkspaceCustody(t *testing.T) {
	u, source, input := repairHostFixture(t)
	client := &hostRepairClient{target: *input.Target, apply: func(ctx context.Context) error {
		lock, err := acquireClaimLock(filepath.Join(u.l.SchedulerDir(), claimLockFileName), "competing", 15*time.Millisecond, time.Now())
		if err == nil {
			_ = lock.Release()
			t.Fatal("provider effect outside claims lock")
		}
		if release, ok := u.setup.RunnerRegistry.TrackCompatible("other", &runner.Runner{}); ok {
			release()
			t.Fatal("new runner entered held repair")
		}
		short, cancel := context.WithTimeout(ctx, 15*time.Millisecond)
		defer cancel()
		clone, err := childRepoCloneURL(source.RetainedGaggle.Spec.Project)
		if err != nil {
			t.Fatal(err)
		}
		err = u.setup.WorktreesByGaggle["gaggle"].WithUnoccupiedBranch(short, clone, "fix", func(context.Context) error { t.Fatal("repository mutex released before receipt"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		return nil
	}}
	u.installSessionPRRepair(func(_ context.Context, _ workbenchservice.ReadBinding, credential interactiveaccess.Credential) (workbenchservice.PRRepairClient, error) {
		if credential.Value != "host-human-token" {
			t.Fatal("wrong credential")
		}
		return client, nil
	})
	if u.setup.SessionPRRepair == nil {
		t.Fatal("repair not installed")
	}
	principal := httpapi.Principal{Issuer: source.Actor.Issuer, Subject: source.Actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}
	assertAvailability := func(want bool) {
		t.Helper()
		caps, err := u.setup.InteractiveAccess.InteractiveCapabilities(t.Context(), principal, "gaggle")
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range caps.Actions {
			if action.Action == "pr.repair" {
				if action.Available != want {
					t.Fatal(action, want)
				}
				return
			}
		}
		t.Fatal("missing repair capability")
	}
	assertAvailability(false)
	u.setup.InteractiveAccess.SetSessionsAvailable(true)
	assertAvailability(true)
	repair, err := u.setup.SessionPRRepair(t.Context(), source)
	if err != nil || repair == nil {
		t.Fatal(err)
	}
	result, err := repair.Repair(t.Context(), "one", input.Request)
	if err != nil || result.State != "confirmed" || client.effects != 1 {
		t.Fatal(result, err, client.effects)
	}
	if release, ok := u.setup.RunnerRegistry.TrackCompatible("later", &runner.Runner{}); !ok {
		t.Fatal("admission remained blocked")
	} else {
		release()
	}
	t.Setenv("REPAIR_HUMAN_TOKEN", "")
	if receipt, err := repair.Command(t.Context(), result.ID); err != nil || receipt.ID != result.ID {
		t.Fatal(receipt, err)
	}
}
func TestPRRepairHostRefusesUnsettledOrUnqualifiedCustody(t *testing.T) {
	for _, mode := range []string{"legacy-claim", "active-run", "unpublished-owner", "remote", "shared", "pinned", "unknown-root", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			u, source, input := repairHostFixture(t)
			ctx := t.Context()
			switch mode {
			case "legacy-claim":
				ledger, err := localscheduler.OpenClaimLedger(filepath.Join(u.l.SchedulerDir(), claimLedgerFileName))
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = ledger.Claim("pr/12", "unknown", "work", time.Hour); err != nil {
					t.Fatal(err)
				}
			case "active-run":
				run, err := journal.Create(u.l.ForGaggle("gaggle").RunsDir(), journal.RunIdentity{RunID: "active", Gaggle: "gaggle", Workflow: "automation", WorkspaceRepository: &source.RetainedGaggle.Spec.Project}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = run.Close() })
			case "unpublished-owner":
				release := u.setup.RunnerRegistry.Track("unpublished", "automation", &runner.Runner{})
				defer release()
			case "remote":
				u.setup.Config.Engine = &instance.EngineConfig{}
			case "shared":
				u.setup.Definitions.Workflows = []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Readiness: apiv1.ReadinessConditions{ClaimVisibility: "shared"}}}}
			case "pinned":
				u.setup.Config.Repos[0].Workspace = &instance.RepoWorkspaceConfig{Pinned: true}
			case "unknown-root":
				if err := os.MkdirAll(u.l.ForGaggle("old").WorkcopiesDir(), 0700); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			u.setup.PRRepairCustody = newPRRepairCatalog(u.setup, true)
			called := false
			c := prRepairCustodian{layout: u.l, setup: u.setup, runID: source.Identity.RunID}
			if err := c.scope(ctx, *input.Target, func(context.Context) error { called = true; return nil }); err == nil || called {
				t.Fatal(err, called)
			}
		})
	}
}
