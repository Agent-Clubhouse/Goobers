package workbenchservice

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

type repairServiceClient struct {
	mu                          sync.Mutex
	target                      providers.RepairPullRequest
	held                        atomic.Bool
	reads, effects, credentials int
	lost, noObservation         bool
	entered, release            chan struct{}
}

func (f *repairServiceClient) InspectRepairPullRequest(context.Context, providers.RepositoryRef, string) (providers.RepairPullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.target, nil
}
func (f *repairServiceClient) ReadRepairFile(_ context.Context, target providers.RepairPullRequest, path string) (providers.RepairFile, error) {
	return providers.RepairFile{Commit: target.HeadSHA, Path: path, Present: true, BlobID: strings.Repeat("b", 40), Mode: "100644", Content: "before"}, nil
}
func (f *repairServiceClient) ApplyPullRequestRepair(ctx context.Context, value providers.PullRequestRepair) (providers.PRRepairResult, error) {
	if !f.held.Load() || providers.ValidatePullRequestRepair(value) != nil {
		return providers.PRRepairResult{}, errors.New("missing trusted writer custody")
	}
	f.mu.Lock()
	f.effects++
	f.target.HeadSHA = strings.Repeat("c", 40)
	if f.effects > 1 {
		f.target.HeadSHA = strings.Repeat("d", 40)
	}
	head := f.target.HeadSHA
	f.mu.Unlock()
	if f.entered != nil {
		close(f.entered)
		select {
		case <-ctx.Done():
			return providers.PRRepairResult{MutationAttempted: true}, ctx.Err()
		case <-f.release:
		}
	}
	return providers.PRRepairResult{MutationAttempted: true, Acknowledged: !f.lost, CommitID: head}, nil
}
func (f *repairServiceClient) ObservePullRequestRepair(context.Context, providers.PullRequestRepair) (providers.PRRepairObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noObservation {
		return providers.PRRepairObservation{}, errors.New("observation unavailable")
	}
	return providers.PRRepairObservation{Matches: true, CommitID: f.target.HeadSHA}, nil
}
func repairServiceFixture(t *testing.T) (*SessionPRRepair, *repairServiceClient, *interactiveaccess.Service) {
	t.Helper()
	read, g, principal := documentsFixture(t, func(*http.Request) (*http.Response, error) { t.Fatal("unconfigured provider use"); return nil, nil })
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read", "pr.repair", "session.message"}
	registry, scrubber := journal.DefaultScrubber()
	permissions, err := interactiveaccess.New([]apiv1.Gaggle{g}, repositoryCredentials(), interactiveaccess.Dependencies{Registrar: registry})
	if err != nil {
		t.Fatal(err)
	}
	read.Permissions = permissions
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := queue.Close(); err != nil {
			t.Error(err)
		}
	})
	selection := &sessioning.PRRepairTarget{SourceBindingID: "strategy", Repository: sessioning.RepairRepository{Provider: "github", Owner: "acme", Name: "code"}, RepositorySourceID: "77", ID: "42", SourceID: "99", ExpectedHeadSHA: strings.Repeat("a", 40)}
	actor := sessioning.Actor{Issuer: principal.Issuer, Subject: principal.Subject}
	id := resolutionTurn(t, queue, g.Name, actor, selection)
	lease, err := permissions.BeginSessionExecution(t.Context(), principal, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Close)
	f := &repairServiceClient{target: providers.RepairPullRequest{Repository: providers.RepositoryRef{Provider: "github", Owner: "acme", Name: "code"}, RepositoryID: "77", ID: "42", StableID: "99", Head: "repair", Base: "main", HeadSHA: selection.ExpectedHeadSHA, BaseSHA: strings.Repeat("f", 40), Open: true}}
	var claims sync.Mutex
	service := &PRRepairService{Queue: queue, Scrubber: scrubber, Client: func(_ context.Context, bound ReadBinding, credential interactiveaccess.Credential) (PRRepairClient, error) {
		if credential.Value != "human-read-canary" || bound.Source.Spec.Name != "strategy" {
			t.Fatal("wrong interactive credential/target")
		}
		f.mu.Lock()
		f.credentials++
		f.mu.Unlock()
		return f, nil
	}, WithCustody: func(ctx context.Context, target providers.RepairPullRequest, use func(context.Context) error) error {
		claims.Lock()
		defer claims.Unlock()
		if target.RepositoryID != "77" || target.ID != "42" {
			t.Fatal("foreign custody")
		}
		f.held.Store(true)
		defer f.held.Store(false)
		return use(ctx)
	}}
	s, err := service.ForSession(t.Context(), &g, lease, actor, id)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, permissions
}
func repairServiceRequest() sessioning.PRRepairRequest {
	value := "fixed"
	return sessioning.PRRepairRequest{RequestID: "model-one", ExpectedHeadSHA: strings.Repeat("a", 40), Rationale: "Fix the selected PR's reported problem", Changes: []sessioning.PRRepairChange{{Path: "file.txt", PreviousBlob: strings.Repeat("b", 40), Content: &value}}}
}
func TestSessionPRRepairConfirmedAndUnknownNeverReplayEffect(t *testing.T) {
	for _, mode := range []string{"confirmed", "lost", "observation"} {
		t.Run(mode, func(t *testing.T) {
			s, f, _ := repairServiceFixture(t)
			f.lost, f.noObservation = mode == "lost", mode == "observation"
			inspection, err := s.Inspect(t.Context(), "")
			if err != nil || inspection.HeadSHA != s.Target().ExpectedHeadSHA {
				t.Fatal(inspection, err)
			}
			file, err := s.ReadFile(t.Context(), "file.txt", "")
			if err != nil || file.BlobID != strings.Repeat("b", 40) {
				t.Fatal(file, err)
			}
			request := repairServiceRequest()
			result, err := s.Repair(t.Context(), "host-one", request)
			state := "unknown"
			if mode == "confirmed" {
				state = "confirmed"
			}
			if err != nil || result.State != state || result.Receipt == nil || f.effects != 1 {
				t.Fatal(result, err, f.effects)
			}
			reads, credentials := f.reads, f.credentials
			t.Setenv("HUMAN_BACKLOG", "")
			replay, err := s.Repair(t.Context(), "host-one", request)
			if err != nil || replay.ID != result.ID || f.reads != reads || f.credentials != credentials || f.effects != 1 {
				t.Fatal(replay, err)
			}
			receipt, err := s.Command(t.Context(), result.ID)
			if err != nil || receipt.ID != result.ID || f.credentials != credentials {
				t.Fatal(receipt, err)
			}
			request.Rationale = "Different intent"
			if _, err = s.Repair(t.Context(), "host-one", request); err == nil {
				t.Fatal("changed replay")
			}
		})
	}
}
func TestSessionPRRepairConfirmedDescendantOnly(t *testing.T) {
	s, f, _ := repairServiceFixture(t)
	first, err := s.Repair(t.Context(), "first", repairServiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Inspect(t.Context(), ""); err == nil {
		t.Fatal("unselected current head")
	}
	if _, err = s.Inspect(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	next := repairServiceRequest()
	next.RequestID = "second"
	next.ParentCommandID = first.ID
	next.ExpectedHeadSHA = first.Receipt.CommitID
	second, err := s.Repair(t.Context(), "second", next)
	if err != nil || second.State != "confirmed" || second.SelectedHeadSHA != first.SelectedHeadSHA || f.effects != 2 {
		t.Fatal(second, err)
	}
	f.target.HeadSHA = strings.Repeat("e", 40)
	if _, err = s.Inspect(t.Context(), second.ID); err == nil {
		t.Fatal("foreign head inherited")
	}
}
func TestSessionPRRepairRefusesWrongSourceActorEvidenceAndSecret(t *testing.T) {
	for _, mode := range []string{"actor", "selection", "source", "input", "secret", "custody", "branch"} {
		t.Run(mode, func(t *testing.T) {
			s, f, _ := repairServiceFixture(t)
			request := repairServiceRequest()
			switch mode {
			case "actor":
				s.actor.Subject = "other"
			case "selection":
				s.selection.SourceID = "100"
			case "source":
				s.retained.Spec.Workbench.Sources[0].Repository.Name = "other"
			case "input":
				s.identity.Session.InputDigest = sessioning.Digest([]byte("other"))
			case "secret":
				value := "human-read-canary"
				request.Changes[0].Content = &value // discovered during credential resolution
			case "custody":
				s.service.WithCustody = func(context.Context, providers.RepairPullRequest, func(context.Context) error) error {
					return interactiveaccess.ErrDenied
				}
			case "branch":
				original := s.service.WithCustody
				s.service.WithCustody = func(ctx context.Context, target providers.RepairPullRequest, use func(context.Context) error) error {
					f.target.Head = "other"
					return original(ctx, target, use)
				}
			}
			if _, err := s.Repair(t.Context(), "refused", request); err == nil || f.effects != 0 {
				t.Fatal(err, f.effects)
			}
		})
	}
}
func TestSessionPRRepairRevocationJoinsUnknownReceipt(t *testing.T) {
	s, f, permissions := repairServiceFixture(t)
	f.entered = make(chan struct{})
	f.release = make(chan struct{})
	f.noObservation = true
	done := make(chan error, 1)
	go func() {
		result, err := s.Repair(t.Context(), "revoked", repairServiceRequest())
		if err == nil && result.State != "unknown" {
			err = errors.New("no unknown receipt")
		}
		s.lease.Close()
		done <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no native attempt")
	}
	changed := s.retained.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = nil
	applied := make(chan error, 1)
	go func() { applied <- permissions.Apply([]apiv1.Gaggle{*changed}, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("policy join deadlock")
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	record, err := s.service.Queue.FindPRRepairCommand(t.Context(), s.scope(), "revoked")
	if err != nil || record.State != "unknown" || record.Receipt == nil || f.held.Load() {
		t.Fatal(record, err)
	}
}

func TestSessionPRRepairConcurrentDuplicateReportsCustodyWithoutSecondEffect(t *testing.T) {
	s, f, _ := repairServiceFixture(t)
	f.entered, f.release = make(chan struct{}), make(chan struct{})
	completed := make(chan sessioning.PRRepairCommandView, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := s.Repair(t.Context(), "same", repairServiceRequest())
		completed <- result
		errs <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("attempt did not start")
	}
	replay, err := s.Repair(t.Context(), "same", repairServiceRequest())
	if err != nil || replay.State != "attempting" {
		t.Fatal(replay, err)
	}
	close(f.release)
	result := <-completed
	if err = <-errs; err != nil || result.ID != replay.ID || result.State != "confirmed" || f.effects != 1 {
		t.Fatal(result, err)
	}
}
