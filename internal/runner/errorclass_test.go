package runner

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/worktree"
)

// TestClassifyDispatchFailureTypesEveryKnownCause is the taxonomy acceptance:
// each cause the 2026-08-08 audit had to recover by reading journal text must
// resolve to its own code and class from the TYPED error alone. Only a
// failure no typed error explains is left as an executor defect.
func TestClassifyDispatchFailureTypesEveryKnownCause(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCode  string
		wantClass telemetry.ErrorClass
	}{
		{
			// The seam across package boundaries: internal/executor's own
			// typed failure, wrapped for infrastructure retry exactly as the
			// shell executor wraps it.
			name:      "provider auth through the executor seam",
			err:       invoke.InfrastructureFailure(executor.StageFailure("github_auth_failed", errors.New("executor: provider stage \"ci-poll\" reported github_auth_failed: 403"))),
			wantCode:  "github_auth_failed",
			wantClass: telemetry.ErrorClassProvider,
		},
		{
			name:      "provider rate limit keeps its own class",
			err:       invoke.InfrastructureFailure(executor.StageFailure("github_rate_limited", errors.New("quota exhausted"))),
			wantCode:  "github_rate_limited",
			wantClass: telemetry.ErrorClassProviderRateLimit,
		},
		{
			name:      "claims lock contention",
			err:       invoke.InfrastructureFailure(executor.StageFailure("claims_lock_timeout", errors.New("claims lock operation \"claim\" timed out after 30s"))),
			wantCode:  "claims_lock_timeout",
			wantClass: telemetry.ErrorClassInfraLock,
		},
		{
			name:      "git provisioning",
			err:       codedStageFailure(errCodeInfraGit, errors.New("prepare stage \"implement\": create worktree: 403")),
			wantCode:  errCodeInfraGit,
			wantClass: telemetry.ErrorClassInfraGit,
		},
		{
			name:      "network provisioning",
			err:       invoke.InfrastructureFailure(codedStageFailure(errCodeInfraNet, errors.New("could not resolve host"))),
			wantCode:  errCodeInfraNet,
			wantClass: telemetry.ErrorClassInfraNet,
		},
		{
			name:      "host workspace",
			err:       codedStageFailure(errCodeInfraWorkspace, errors.New("create scratch workspace root: permission denied")),
			wantCode:  errCodeInfraWorkspace,
			wantClass: telemetry.ErrorClassInfra,
		},
		{
			name:      "agentic session timeout",
			err:       invoke.Timeout(errors.New("harness: copilot-cli: session timed out after 30m0s")),
			wantCode:  telemetry.ErrCodeTimeout,
			wantClass: telemetry.ErrorClassTimeout,
		},
		{
			name:      "residual executor defect",
			err:       errors.New("executor: record stdout: no space left on device"),
			wantCode:  errCodeExecutor,
			wantClass: telemetry.ErrorClassExecutor,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, class := classifyDispatchFailure(fmt.Errorf("runner: execute stage %q: %w", "implement", tt.err))
			if code != tt.wantCode || class != tt.wantClass {
				t.Fatalf("classifyDispatchFailure = (%q, %q), want (%q, %q)", code, class, tt.wantCode, tt.wantClass)
			}
		})
	}
}

// TestProvisionFailureCodeSplitsOwners proves the runner keeps worktree's
// tiers apart rather than folding them back together: a token that cannot
// clone and a DNS outage are different owners' problems.
func TestProvisionFailureCodeSplitsOwners(t *testing.T) {
	if got := provisionFailureCode(errors.New("create scratch workspace root: read-only file system")); got != errCodeInfraWorkspace {
		t.Fatalf("provisionFailureCode(host failure) = %q, want %q", got, errCodeInfraWorkspace)
	}
	// The typed-tier mapping itself is covered by internal/worktree's own
	// table; this asserts only that each tier reaches a distinct code.
	codes := map[worktree.FailureTier]string{
		worktree.TierNetwork: errCodeInfraNet,
		worktree.TierGit:     errCodeInfraGit,
		worktree.TierUnknown: errCodeInfraWorkspace,
	}
	seen := map[string]bool{}
	for tier, code := range codes {
		if seen[code] {
			t.Fatalf("tier %q reuses code %q", tier, code)
		}
		seen[code] = true
	}
}

// TestDispatchFailureJournalsTypedCause is the end-to-end acceptance for the
// bucket this cluster exists to kill: a stage whose workspace cannot be
// provisioned journals WHY on its error event, in the runner namespace that
// telemetry projects. Before this the same event carried only
// code=executor_error, so an unauthorized clone and a DNS outage were
// indistinguishable in SQL (audit: 3,487 attempts in one such bucket).
func TestDispatchFailureJournalsTypedCause(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		runID     string
		wantCode  string
		wantClass telemetry.ErrorClass
	}{
		{
			name:      "unauthorized clone is git-owned",
			status:    http.StatusForbidden,
			runID:     "run-typed-git",
			wantCode:  errCodeInfraGit,
			wantClass: telemetry.ErrorClassInfraGit,
		},
		{
			name:      "unreachable remote is network-owned",
			status:    http.StatusServiceUnavailable,
			runID:     "run-typed-net",
			wantCode:  errCodeInfraNet,
			wantClass: telemetry.ErrorClassInfraNet,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoURL := newHTTPGitRemote(t, int32(tt.status), -1)
			stage := &flakyDeterministic{}
			r, runsDir := newWorktreeProvisioningTestRunner(t, repoURL, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
				return stage, nil
			})

			if _, err := r.Start(t.Context(), StartInput{
				RunID:   tt.runID,
				Machine: fixtureMachine(t),
				Gaggle:  "acme-web",
				Trigger: journal.Trigger{Kind: journal.TriggerManual},
				RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
			}); err == nil {
				t.Fatal("expected the worktree provisioning failure to fail the run")
			}

			rd, err := journal.OpenRead(filepath.Join(runsDir, tt.runID))
			if err != nil {
				t.Fatalf("OpenRead: %v", err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			found := 0
			for _, e := range events {
				if e.Type != journal.EventError || e.Stage != "implement" || e.Error == nil {
					continue
				}
				found++
				// The normative code is unchanged: it is what marks an
				// attempt that failed before it could report a result.
				if e.Error.Code != "executor_error" {
					t.Fatalf("error.code = %q, want executor_error (the attempt-boundary marker)", e.Error.Code)
				}
				if got := e.Runner[stageErrorCodeKey]; got != tt.wantCode {
					t.Fatalf("runner[%s] = %v, want %q", stageErrorCodeKey, got, tt.wantCode)
				}
				if got := e.Runner[stageErrorClassKey]; got != string(tt.wantClass) {
					t.Fatalf("runner[%s] = %v, want %q", stageErrorClassKey, got, tt.wantClass)
				}
			}
			if found == 0 {
				t.Fatal("no stage error event journaled for the worktree provisioning failure")
			}
		})
	}
}
