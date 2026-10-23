package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/childadmissionclient"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
)

func TestWorkerChildAdmissionsMintsOnlyTheChildAdmissionBearer(t *testing.T) {
	if got := workerChildAdmissions("http://control", "worker-0", nil); got != nil {
		t.Fatalf("no signing key produced %#v, want an untyped nil", got)
	}
	signer, err := podauth.NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if got := workerChildAdmissions("", "worker-0", signer); got != nil {
		t.Fatalf("no plane endpoint produced %#v, want an untyped nil", got)
	}
	client, ok := workerChildAdmissions("http://control", "worker-0", signer).(*childadmissionclient.Client)
	if !ok || client.BaseURL != "http://control" {
		t.Fatalf("client = %#v", client)
	}
	token, err := client.TokenSource()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	principal, err := auth.Authenticate(request)
	if err != nil || principal.Issuer != httpapi.WorkerChildAdmissionPrincipalIssuer {
		t.Fatalf("minted bearer authenticated as %+v (%v), want the child-admission issuer", principal, err)
	}
}
