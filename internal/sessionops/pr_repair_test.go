package sessionops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/sessioning"
)

type repairToolFixture struct {
	target sessioning.PRRepairTarget
	repair func(context.Context, string, sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error)
}

func (f repairToolFixture) Target() sessioning.PRRepairTarget { return f.target }
func (f repairToolFixture) Inspect(context.Context, string) (sessioning.PRRepairInspection, error) {
	return sessioning.PRRepairInspection{Target: f.target, HeadSHA: f.target.ExpectedHeadSHA}, nil
}
func (f repairToolFixture) ReadFile(context.Context, string, string) (sessioning.PRRepairFile, error) {
	return sessioning.PRRepairFile{}, errors.New("unused")
}
func (f repairToolFixture) Repair(ctx context.Context, key string, request sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error) {
	return f.repair(ctx, key, request)
}
func (f repairToolFixture) Command(context.Context, string) (sessioning.PRRepairCommandView, error) {
	return sessioning.PRRepairCommandView{}, errors.New("unused")
}
func repairToolTarget() sessioning.PRRepairTarget {
	return sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "acme", Name: "code"}, RepositorySourceID: "77", ID: "42", SourceID: "99", ExpectedHeadSHA: strings.Repeat("a", 40)}
}
func repairToolRequest() sessioning.PRRepairRequest {
	value := "fixed"
	return sessioning.PRRepairRequest{RequestID: "one", ExpectedHeadSHA: strings.Repeat("a", 40), Rationale: "Fix the selected PR", Changes: []sessioning.PRRepairChange{{Path: "file.txt", Content: &value}}}
}
func TestPRRepairBridgeRecordsActualReceiptAndHumanAfterRevocation(t *testing.T) {
	f := fixtureFor(t)
	f.inv.Reader = nil
	var key string
	f.inv.Repairer = repairToolFixture{target: repairToolTarget(), repair: func(_ context.Context, k string, _ sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error) {
		key = k
		f.cancel()
		return sessioning.PRRepairCommandView{ID: "repair-" + strings.Repeat("c", 32), SourceBindingID: "code", State: "unknown", RequestDigest: strings.Repeat("d", 64), OperationDigest: strings.Repeat("e", 64), SelectedHeadSHA: repairToolTarget().ExpectedHeadSHA, ExpectedHeadSHA: repairToolTarget().ExpectedHeadSHA, RunID: f.inv.Identity.RunID, Actor: f.inv.Actor, AcceptedAt: time.Now()}, nil
	}}
	access := open(t, f)
	if names := mcpio.SessionOperationToolNames(access, f.inv.Identity.RunID); len(names) != 4 {
		t.Fatal(names)
	}
	if _, err := f.bridge.RepairSelectedPR(t.Context(), access.BearerToken, f.inv.Identity.RunID, repairToolRequest()); err == nil {
		t.Fatal("revoked result returned")
	}
	if key != sessionCommandKey(f.inv, "one") || len(f.rec.data) != 1 || f.rec.events[1].Runner["outcome"] != "unknown" || f.rec.events[1].Runner["humanSubject"] != f.inv.Actor.Subject || f.rec.events[1].Runner["sourceBindingId"] != "code" {
		t.Fatal(key, f.rec.events)
	}
}
func TestPRRepairBridgeRefusesUnownedAdapterAuditAndForeignReceipt(t *testing.T) {
	for _, mode := range []string{"missing", "audit", "foreign", "run"} {
		t.Run(mode, func(t *testing.T) {
			f := fixtureFor(t)
			calls := 0
			f.inv.Repairer = repairToolFixture{target: repairToolTarget(), repair: func(context.Context, string, sessioning.PRRepairRequest) (sessioning.PRRepairCommandView, error) {
				calls++
				return sessioning.PRRepairCommandView{ID: "repair-" + strings.Repeat("c", 32), SourceBindingID: "code", Actor: sessioning.Actor{Issuer: f.inv.Actor.Issuer, Subject: "foreign"}, RunID: f.inv.Identity.RunID, SelectedHeadSHA: repairToolTarget().ExpectedHeadSHA}, nil
			}}
			if mode == "missing" {
				f.inv.Repairer = nil
			}
			if mode == "audit" {
				f.rec.fail = true
			}
			access := open(t, f)
			run := f.inv.Identity.RunID
			if mode == "run" {
				run = "foreign"
			}
			if _, err := f.bridge.RepairSelectedPR(t.Context(), access.BearerToken, run, repairToolRequest()); err == nil {
				t.Fatal("unsafe repair")
			}
			if mode != "foreign" && calls != 0 {
				t.Fatal("effect before authority")
			}
			if len(f.rec.data) > 0 {
				t.Fatal("foreign receipt retained")
			}
		})
	}
}
