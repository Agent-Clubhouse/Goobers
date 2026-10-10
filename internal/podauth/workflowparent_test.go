package podauth

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestWorkflowParentTokenCannotBorrowOrdinaryOrChildCustody(t *testing.T) {
	key, err := NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := key.MintWorkflowParentPod("child-run", "sha256:"+strings.Repeat("a", 64), time.Hour)
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
	if err != nil || p.WorkflowParent == nil || p.WorkflowParent.ContractDigest != "sha256:"+strings.Repeat("a", 64) || p.Subject != "run:child-run" || httpapi.IsPodPrincipal(*p) || p.Issuer != httpapi.WorkflowParentPrincipalIssuer {
		t.Fatal("signed child identity lost", p, err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Replace(token, workflowParentPodPrefix, tokenPrefix, 1))
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("child marker removed without invalidating signature")
	}
	for _, prefix := range []string{childPodTokenPrefix, ChildWorkflowGrantPrefix} {
		request.Header.Set("Authorization", "Bearer "+strings.Replace(token, workflowParentPodPrefix, prefix, 1))
		if _, err := auth.Authenticate(request); err == nil {
			t.Fatal("parent bearer crossed signing domain", prefix)
		}
	}
	ordinary, err := key.Mint("child-run", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+strings.Replace(ordinary, tokenPrefix, workflowParentPodPrefix, 1))
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("ordinary bearer relabeled as child")
	}
	original := base64.RawURLEncoding.EncodeToString([]byte("sha256:" + strings.Repeat("a", 64)))
	replacement := base64.RawURLEncoding.EncodeToString([]byte("sha256:" + strings.Repeat("b", 64)))
	request.Header.Set("Authorization", "Bearer "+strings.Replace(token, original, replacement, 1))
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("child contract changed without invalidating signature")
	}
	key.WithClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	request.Header.Set("Authorization", "Bearer "+token)
	if _, err := auth.Authenticate(request); err == nil {
		t.Fatal("expired child bearer authenticated")
	}
}
