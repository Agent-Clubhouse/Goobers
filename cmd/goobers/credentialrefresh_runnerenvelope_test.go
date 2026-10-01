package main

import (
	"context"
	"errors"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// credentialrefresh_runnerenvelope_test.go pins Goobers#6193: the local
// runner's envelope TaskID is the run-scoped "<runId>:<stage>" instance id
// (internal/runner/run.go, internal/engine/engine.go), and a refresh grant
// minted from it must still name a stage the run's PINNED definition knows.
// Before the fix every local refresh came back 404 stage_unknown: the live
// soak's ADO queue-watch died on its first rejected bearer.

// runnerStageEnvelope is the envelope exactly as the local runner shapes it
// for stage of runID.
func runnerStageEnvelope(runID, stage string, attempt int32) apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{RunID: runID, TaskID: runID + ":" + stage, Attempt: attempt}
}

type grantTestRecorder struct{}

func (grantTestRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	return journal.Ref{Path: name, Digest: journal.Digest(data), Size: int64(len(data))}, nil
}

// heldRevokeMinter passes every mint through to the daemon's real minter and
// records what the executor asked for and received, but holds the revoke the
// executor fires when the attempt returns. The test can then play the stage
// child's mid-stage refresh against the exact grant the executor delivered,
// with no cross-process timing, and release the revoke afterwards.
type heldRevokeMinter struct {
	inner executor.StageCredentialGrants

	mu             sync.Mutex
	envs           []apiv1.InvocationEnvelope
	grants         []executor.StageCredentialGrant
	executorRevoke int
}

func (m *heldRevokeMinter) MintStageGrant(env apiv1.InvocationEnvelope, capabilities []string, ttl time.Duration) (executor.StageCredentialGrant, error) {
	grant, err := m.inner.MintStageGrant(env, capabilities, ttl)
	if err != nil {
		return grant, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envs = append(m.envs, env)
	m.grants = append(m.grants, grant)
	held := grant
	held.Revoke = func() {
		m.mu.Lock()
		m.executorRevoke++
		m.mu.Unlock()
	}
	return held, nil
}

// TestLocalRunnerStageRefreshesThroughThePinnedDefinition is the round trip
// the live failure took: the real ShellExecutor runs a goobers-CLI stage
// under a runner-shaped envelope and mints its grant through the daemon's
// credential service; the stage child's ADO delivered source is then
// rejected and re-resolves with that grant over the loopback refresh route,
// which verifies the grant's stage against the run's pinned definition and
// mints a fresh value.
func TestLocalRunnerStageRefreshesThroughThePinnedDefinition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("drives a POSIX shell stub as the goobers-CLI stage child")
	}
	const delivered = "ado-delivered-bearer-value-0123456789"
	resetStageRefreshingTokens(t)
	api := &switchingHandler{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	service, minted, runID := newRefreshFixture(t, server.URL)
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.AllowAll, stdlog.New(io.Discard, "", 0), httpapi.WithCredentialService(service))
	if err != nil {
		t.Fatal(err)
	}
	api.set(handler)

	// The executor's injector delivers repo:push with a stated expiry, so the
	// executor asks the daemon for a grant.
	deliveredExpiry := time.Now().Add(40 * time.Minute).UTC().Truncate(time.Second)
	resolver, err := credentials.NewResolverWithExpiring(nil, nil, nil, map[string]credentials.ExpiringResolveFunc{
		"app": func(context.Context) (string, time.Time, error) { return delivered, deliveredExpiry, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: "repo:push", Ref: "app"}}, service.shared)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := executor.NewShellExecutor(injector, grantTestRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	minter := &heldRevokeMinter{inner: service}
	shell.CredentialGrants = minter
	shell.DefaultTimeout = time.Minute
	stub := filepath.Join(t.TempDir(), "goobers")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shell.SelfBin = stub

	env := runnerStageEnvelope(runID, "push-branch", 1)
	env.Workspace = t.TempDir()
	env.Capabilities = []string{"repo:push"}
	result, err := shell.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"goobers", "push-branch"}})
	if err != nil || result.Status != apiv1.ResultSuccess {
		t.Fatalf("stage = %+v, %v", result, err)
	}
	if len(minter.grants) != 1 || minter.executorRevoke != 1 {
		t.Fatalf("executor minted %d grants and revoked %d; want one grant, revoked when the attempt returned", len(minter.grants), minter.executorRevoke)
	}
	if minter.envs[0].TaskID != runID+":push-branch" {
		t.Fatalf("executor minted from TaskID %q; the fixture no longer plays the runner's run-scoped shape", minter.envs[0].TaskID)
	}
	grant := minter.grants[0]
	if grant.Endpoint != server.URL || grant.Token == "" {
		t.Fatalf("grant = endpoint %q token set %v; want the daemon endpoint and a token", grant.Endpoint, grant.Token != "")
	}

	// The stage child, as the executor equipped it: its delivered bearer is
	// rejected, so it re-resolves through the grant.
	t.Setenv(executor.CredentialEnvVar("repo:push"), delivered)
	t.Setenv(capability.CredentialExpiryEnvVar("repo:push"), capability.FormatCredentialExpiry(deliveredExpiry))
	t.Setenv(executor.CredentialEndpointEnvVar, grant.Endpoint)
	t.Setenv(executor.CredentialGrantEnvVar, grant.Token)
	t.Setenv(executor.RepoAuthSchemeEnvVar, "bearer")
	source, err := stageADOCredentialSource(capability.RepoPush, delivered)
	if err != nil {
		t.Fatal(err)
	}
	refreshable, ok := source.(interface{ Invalidate() })
	if !ok {
		t.Fatalf("source %T is not refreshable", source)
	}
	refreshable.Invalidate()
	credential, err := source.Credential(context.Background())
	if err != nil {
		t.Fatalf("mid-stage re-resolve of a runner-shaped stage grant: %v", err)
	}
	if credential.Secret != minted.value(1) || credential.Kind != providers.ADOCredentialKindBearer || minted.calls != 1 {
		t.Fatalf("credential after re-resolve = %+v (minted %d); want the daemon's one fresh mint", credential, minted.calls)
	}
	found := false
	for _, event := range instanceLogEvents(t, service.layout) {
		if event.Runner["kind"] == credentialRefreshMarker {
			found = true
			if event.Stage != "push-branch" {
				t.Fatalf("refresh audit stage = %q, want the pinned stage name push-branch", event.Stage)
			}
		}
	}
	if !found {
		t.Fatal("the re-resolve was not journaled")
	}

	// Releasing the held revoke ends the grant, as the attempt's return does.
	grant.Revoke()
	_, err = service.Refresh(context.Background(), grant.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"})
	if err == nil || planeErrorOf(t, err).Code != "credential_grant_revoked" {
		t.Fatalf("refresh after revoke = %v, want credential_grant_revoked", err)
	}
}

// TestRunnerShapedGrantStillRefusesAStageOutsideThePin: stripping the run's
// own prefix is the whole of the fix. A runner-shaped TaskID naming a stage
// the pinned definition lacks, and a TaskID carrying ANOTHER run's prefix,
// are both still refused 404 stage_unknown, and nothing is minted.
func TestRunnerShapedGrantStillRefusesAStageOutsideThePin(t *testing.T) {
	for name, envelope := range map[string]func(runID string) apiv1.InvocationEnvelope{
		"stage not in the pinned definition": func(runID string) apiv1.InvocationEnvelope {
			return runnerStageEnvelope(runID, "queue-watch", 1)
		},
		"another run's prefix": func(runID string) apiv1.InvocationEnvelope {
			return apiv1.InvocationEnvelope{RunID: runID, TaskID: "run-someone-else:push-branch", Attempt: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, minted, runID := newRefreshFixture(t, "http://127.0.0.1:1")
			grant, err := service.MintStageGrant(envelope(runID), []string{"repo:push"}, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Refresh(context.Background(), grant.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"})
			var planeErr *httpapi.InterventionError
			if !errors.As(err, &planeErr) || planeErr.Status != http.StatusNotFound || planeErr.Code != "stage_unknown" {
				t.Fatalf("Refresh = %v, want 404 stage_unknown", err)
			}
			if minted.calls != 0 {
				t.Fatalf("a refused refresh minted %d values", minted.calls)
			}
		})
	}
}
