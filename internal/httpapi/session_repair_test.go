package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
)

func TestSharedSessionRepairSelectionForwardsExactIntentAndRejectsAmbiguity(t *testing.T) {
	s := &sessionStub{}
	h := sessionTestHandler(t, s, &fakeAuthenticator{principal: &Principal{Issuer: "https://issuer", Subject: "alice", Roles: []Role{RoleOperate}}})
	target := &sessioning.PRRepairTarget{SourceBindingID: "code", Repository: sessioning.RepairRepository{Provider: "github", Owner: "org", Name: "repo"}, RepositorySourceID: "100", ID: "12", SourceID: "900", ExpectedHeadSHA: strings.Repeat("a", 40)}
	raw, _ := json.Marshal(apicontract.SessionMessageRequest{Text: "Repair", RepairTarget: target})
	out := sessionRequest(h, http.MethodPost, "/session-one/messages", string(raw), "same-key", "")
	if out.Code != 202 || s.repairTarget == nil || *s.repairTarget != *target || s.key != "same-key" || s.principal.Subject != "alice" {
		t.Fatal(out.Code, out.Body, s)
	}
	for _, body := range []string{
		strings.Replace(string(raw), `"sourceId":"900"`, `"sourceId":"900","sourceId":"901"`, 1),
		strings.Replace(string(raw), `"owner":"org"`, `"owner":"org","credentialRef":"automation"`, 1),
		strings.Replace(string(raw), `"owner":"org"`, `"Owner":"org"`, 1),
		strings.Replace(string(raw), `"expectedHeadSha"`, `"ExpectedHeadSha"`, 1),
		`{"text":"Repair","repairTarget":null}`, `{"text":"Repair","repairTarget":{}}`,
	} {
		before := s.calls
		out = sessionRequest(h, http.MethodPost, "/session-one/messages", body, "same-key", "")
		if out.Code != 400 || s.calls != before {
			t.Fatal("ambiguous intent reached runtime", out.Code, body)
		}
	}
}
