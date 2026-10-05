package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

func TestCounterReadScopePartitionsGaggleGenerationAndRotatedCredential(t *testing.T) {
	t.Setenv("COUNTER_SCOPE_TOKEN", "first")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "acme/web", Env: "COUNTER_SCOPE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	transport := scopedStageReadTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") == "" {
			t.Fatal("read bypassed credential resolution")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`[{"number":1,"title":"ready","state":"open"}]`)), Request: request}, nil
	})
	previous := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return providers.NewGitHubProvider(token, append([]func(*providers.GitHubProvider){providers.WithHTTPClient(transport)}, opts...)...)
	}
	t.Cleanup(func() { newGitHubProvider = previous })
	directory := t.TempDir()
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web"}}}
	read := func(gaggle, generation string) {
		t.Helper()
		wf := &apiv1.Workflow{Spec: apiv1.WorkflowSpec{Gaggle: gaggle, Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}}}
		counter, err := buildBacklogCounter(cfg, apiv1.Gaggle{}, wf, apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}, resolver, &backlogTestRegistrar{}, directory, nil, "", generation)
		if err != nil {
			t.Fatal(err)
		}
		count, err := counter.EligibleCount(providersnapshot.WithID(t.Context(), "one-evaluation"))
		if err != nil || count != 1 {
			t.Fatal(count, err)
		}
	}
	read("one", "generation-1")
	read("one", "generation-1")
	if calls != 1 {
		t.Fatal("same authorized partition failed to share", calls)
	}
	read("two", "generation-1")
	read("one", "generation-2")
	if calls != 3 {
		t.Fatal("gaggle/generation crossed", calls)
	}
	t.Setenv("COUNTER_SCOPE_TOKEN", "rotated")
	read("one", "generation-1")
	if calls != 4 {
		t.Fatal("credential rotation replayed prior token", calls)
	}
	read("one", "")
	read("one", "")
	if calls != 6 {
		t.Fatal("missing generation shared reads", calls)
	}
}

func TestOpenPRRefresherDoesNotDeduplicateAcrossGagglesOnSameRepository(t *testing.T) {
	t.Setenv("OPENPR_SCOPE_TOKEN", "one-token")
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web", Token: instance.TokenRef{Env: "OPENPR_SCOPE_TOKEN"}}}}
	workflows := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Gaggle: "one", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 2}}}, {Spec: apiv1.WorkflowSpec{Gaggle: "two", Readiness: apiv1.ReadinessConditions{MaxOpenPRs: 2}}}}
	deps := productionRuntimeDeps()
	var polls atomic.Int32
	deps.openPRListers.github = func(string, ...func(*providers.GitHubProvider)) localscheduler.OpenPRLister {
		polls.Add(1)
		return &fakeHeadLister{}
	}
	set, err := deps.openPRRefresher(cfg, workflows, nil, &openPRTestRegistrar{}, nil, t.TempDir(), nil, "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { set.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitOpenPRCount(t, set, "one", "worker")
	waitOpenPRCount(t, set, "two", "worker")
	if polls.Load() != 2 {
		t.Fatal("same-repo gaggles shared an in-memory refresher", polls.Load())
	}
}

func TestADOOpenPRReadCacheUsesActualConfiguredAuthAndFreshScope(t *testing.T) {
	t.Setenv("ADO_SCOPE_TOKEN", "first")
	repo := instance.RepoRef{Provider: "ado", Owner: "org", Project: "project", Name: "repo", Token: instance.TokenRef{Env: "ADO_SCOPE_TOKEN"}}
	deps := productionRuntimeDeps().openPRListers
	original := deps.ado
	var conditionals []bool
	transport := scopedStageReadTransport(func(request *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Basic ") {
			t.Fatal("configured ADO auth missing")
		}
		conditional := request.Header.Get("If-None-Match") != ""
		conditionals = append(conditionals, conditional)
		status := http.StatusOK
		if conditional {
			status = http.StatusNotModified
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Etag": {`"version"`}}, Body: io.NopCloser(strings.NewReader(`{"value":[]}`)), Request: request}, nil
	})
	deps.ado = func(repo instance.RepoRef, reg runner.SecretRegistrar, stores credentials.StoreResolver) (localscheduler.OpenPRLister, error) {
		l, err := original(repo, reg, stores)
		if err != nil {
			return nil, err
		}
		l.(*providers.ADOProvider).Client = transport
		return l, nil
	}
	dir := t.TempDir()
	read := func(gaggle, generation string) {
		t.Helper()
		l, ref, _, err := deps.forRepo(gaggle, repo, nil, &openPRTestRegistrar{}, dir, nil, generation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = l.ListOpenPullRequests(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
	read("one", "generation-1")
	read("one", "generation-1")
	read("two", "generation-1")
	read("one", "generation-2")
	t.Setenv("ADO_SCOPE_TOKEN", "rotated")
	read("one", "generation-1")
	if len(conditionals) != 5 || !conditionals[1] || conditionals[0] || conditionals[2] || conditionals[3] || conditionals[4] {
		t.Fatal("scope or credential crossed", conditionals)
	}
}

func TestStageADOReadCacheRequiresExplicitLaunchScope(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(layoutFor(root).SchedulerDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(executor.GaggleEnvVar, "own")
	t.Setenv(executor.ConfigGenerationEnvVar, "pinned-run-generation")
	t.Setenv(providersnapshot.EnvVar, "evaluation")
	t.Setenv(executor.ProviderReadBindingEnvVar, "")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "")
	calls := 0
	transport := scopedStageReadTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"value":[]}`)), Request: request}, nil
	})
	previous := newADOProviderForStage
	newADOProviderForStage = func(repo providers.RepositoryRef, source providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		p := providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source))
		p.Client = transport
		return p, nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })
	source, err := providers.NewADODeliveredCredentialSourceWithExpiry("pat", "same-token", "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	read := func() {
		t.Helper()
		p, err := newRegisteredADOProviderForStage(stageProviderConfig{root: root, repo: repo, adoSource: source})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.(*providers.ADOProvider).ListOpenPullRequests(t.Context(), repo); err != nil {
			t.Fatal(err)
		}
	}
	read()
	read()
	if calls != 2 {
		t.Fatal("gaggle/config alone inferred automation scope", calls)
	}
	t.Setenv(executor.ProviderReadBindingEnvVar, "automation")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "policy-1")
	read()
	read()
	if calls != 3 {
		t.Fatal("explicit ADO stage scope failed to share", calls)
	}
	t.Setenv(executor.ProviderReadBindingEnvVar, "")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "")
	read()
	read()
	if calls != 5 {
		t.Fatal("human/unknown stage reused automation snapshot", calls)
	}
}

func TestCounterReadScopeLaunchOverridesAuthoredAndAmbientScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub uses Unix shell")
	}
	t.Setenv(executor.ProviderReadBindingEnvVar, "forged-ambient")
	t.Setenv(executor.ProviderReadGenerationEnvVar, "forged-ambient")
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "goobers")
	script := "#!/bin/sh\nprintf '%s/%s' \"$GOOBERS_PROVIDER_READ_BINDING\" \"$GOOBERS_PROVIDER_READ_GENERATION\"\n"
	if err = os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			rec := runnerWiringArtifactRecorder{}
			delegate, err := buildDeterministicExecutor(deterministicExecutorInput{AutomationReadCache: enabled, Config: &instance.Config{Runner: instance.RunnerConfig{EnvPassthrough: []string{executor.ProviderReadBindingEnvVar, executor.ProviderReadGenerationEnvVar}}}, Resolver: resolver, SharedRegistry: journal.NewRegistryScrubber(), InstanceRoot: t.TempDir(), SelfBin: stub, ArtifactRecorder: rec, SecretRegistrar: journal.NewRegistryScrubber(), ScratchDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			result, err := delegate.Run(t.Context(), apiv1.InvocationEnvelope{TaskID: "task", Gaggle: "own", ConfigGeneration: "pinned", Workspace: t.TempDir()}, apiv1.DeterministicRun{Command: []string{"goobers", "read"}, Env: map[string]string{executor.ProviderReadBindingEnvVar: "forged-authored", executor.ProviderReadGenerationEnvVar: "forged-authored"}})
			if err != nil || result.Status != apiv1.ResultSuccess {
				t.Fatal(result, err)
			}
			want := "/"
			if enabled {
				want = "automation/pinned"
			}
			if actual := string(rec["task/stdout.log"]); actual != want {
				t.Fatal(actual, want)
			}
		})
	}
}

func TestCounterReadScopeComesFromRetainedSchedulerGeneration(t *testing.T) {
	f := sourceHost(t, "    - type: backlog-item")
	counter, ok := f.entry.BacklogCounter.(*backlogCounter)
	if !ok {
		t.Fatal("configured counter unavailable")
	}
	if counter.readScope.Gaggle != f.entry.Gaggle || counter.readScope.Generation != f.generation || counter.readScope.Binding != "automation" {
		t.Fatal(counter.readScope)
	}
}
