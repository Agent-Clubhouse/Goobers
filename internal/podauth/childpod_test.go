package podauth

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestChildPodTokenCannotBeDowngradedToOrdinaryCustody(t *testing.T) {
	key, err := NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintChildPod("child-run", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	p, err := auth.Authenticate(request)
	if err != nil || !p.GeneratedChild || p.Subject != "run:child-run" || !httpapi.IsPodPrincipal(*p) {
		t.Fatal("signed child identity lost", p, err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Replace(token, childPodTokenPrefix, tokenPrefix, 1))
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("child marker removed without invalidating signature")
	}
	ordinary, err := key.Mint("child-run", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Replace(ordinary, tokenPrefix, childPodTokenPrefix, 1))
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("ordinary bearer relabeled as child")
	}
	key.WithClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	request.Header.Set("Authorization", "Bearer "+token)
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("expired child bearer authenticated")
	}
}

func TestWorkflowParentPodHasIndependentSignedCustody(t *testing.T) {
	key, err := NewSignedKey([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintWorkflowParentPod("parent-run", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	p, err := auth.Authenticate(request)
	if err != nil || !p.WorkflowParent || p.GeneratedChild || p.Subject != "run:parent-run" {
		t.Fatal(p, err)
	}
	for _, prefix := range []string{tokenPrefix, childPodTokenPrefix} {
		request.Header.Set("Authorization", "Bearer "+strings.Replace(token, workflowParentPodPrefix, prefix, 1))
		if _, err := auth.Authenticate(request); err == nil {
			t.Fatal("parent custody domain relabeled")
		}
	}
}
