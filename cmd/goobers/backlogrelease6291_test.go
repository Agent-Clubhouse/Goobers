package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// Exercise the actual CLI and provider transport: blocking the first of 100
// provider releases must not prevent an unrelated lock-taking ledger operation.
func TestBacklogQueryReleaseProviderDoesNotHoldClaimsLock(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= 100; i++ {
		server.addIssue(i, "Claimed item", "goobers:claimed")
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Chdir(t.TempDir())
	raw, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 100; i++ {
		if ok, _, err := raw.Claim(fmt.Sprint(i), "curation-run", "backlog-curation", DefaultClaimLease); !ok || err != nil {
			t.Fatalf("seed %d: %t %v", i, ok, err)
		}
	}
	started, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			close(started)
			<-proceed
		})
		server.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	// Register unblock before waiting for a failed assertion's deferred close.
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	}()
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = proxy.URL })...)
	}
	done := make(chan string, 1)
	go func() {
		code, stdout, stderr := runArgs(t, "backlog-query", "--release", root)
		if code != 0 {
			done <- fmt.Sprintf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
		} else {
			done <- ""
		}
	}()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("release did not use blocked provider: %s", result)
	case <-time.After(10 * time.Second):
		t.Fatal("provider call did not start")
	}
	other, err := fileClaimLedger(layoutFor(root))
	if err != nil {
		t.Fatal(err)
	}
	progress := make(chan error, 1)
	go func() {
		ok, _, err := other.ClaimScoped(context.Background(), claimsclient.Key{ExternalID: "unrelated"}, "other-run", "implementation", time.Minute)
		if err == nil && !ok {
			err = fmt.Errorf("unrelated claim refused")
		}
		progress <- err
	}()
	select {
	case err := <-progress:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider I/O held the claims lock")
	}
	close(proceed)
	if result := <-done; result != "" {
		t.Fatal(result)
	}
}

func TestProviderReleaseSlowLockReportsOneAffectedItem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scheduler")
	path := filepath.Join(dir, claimLockFileName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := withClaimLockThreshold(path, claimsclient.ProviderReleaseReserveOperation, -time.Second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	event := readSingleSlowClaimLockEvent(t, dir, claimsclient.ProviderReleaseReserveOperation)
	if fmt.Sprint(event.Runner["itemCount"]) != "1" {
		t.Fatalf("missing affected-item count: %v", event.Runner)
	}
}
