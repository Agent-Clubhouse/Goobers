package interactivesession

import (
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/sessioning"
)

// authority is host-created from the verified request principal. The durable
// snapshot supports asynchronous policy rechecks, not browser-supplied claims.
type authority struct {
	Version int              `json:"version"`
	Actor   sessioning.Actor `json:"actor"`
	Roles   []httpapi.Role   `json:"roles"`
	Groups  []string         `json:"groups,omitempty"`
}

func marshalAuthority(p httpapi.Principal) ([]byte, error) {
	a := authority{Version: 1, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}, Roles: append([]httpapi.Role(nil), p.Roles...), Groups: append([]string(nil), p.Groups...)}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 {
		return nil, sessioning.ErrInvalidRequest
	}
	return raw, nil
}

func turnPrincipal(raw []byte, actor *sessioning.Actor) (httpapi.Principal, error) {
	var a authority
	if len(raw) > 16384 || json.Unmarshal(raw, &a) != nil || a.Version != 1 || actor == nil || a.Actor != *actor {
		return httpapi.Principal{}, errors.New("session turn human authority mismatch")
	}
	return httpapi.Principal{Issuer: a.Actor.Issuer, Subject: a.Actor.Subject, Roles: a.Roles, Groups: a.Groups}, nil
}
