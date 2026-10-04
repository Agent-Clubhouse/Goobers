package startcontrol

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type cancellationAuthority struct {
	Version                                          int `json:"version"`
	Gaggle, AcceptanceID, RequestID, Issuer, Subject string
	Roles                                            []httpapi.Role
	Groups                                           []string
}

func cancellationCommand(p httpapi.Principal, c triggerqueue.StartControl, in apicontract.StartQueueCancelInput) (triggerqueue.StartCancellation, error) {
	if c.Cancellation != nil {
		a, err := readCancellationAuthority(c)
		if err != nil {
			return triggerqueue.StartCancellation{}, err
		}
		if a.Issuer != p.Issuer || a.Subject != p.Subject || c.Cancellation.RequestID != in.RequestID || c.Cancellation.Reason != in.Reason {
			return triggerqueue.StartCancellation{}, triggerqueue.ErrConflict
		}
		return *c.Cancellation, nil
	}
	a := cancellationAuthority{Version: 1, Gaggle: c.Scope.Gaggle, AcceptanceID: c.Record.ID, RequestID: in.RequestID, Issuer: p.Issuer, Subject: p.Subject, Roles: slices.Clone(p.Roles), Groups: slices.Clone(p.Groups)}
	slices.Sort(a.Roles)
	a.Roles = slices.Compact(a.Roles)
	slices.Sort(a.Groups)
	a.Groups = slices.Compact(a.Groups)
	raw, err := json.Marshal(a)
	if err != nil || len(raw) > 8192 || !a.valid() {
		return triggerqueue.StartCancellation{}, triggerqueue.ErrTransition
	}
	identity, _ := json.Marshal([]string{p.Issuer, p.Subject})
	actor := p.Issuer + ":" + p.Subject
	if len(actor) > 1024 {
		actor = fmt.Sprintf("human:%x", sha256.Sum256(identity))
	}
	return triggerqueue.StartCancellation{RequestID: in.RequestID, Actor: actor, Reason: in.Reason, Authority: raw}, nil
}
func (a cancellationAuthority) valid() bool {
	if a.Version != 1 || !commandText(a.Issuer, 2048) || !commandText(a.Subject, 512) || len(a.Roles) > 3 || len(a.Groups) > 128 || !slices.IsSorted(a.Roles) || !slices.IsSorted(a.Groups) {
		return false
	}
	for _, role := range a.Roles {
		if role != httpapi.RoleView && role != httpapi.RoleOperate && role != httpapi.RoleAdmin {
			return false
		}
	}
	for _, group := range a.Groups {
		if !commandText(group, 512) {
			return false
		}
	}
	return (httpapi.Principal{Roles: a.Roles}).HasRole(httpapi.RoleOperate)
}
func readCancellationAuthority(c triggerqueue.StartControl) (cancellationAuthority, error) {
	var a cancellationAuthority
	if c.Cancellation == nil || len(c.Cancellation.Authority) > 8192 {
		return a, triggerqueue.ErrConflict
	}
	if err := json.Unmarshal(c.Cancellation.Authority, &a); err != nil || !a.valid() || a.Gaggle != c.Scope.Gaggle || a.AcceptanceID != c.Record.ID || a.RequestID != c.Cancellation.RequestID {
		return a, triggerqueue.ErrConflict
	}
	return a, nil
}
