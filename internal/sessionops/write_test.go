package sessionops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/workbench"
)

type nativeWriter struct {
	patch func(context.Context, string, string, workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error)
}

func (w nativeWriter) Patch(c context.Context, b, k string, r workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
	return w.patch(c, b, k, r)
}
func (nativeWriter) Capabilities(context.Context, string) (workbench.BacklogWriteCapabilities, error) {
	return workbench.BacklogWriteCapabilities{Fields: []apiv1.WorkbenchField{"title"}}, nil
}
func (nativeWriter) Command(context.Context, string, string) (workbench.BacklogEditCommand, error) {
	return workbench.BacklogEditCommand{}, errors.New("unused")
}
func editRequest() sessioning.BacklogEditRequest {
	value := "scoped item"
	return sessioning.BacklogEditRequest{SourceBindingID: "backlog", RequestID: "edit-1", BacklogPatchRequest: workbench.BacklogPatchRequest{ID: "42", SourceID: "99", ExpectedRevision: "1", Field: "title", Value: &value}}
}
func commandFor(inv Invocation) workbench.BacklogEditCommand {
	return workbench.BacklogEditCommand{ID: "workbench-" + strings.Repeat("d", 32), Gaggle: inv.Identity.Gaggle, SourceBindingID: "backlog", Actor: workbench.CommandActor{Issuer: inv.Actor.Issuer, Subject: inv.Actor.Subject}, ItemID: "42", SourceID: "99", Field: "title", State: "confirmed", RequestDigest: strings.Repeat("a", 64), OperationDigest: strings.Repeat("b", 64), AcceptedAt: time.Now()}
}
func TestSessionNativeEditNamespacesKeyAndRecordsCustody(t *testing.T) {
	f := fixtureFor(t)
	var keys []string
	f.inv.Writer = nativeWriter{patch: func(_ context.Context, b, k string, r workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
		keys = append(keys, k)
		return commandFor(f.inv), nil
	}}
	access := open(t, f)
	for range 2 {
		result, err := f.bridge.EditBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, editRequest())
		if err != nil || result.ID == "" {
			t.Fatal(result, err)
		}
	}
	if keys[0] != keys[1] || keys[0] == "edit-1" || !strings.HasPrefix(keys[0], "session-native:") {
		t.Fatal(keys)
	}
	inv := f.inv
	lineage := *inv.Identity.Session
	inv.Identity.Session = &lineage
	inv.Attempt++
	if sessionCommandKey(inv, "edit-1") != keys[0] {
		t.Fatal("attempt changed turn key")
	}
	inv.Identity.Session.TurnID = "another-turn"
	if sessionCommandKey(inv, "edit-1") == keys[0] {
		t.Fatal("another turn inherited key")
	}
	note := f.rec.events[1]
	if note.Runner["commandId"] == nil || note.Runner["outcome"] != "confirmed" || len(note.Artifacts) != 1 || note.Runner["humanSubject"] != f.inv.Actor.Subject {
		t.Fatal(note)
	}
}
func TestSessionNativeEditRetainsReceiptAfterCancellation(t *testing.T) {
	f := fixtureFor(t)
	f.inv.Writer = nativeWriter{patch: func(_ context.Context, b, k string, r workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
		f.cancel()
		result := commandFor(f.inv)
		result.State = "unknown"
		return result, nil
	}}
	access := open(t, f)
	if _, err := f.bridge.EditBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, editRequest()); err == nil {
		t.Fatal("revoked caller received receipt")
	}
	if len(f.rec.data) != 1 || f.rec.events[1].Runner["outcome"] != "unknown" {
		t.Fatal("lost effect custody", f.rec.events)
	}
	var command workbench.BacklogEditCommand
	if err := json.Unmarshal(f.rec.data[0], &command); err != nil || command.State != "unknown" {
		t.Fatal(command, err)
	}
}
func TestSessionNativeEditAuthorityAndAuditFailBeforeEffect(t *testing.T) {
	for _, mode := range []string{"no-writer", "audit", "budget", "foreign-receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := fixtureFor(t)
			calls := 0
			f.inv.Writer = nativeWriter{patch: func(_ context.Context, b, k string, r workbench.BacklogPatchRequest) (workbench.BacklogEditCommand, error) {
				calls++
				result := commandFor(f.inv)
				if mode == "foreign-receipt" {
					result.Actor.Subject = "other"
				}
				return result, nil
			}}
			if mode == "no-writer" {
				f.inv.Writer = nil
			}
			if mode == "audit" {
				f.rec.fail = true
			}
			access := open(t, f)
			if mode == "budget" {
				g, _ := f.bridge.lookup(access.BearerToken)
				g.bytes = sessioning.MaxOperationTurnBytes - 1
			}
			if _, err := f.bridge.EditBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, editRequest()); err == nil {
				t.Fatal("unsafe result")
			}
			if mode != "foreign-receipt" && calls != 0 {
				t.Fatal("effect before admission")
			}
			if len(f.rec.data) > 0 {
				t.Fatal("unauthorized receipt persisted")
			}
		})
	}
}
func TestSessionWriteOnlyGrantDoesNotEnableReads(t *testing.T) {
	f := fixtureFor(t)
	f.inv.Reader = nil
	f.inv.Writer = nativeWriter{}
	f.inv.WriteBindings = []string{"backlog"}
	access := open(t, f)
	if !access.BacklogReadDisabled || len(access.BacklogWriteSources) != 1 {
		t.Fatal(access)
	}
	if _, err := f.bridge.GetBacklogItem(t.Context(), access.BearerToken, f.inv.Identity.RunID, readRequest()); err == nil {
		t.Fatal("writer granted reader")
	}
}
