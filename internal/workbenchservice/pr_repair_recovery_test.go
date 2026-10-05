package workbenchservice

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func TestPRRepairRecoveryAfterClosedTurnAndCurrentHumanAuthorization(t *testing.T) {
	s, f, permissions := repairServiceFixture(t)
	f.lost = true
	f.noObservation = true
	prior, err := s.Repair(t.Context(), "lost", repairServiceRequest())
	if err != nil || prior.State != "unknown" {
		t.Fatal(prior, err)
	}
	s.lease.Close()
	actor := s.actor
	if _, err = s.service.Queue.CloseSession(t.Context(), triggerqueue.SessionCommand{Gaggle: s.retained.Name, Actor: actor, RequestID: "close", RequestDigest: sessioning.Digest([]byte("close"))}, s.origin.SessionID, "done", time.Now()); err != nil {
		t.Fatal(err)
	}
	checker := httpapi.Principal{Issuer: actor.Issuer, Subject: "second-operator", Roles: []httpapi.Role{httpapi.RoleOperate}}
	g := s.retained.DeepCopy()
	g.Spec.InteractiveAccess.Humans.Operators = append(g.Spec.InteractiveAccess.Humans.Operators, apiv1.InteractiveHumanGrant{Issuer: checker.Issuer, Subject: checker.Subject})
	if err = permissions.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	recovery := &PRRepairRecoveryService{Queue: s.service.Queue, Permissions: permissions, Client: s.service.Client}
	reads := f.credentials
	got, err := recovery.Get(t.Context(), checker, g.Name, prior.ID)
	if err != nil || got.Actor != actor || f.credentials != reads {
		t.Fatal(got, err)
	}
	got, err = recovery.Check(t.Context(), checker, g.Name, prior.ID)
	if err != nil || got.State != "unknown" || len(got.Observations) != 1 {
		t.Fatal(got, err)
	}
	f.noObservation = false
	got, err = recovery.Check(t.Context(), checker, g.Name, prior.ID)
	if err != nil || got.State != "observed-applied" || !reflect.DeepEqual(got.Receipt, prior.Receipt) || got.Observations[1].Checker.Subject != checker.Subject || f.effects != 1 {
		t.Fatal(got, err)
	}
	reads = f.credentials
	if _, err = recovery.Check(t.Context(), checker, g.Name, prior.ID); err != nil || f.credentials != reads {
		t.Fatal("settled proof repeated provider read", err)
	}
	for _, mode := range []string{"viewer", "wrong-gaggle", "revoked-repair", "changed-source", "missing-credential"} {
		t.Run(mode, func(t *testing.T) {
			current := g.DeepCopy()
			p := checker
			gaggle := g.Name
			switch mode {
			case "viewer":
				p.Roles = []httpapi.Role{httpapi.RoleView}
			case "wrong-gaggle":
				gaggle = "other"
			case "revoked-repair":
				current.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
			case "changed-source":
				current.Spec.Workbench.Sources[0].Repository.Name = "foreign"
			case "missing-credential":
				current.Spec.InteractiveAccess.Credentials.Repositories = nil
			}
			if err := permissions.Apply([]apiv1.Gaggle{*current}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := recovery.Get(t.Context(), p, gaggle, prior.ID); err == nil {
				t.Fatal("current authority ignored")
			}
		})
	}
}

func TestPRRepairObservationAllowsOnlyExactSameTurnDescendant(t *testing.T) {
	s, f, permissions := repairServiceFixture(t)
	f.lost = true
	prior, err := s.Repair(t.Context(), "one", repairServiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	p := httpapi.Principal{Issuer: s.actor.Issuer, Subject: s.actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}
	recovery := &PRRepairRecoveryService{Queue: s.service.Queue, Permissions: permissions, Client: s.service.Client}
	got, err := recovery.Check(t.Context(), p, s.retained.Name, prior.ID)
	if err != nil || got.State != "observed-applied" {
		t.Fatal(got, err)
	}
	request := repairServiceRequest()
	request.ParentCommandID = prior.ID
	request.ExpectedHeadSHA = strings.Repeat("c", 40)
	f.target.HeadSHA = strings.Repeat("e", 40)
	if _, err = s.Repair(t.Context(), "foreign", request); err == nil {
		t.Fatal("foreign moved head accepted")
	}
	f.target.HeadSHA = request.ExpectedHeadSHA
	f.lost = false
	if got, err = s.Repair(t.Context(), "second", request); err != nil || got.State != "confirmed" || f.effects != 2 {
		t.Fatal(got, err)
	}
}

type blockedRepairObserver struct {
	PRRepairClient
	entered, release chan struct{}
}

func (f blockedRepairObserver) ObservePullRequestRepair(ctx context.Context, _ providers.PullRequestRepair) (providers.PRRepairObservation, error) {
	close(f.entered)
	select {
	case <-ctx.Done():
		return providers.PRRepairObservation{}, ctx.Err()
	case <-f.release:
		return providers.PRRepairObservation{}, errors.New("unverified")
	}
}

func TestPRRepairRecoveryFencesPolicyUntilObservationReceiptJoins(t *testing.T) {
	s, f, permissions := repairServiceFixture(t)
	f.lost = true
	prior, err := s.Repair(t.Context(), "one", repairServiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	s.lease.Close()
	observer := blockedRepairObserver{PRRepairClient: f, entered: make(chan struct{}), release: make(chan struct{})}
	recovery := &PRRepairRecoveryService{Queue: s.service.Queue, Permissions: permissions, Client: func(context.Context, ReadBinding, interactiveaccess.Credential) (PRRepairClient, error) {
		return observer, nil
	}}
	p := httpapi.Principal{Issuer: s.actor.Issuer, Subject: s.actor.Subject, Roles: []httpapi.Role{httpapi.RoleOperate}}
	checked := make(chan error, 1)
	go func() { _, err := recovery.Check(t.Context(), p, s.retained.Name, prior.ID); checked <- err }()
	<-observer.entered
	next := s.retained.DeepCopy()
	next.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	published := make(chan error, 1)
	go func() { published <- permissions.Apply([]apiv1.Gaggle{*next}, nil) }()
	select {
	case err := <-published:
		t.Fatal("policy published before check joined", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(observer.release)
	if err = <-checked; err != nil {
		t.Fatal(err)
	}
	if err = <-published; err != nil {
		t.Fatal(err)
	}
	record, err := s.service.Queue.PRRepairCommandForReview(t.Context(), s.retained.Name, prior.ID)
	if err != nil || record.State != "unknown" || len(record.Observations) != 1 {
		t.Fatal(record, err)
	}
	if _, err = recovery.Check(t.Context(), p, s.retained.Name, prior.ID); err == nil {
		t.Fatal("revoked operator reached provider")
	}
}
