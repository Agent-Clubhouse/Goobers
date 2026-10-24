package podauth

import (
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

func TestWorkerTerminalInterventionCredentialIsSeparateFromEveryOtherIdentity(t *testing.T) {
	now := time.Now()
	signer, err := NewSignedKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	signer.WithClock(func() time.Time { return now })
	fallback := &stubAuthenticator{principal: &httpapi.Principal{Subject: "human", Roles: []httpapi.Role{httpapi.RoleAdmin}}}
	auth, err := NewAuthenticator(signer, fallback)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.MintWorkerTerminalIntervention("engine-worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.Authenticate(requestWithBearer(token))
	if err != nil || principal == nil || principal.Subject != "worker:engine-worker-a" || principal.Issuer != httpapi.WorkerTerminalInterventionPrincipalIssuer || len(principal.Roles) != 0 || httpapi.IsPodPrincipal(*principal) {
		t.Fatalf("wrong worker identity: %+v, %v", principal, err)
	}
	pod, _ := signer.Mint("real-run", time.Minute)
	surrender, _ := signer.MintWorkerSurrender("engine-worker-a", time.Minute)
	blob, _ := signer.MintWorkerBlob("engine-worker-a", time.Minute)
	child, _ := signer.MintWorkerChildAdmission("engine-worker-a", time.Minute)
	config, _ := signer.MintWorkerConfigDigest("engine-worker-a", time.Minute)
	body := strings.TrimPrefix(token, workerTerminalTokenPrefix)
	for _, forged := range []string{
		workerChildTokenPrefix + body, workerTerminalTokenPrefix + strings.TrimPrefix(child, workerChildTokenPrefix),
		tokenPrefix + body, workerSurrenderTokenPrefix + body, workerBlobTokenPrefix + body, workerTokenPrefix + body,
		workerTerminalTokenPrefix + strings.TrimPrefix(pod, tokenPrefix),
		workerTerminalTokenPrefix + strings.TrimPrefix(surrender, workerSurrenderTokenPrefix),
		workerTerminalTokenPrefix + strings.TrimPrefix(blob, workerBlobTokenPrefix),
		workerTerminalTokenPrefix + strings.TrimPrefix(config, workerTokenPrefix),
		tokenPrefix + token, token + "x", workerTerminalTokenPrefix + "malformed",
	} {
		if p, err := auth.Authenticate(requestWithBearer(forged)); err == nil {
			t.Fatalf("forged identity accepted as %+v", p)
		}
	}
	for _, ttl := range []time.Duration{0, MaxWorkerTokenTTL + time.Second} {
		if _, err := signer.MintWorkerTerminalIntervention("engine-worker-a", ttl); err == nil {
			t.Fatal("invalid lifetime minted")
		}
	}
	now = now.Add(time.Minute)
	if _, err := auth.Authenticate(requestWithBearer(token)); err == nil {
		t.Fatal("expired worker accepted")
	}
	if fallback.called {
		t.Fatal("worker credential delegated to human authentication")
	}
}
