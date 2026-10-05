package interactivesession

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/sessioning"
)

func TestSessionRepairSelectionExactRetryAndHumanAttribution(t *testing.T) {
	s, _, gaggle := serviceFixture(t)
	created, err := s.Create(t.Context(), sessionPrincipal("alice"), gaggle.Name, sessioning.CreateRequest{RequestID: "create", Title: "Repair", Goober: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	target := &sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "org", Name: "repo"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}
	req := sessioning.MessageRequest{RequestID: "selection", Text: "Repair the selected PR", RepairTarget: target}
	first, err := s.SubmitMessage(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, req)
	if err != nil || first.Message.Actor.Subject != "alice" || *first.Message.RepairTarget != *target {
		t.Fatal(first, err)
	}
	replay, err := s.SubmitMessage(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, req)
	if err != nil || !replay.Duplicate || replay.AcceptanceID != first.AcceptanceID {
		t.Fatal(replay, err)
	}
	target.ExpectedHeadSHA = strings.Repeat("b", 40)
	if _, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, req); !errors.Is(err, sessioning.ErrConflict) {
		t.Fatal("changed head reused request key", err)
	}
	if first.Message.RepairTarget.ExpectedHeadSHA != strings.Repeat("a", 40) {
		t.Fatal("caller changed retained response")
	}
	req.RequestID = "invalid"
	target.Repository.Owner = "https://foreign"
	if _, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, req); !errors.Is(err, sessioning.ErrInvalidRequest) {
		t.Fatal("invalid selection admitted", err)
	}
}

func TestSessionRepairSelectionNeverPersistsOrReexposesRegisteredSecret(t *testing.T) {
	s, reg, gaggle := serviceFixture(t)
	created, err := s.Create(t.Context(), sessionPrincipal("alice"), gaggle.Name, sessioning.CreateRequest{RequestID: "create", Title: "Repair", Goober: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	target := &sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "org", Name: "secret-canary"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}
	req := sessioning.MessageRequest{RequestID: "selection", Text: "Repair", RepairTarget: target}
	if _, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, req); err != nil {
		t.Fatal(err)
	}
	reg.Register([]byte("secret-canary"))
	page, err := s.Messages(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, 0, 50)
	if err != nil || len(page.Items) != 1 || page.Items[0].RepairTarget != nil {
		t.Fatal("late registered secret exposed", page, err)
	}
	req.RequestID = "new-secret"
	if _, err = s.SubmitMessage(t.Context(), sessionPrincipal("alice"), gaggle.Name, created.Session.ID, req); !errors.Is(err, sessioning.ErrInvalidRequest) {
		t.Fatal("secret selection persisted", err)
	}
}
