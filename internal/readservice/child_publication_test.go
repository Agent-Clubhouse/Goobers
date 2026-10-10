package readservice

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestChildPublicationProjectionRefusesUnrelatedOrMalformedCustody(t *testing.T) {
	child := triggerqueue.ChildRecord{RunID: "child"}
	for _, scenario := range []string{"other run", "duplicate", "unbounded", "unsafe URL", "URL credentials", "uncertain link", "unknown state", "false confirmation", "oversize", "source error"} {
		t.Run(scenario, func(t *testing.T) {
			status := ChildPublicationObservation{SourceRunID: child.RunID, Item: ChildPublicationItem{Action: "pr", State: "confirmed", Head: "child-branch", Base: "main", Commit: strings.Repeat("a", 40), PullRequestURL: "https://github.com/owner/repo/pull/7", PullRequestNumber: 7}}
			var sourceErr error
			switch scenario {
			case "other run":
				status.SourceRunID = "other"
			case "unsafe URL":
				status.Item.PullRequestURL = "javascript:alert(1)"
			case "URL credentials":
				status.Item.PullRequestURL = "https://secret@example.com/pull/7"
			case "uncertain link":
				status.Item.State, status.Item.NeedsHuman = "effect_pending", true
			case "unknown state":
				status.Item.State = "unknown"
			case "false confirmation":
				status.Item.NeedsHuman = true
			case "oversize":
				status.Item.Head = strings.Repeat("x", 1025)
			case "source error":
				sourceErr = errors.New("private storage diagnostic")
			}
			statuses := []ChildPublicationObservation{status}
			if scenario == "duplicate" {
				statuses = append(statuses, status)
			}
			if scenario == "unbounded" {
				statuses = append(statuses, status, status)
			}
			service := &Local{sources: LocalSources{ChildPublications: func(context.Context, triggerqueue.ChildIdentity) ([]ChildPublicationObservation, error) {
				return statuses, sourceErr
			}}}
			result, err := service.childPublications(t.Context(), child)
			if err != nil || result.Status != "unavailable" || len(result.Items) != 0 {
				t.Fatal("untrusted publication became visible", result, err)
			}
		})
	}
}

func TestChildPublicationProjectionDistinguishesAbsenceUncertaintyAndExpiry(t *testing.T) {
	child := triggerqueue.ChildRecord{RunID: "child", Identity: triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "scope", ParentRunID: "parent"}, StageOccurrence: "stage", InvocationKey: "key"}}
	service := &Local{}
	result, err := service.childPublications(t.Context(), child)
	if err != nil || result.Status != "unavailable" {
		t.Fatal(result, err)
	}
	var statuses []ChildPublicationObservation
	calls := 0
	service.sources.ChildPublications = func(_ context.Context, identity triggerqueue.ChildIdentity) ([]ChildPublicationObservation, error) {
		if identity != child.Identity {
			t.Fatal("publication lookup changed its child scope")
		}
		calls++
		return statuses, nil
	}
	result, err = service.childPublications(t.Context(), child)
	if err != nil || result.Status != "recorded" || len(result.Items) != 0 {
		t.Fatal("empty custody became unavailable", result, err)
	}
	statuses = []ChildPublicationObservation{{SourceRunID: "child", Item: ChildPublicationItem{Action: "branch", State: "confirmed", Head: "child-branch", Base: "main", Commit: strings.Repeat("a", 40)}}, {SourceRunID: "child", Item: ChildPublicationItem{Action: "pr", State: "effect_pending", NeedsHuman: true}}}
	result, err = service.childPublications(t.Context(), child)
	if err != nil || result.Status != "recorded" || len(result.Items) != 2 || !result.Items[1].NeedsHuman || result.Items[1].PullRequestURL != "" {
		t.Fatal("uncertain effect lost", result, err)
	}
	statuses[1].Item.State, statuses[1].Item.NeedsHuman = "confirmed", false
	statuses[1].Item.PullRequestURL, statuses[1].Item.PullRequestNumber = "https://github.com/owner/repo/pull/7", 7
	result, err = service.childPublications(t.Context(), child)
	if err != nil || result.Status != "recorded" || result.Items[1].PullRequestNumber != 7 {
		t.Fatal("confirmed PR link lost", result, err)
	}
	child.TombstonedAt = time.Now()
	result, err = service.childPublications(t.Context(), child)
	if err != nil || result.Status != "expired" || calls != 3 {
		t.Fatal("expired child queried publication custody", result, calls, err)
	}
	child.TombstonedAt = time.Time{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.childPublications(ctx, child); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled read succeeded", err)
	}
}
