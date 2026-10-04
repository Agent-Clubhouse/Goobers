package childmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

type checkObserver struct {
	reads  int
	fail   bool
	commit string
}

func (o *checkObserver) GetBranch(_ context.Context, _ providers.RepositoryRef, head string) (providers.BranchSummary, bool, error) {
	o.reads++
	if o.fail {
		return providers.BranchSummary{}, false, errors.New("provider unavailable")
	}
	return providers.BranchSummary{Name: head, SHA: o.commit}, true, nil
}
func (*checkObserver) FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error) {
	return providers.PullRequestResult{}, false, errors.New("unexpected PR read")
}

func TestHumanPublicationCheckReplaysAuditAndKeepsCancelledResultImmutable(t *testing.T) {
	for _, humanEpoch := range []bool{false, true} {
		name := "original"
		if humanEpoch {
			name = "human epoch"
		}
		t.Run(name, func(t *testing.T) { testHumanPublicationCheck(t, humanEpoch) })
	}
}
func testHumanPublicationCheck(t *testing.T, humanEpoch bool) {
	t.Setenv("OBSERVATION_TOKEN", "human-only-token")
	layout := instance.NewLayout(t.TempDir())
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = queue.Close() }()
	parent := strings.Repeat("a", 32)
	child, envelope := acceptMonitorChild(t, queue, "own", parent, "plan/1", "publish")
	start, err := queue.ChildStart(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	id := journal.RunIdentity{RunID: child.RunID, Gaggle: "own", Workflow: "generated", WorkflowVersion: 1, WorkflowDigest: envelope.WorkflowDigest, GooberDigest: envelope.ParentGooberDigest, ConfigGeneration: envelope.ConfigGeneration, Child: &journal.ChildLineage{Gaggle: "own", ParentRunID: parent, ParentWorkflow: "parent", StageOccurrence: child.Identity.StageOccurrence, InvocationKey: child.Identity.InvocationKey, AcceptanceID: child.AcceptanceID, SourceDigest: child.ProposalDigest, EnvelopeDigest: journal.Digest(start.Payload)}}
	run, err := journal.Create(layout.ForGaggle("own").RunsDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(journal.Event{Type: journal.EventRunFinished, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	if humanEpoch {
		child, id = restartMonitorPublication(t, queue, layout, child, id)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	raw, _ := json.Marshal(childpublication.BranchIntent{Version: 1, RunID: id.RunID, Lineage: *id.Child, Stage: "push", Repository: repo, Base: "main", Head: "goobers/children/" + id.RunID, Commit: strings.Repeat("b", 40)})
	intent, err := queue.PrepareChildExecutionPublication(t.Context(), child.Identity, id.RunID, "branch", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = queue.BeginChildExecutionPublicationEffect(t.Context(), intent, id.RunID); err != nil {
		t.Fatal(err)
	}
	sealed := triggerqueue.ChildResult{Receipt: []byte("immutable result " + id.RunID)}
	sealed.ReceiptDigest = journal.Digest(sealed.Receipt)
	if err = queue.KeepChildResult(t.Context(), child, sealed); err != nil {
		t.Fatal(err)
	}
	if err = queue.SetChildState(t.Context(), child.Identity, triggerqueue.ChildStateUpdate{ExecutionRunID: id.RunID, Expected: child.State, State: triggerqueue.ChildFailed, ResultRef: sealed.ReceiptDigest}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = queue.FenceChildParent(t.Context(), child.Identity.ChildParent, "human", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := queue.GetChild(t.Context(), child.Identity)
	if err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}
	repository := apiv1.InteractiveRepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "own"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}, InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Operators: []apiv1.InteractiveHumanGrant{{Issuer: p.Issuer, Subject: p.Subject}}}, Actions: []apiv1.InteractiveAction{"run.intervene", "repository.read"}, Credentials: apiv1.InteractiveCredentialBindings{Repositories: []apiv1.InteractiveRepositoryCredential{{Repository: repository, CredentialRef: "human-code"}}}}}}
	scrubber := journal.NewRegistryScrubber()
	policy, err := interactiveaccess.New([]apiv1.Gaggle{gaggle}, []instance.InteractiveCredential{{Name: "human-code", Provider: "github", Owner: "acme", Repository: "web", Token: instance.TokenRef{Env: "OBSERVATION_TOKEN"}}}, interactiveaccess.Dependencies{Registrar: scrubber})
	if err != nil {
		t.Fatal(err)
	}
	observer := &checkObserver{fail: true, commit: strings.Repeat("b", 40)}
	service := &Service{Layout: layout, Queue: queue, Permissions: policy, Scrubber: scrubber}
	for _, field := range []string{"gaggle", "parent", "source", "envelope", "workflow", "generation"} {
		changed := id
		lineage := *id.Child
		changed.Child = &lineage
		switch field {
		case "gaggle":
			changed.Gaggle, lineage.Gaggle = "foreign", "foreign"
		case "parent":
			lineage.ParentRunID = strings.Repeat("c", 32)
		case "source":
			lineage.SourceDigest = journal.Digest([]byte("changed source"))
		case "envelope":
			lineage.EnvelopeDigest = journal.Digest([]byte("changed envelope"))
		case "workflow":
			changed.Workflow = "different"
		case "generation":
			changed.ConfigGeneration = journal.Digest([]byte("changed generation"))
		}
		if _, err = service.publicationChild(t.Context(), changed); err == nil {
			t.Fatalf("publication custody accepted changed %s", field)
		}
	}
	service.Observe = func(ctx context.Context, p httpapi.Principal, id journal.RunIdentity, target childpublication.ObservationTarget, use func(context.Context, childpublication.EffectObserver, *journal.Run, []journal.Event) error) error {
		if target.Repository != repo || target.ChildRunID != child.RunID || target.SourceRunID != id.RunID {
			t.Fatal("changed observation target")
		}
		return policy.WithRunObservationCredential(ctx, p, id.Gaggle, interactiveaccess.Target{Kind: "repository", Repository: repository}, func(ctx context.Context, credential interactiveaccess.Credential) error {
			if credential.Value != "human-only-token" {
				t.Fatal("wrong credential")
			}
			dir, err := layout.FindRunDir(id.RunID)
			if err != nil {
				return err
			}
			writer, report, err := journal.TryRecover(dir, journal.WithScrubber(scrubber))
			if err != nil {
				return err
			}
			defer func() { _ = writer.Close() }()
			return use(ctx, observer, writer, report.Events)
		})
	}
	page, err := service.ListChildWorkflows(t.Context(), p, id.RunID, "")
	if err != nil || len(page.Publications) != 1 || !page.PublicationCheckAvailable || !page.Publications[0].NeedsHuman {
		t.Fatal(page, err)
	}
	command := apicontract.ChildPublicationCheckRequest{Action: "branch", ExpectedIntentDigest: intent.Digest}
	if _, err = service.CheckChildPublication(t.Context(), p, id.RunID, "one", command); err == nil {
		t.Fatal("failed provider observation reported success")
	}
	observer.fail = false
	result, err := service.CheckChildPublication(t.Context(), p, id.RunID, "one", command)
	if err != nil || result.Publication.State != "confirmed" || result.RunID != id.RunID || result.Publication.SourceRunID != id.RunID || result.Publication.ExecutionEpoch != id.Child.ExecutionEpoch {
		t.Fatal(result, err)
	}
	replay, err := service.CheckChildPublication(t.Context(), p, id.RunID, "one", command)
	if err != nil || !reflect.DeepEqual(result, replay) || observer.reads != 2 {
		t.Fatal("same-key check was not replayed", replay, err, observer.reads)
	}
	dir, err := layout.FindRunDir(id.RunID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := reader.OperatorMessages()
	if err != nil || len(records) != 1 || records[0].Request.PrincipalRef != p.Issuer+":"+p.Subject || records[0].Outcome == nil {
		t.Fatal(records, err)
	}
	after, err := queue.GetChild(t.Context(), child.Identity)
	if err != nil || before != after {
		t.Fatal("human check changed child result", after, err)
	}
	var ref journal.Ref
	if err = json.Unmarshal([]byte(records[0].Outcome.Detail), &ref); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ref.Path), []byte("tampered observation"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = service.CheckChildPublication(t.Context(), p, id.RunID, "one", command); err == nil || observer.reads != 2 {
		t.Fatal("tampered recorded observation was replayed or reread provider", err)
	}
	gaggle.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	if err = policy.Apply([]apiv1.Gaggle{gaggle}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = service.CheckChildPublication(t.Context(), p, id.RunID, "one", command); err == nil || observer.reads != 2 {
		t.Fatal("replay bypassed current policy", err)
	}
}
