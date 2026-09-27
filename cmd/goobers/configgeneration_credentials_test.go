package main

import (
	"context"
	"encoding/json"
	"io"
	stdlog "log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/mergepolicy"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

func TestPinnedCredentialPlaneKeepsReferencesAndRotatesValuesButRevokesMerge(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	workflowPath := filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	writeFixture(t, workflowPath, strings.Replace(deterministicWorkflowYAML, "      run:", "      capabilities: [github:pr:merge]\n      run:", 1))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	input, release, err := pinnedDirectEngineInput(t.Context(), layout, cfg, "example", "default-implement", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		t.Fatalf("load credential fixture: %v (%s)", err, validationIssueSummary(report))
	}
	registry, _, err := bootstrap.RegisterGaggleWorkflows(set, "example")
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := registry.Latest("default-implement")
	if !ok {
		t.Fatal("missing definition")
	}
	machine, err := workflow.Compile(definition)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	const runID = "pinned-credentials"
	run, err := journal.Create(layout.ForGaggle("example").RunsDir(), journal.RunIdentity{RunID: runID, InstanceID: input.InstanceID, Gaggle: "example", Workflow: definition.Name, WorkflowVersion: definition.Version, WorkflowDigest: machine.Digest(), ConfigGeneration: input.ConfigGeneration, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, map[string][]byte{journal.PinnedWorkflowDefinitionInputName: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	shared, chain := journal.DefaultScrubber()
	log, _, err := journal.OpenInstanceLog(layout.SchedulerDir(), journal.WithScrubber(chain))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	service := newDaemonCredentialService(layout, cfg, nil, shared, log)
	service.Replace(credentialPlaneDefinitionsFromSet(set))
	materializations := 0
	service.buildSources = func(scope credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
		if scope.Project.Owner != "your-org" {
			t.Errorf("credential scope substituted live provider reference %q", scope.Project.Owner)
		}
		resolver, err := credentials.NewResolverWithExpiring(nil, nil, map[string]credentials.ResolveFunc{"merge": func(context.Context) (string, error) {
			materializations++
			return os.Getenv("GENERATION_TEST_TOKEN"), nil
		}}, nil)
		return resolver, []credentials.Grant{{Capability: "github:pr:merge", Ref: "merge"}}, err
	}
	request := httpapi.CredentialResolveRequest{RunID: runID, Stage: "local-ci", Capabilities: []string{"github:pr:merge"}}
	for _, value := range []string{"first-value", "rotated-value"} {
		t.Setenv("GENERATION_TEST_TOKEN", value)
		response, err := service.Resolve(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Credentials) != 1 || response.Credentials[0].Value != value {
			t.Fatal("credential value was captured with execution generation")
		}
		// A reload can change the currently served provider binding, but it must
		// not redirect a previously admitted run's credential scope.
		changed := readFileContent(t, gagglePath(root, "example"))
		writeFixture(t, gagglePath(root, "example"), strings.ReplaceAll(changed, "your-org", "new-org"))
		current, report, err := loadConfigDirectory(layout.ConfigDir())
		if err != nil {
			t.Fatalf("reload credential fixture: %v (%s)", err, validationIssueSummary(report))
		}
		service.Replace(credentialPlaneDefinitionsFromSet(current))
	}

	verifyRevocation := checkRemoteMergeAuthorityBeforeRevocation(t, layout, log, input.ConfigGeneration, runID)
	writeFixture(t, workflowPath, deterministicWorkflowYAML)
	verifyRevocation()

	_, err = service.Resolve(t.Context(), request)
	if refusal := planeErrorOf(t, err); refusal.Code != "merge_authority_revoked" {
		t.Fatalf("unexpected revocation: %+v", refusal)
	}
	if materializations != 2 {
		t.Fatalf("revoked grant materialized credentials: %d", materializations)
	}
}

type mergeAuthorityTestLander struct{ calls *int }

func (l mergeAuthorityTestLander) Land(context.Context, *providers.Dispatcher, mergepolicy.Request) (mergepolicy.Result, error) {
	*l.calls++
	return mergepolicy.Result{}, nil
}

func checkRemoteMergeAuthorityBeforeRevocation(t *testing.T, layout instance.Layout, instanceLog *journal.InstanceLog, generation, runID string) func() {
	t.Helper()

	// A grant was already issued above. The portable CLI has no daemon config
	// tree, and a worker's old copy would still declare the original merge grant.
	registryTokens := podauth.NewRegistry()
	auth, err := podauth.NewAuthenticator(registryTokens, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), stdlog.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithRunJournalService(newDaemonRunJournalService(layout, instanceLog)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	worker := &workerSeams{recoveryEmitter: &livejournal.HTTPEmitter{BaseURL: server.URL, Token: "parent-only"}, journalMinter: registryTokens}
	invocation := apiv1.InvocationEnvelope{RunID: runID, Gaggle: "example", WorkflowID: "default-implement", TaskID: runID + ":local-ci", Capabilities: []string{"github:pr:merge"}, ConfigGeneration: generation}
	workerContext, err := worker.mergeAuthorityContext(t.Context(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireInvocationMergeAuthority(workerContext, instance.NewLayout(t.TempDir()), invocation); err != nil {
		t.Fatalf("worker used stale/local config: %v", err)
	}
	token, err := registryTokens.MintScoped(runID, time.Hour, podauth.ScopeJournal)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(journalclient.EnvEndpoint, server.URL)
	t.Setenv(journalclient.EnvToken, token)
	t.Setenv(executor.RunIDEnvVar, runID)
	t.Setenv(executor.GaggleEnvVar, "example")
	t.Setenv(executor.TaskEnvVar, "")
	t.Setenv("GOOBERS_STAGE", "local-ci")
	t.Setenv(executor.InstanceRootEnvVar, t.TempDir())
	t.Setenv(executor.ConfigGenerationEnvVar, "")
	podContext := podAgenticMergeAuthorityContext(t.Context(), invocation)
	if err := requireInvocationMergeAuthority(podContext, instance.NewLayout(t.TempDir()), invocation); err != nil {
		t.Fatalf("agentic pod cannot check daemon authority: %v", err)
	}
	landed := 0
	lander := currentMergeAuthorityLander{Lander: mergeAuthorityTestLander{calls: &landed}, capability: capability.GitHubPRMerge}
	if _, err := lander.Land(t.Context(), nil, mergepolicy.Request{}); err != nil {
		t.Fatalf("portable CLI requires no local live config: %v", err)
	}
	client, err := journalclient.NewHTTP(journalclient.HTTPConfig{BaseURL: server.URL, Token: token, RunID: runID, Gaggle: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RequireMergeAuthority(t.Context(), "missing-stage", string(capability.GitHubPRMerge)); err == nil {
		t.Fatal("undeclared stage authorized")
	}
	wrongRun, err := journalclient.NewHTTP(journalclient.HTTPConfig{BaseURL: server.URL, Token: token, RunID: "other-run", Gaggle: "example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongRun.RequireMergeAuthority(t.Context(), "local-ci", string(capability.GitHubPRMerge)); err == nil {
		t.Fatal("cross-run authority authorized")
	}
	wrongScope, err := registryTokens.MintScoped(runID, time.Hour, podauth.ScopeState)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(journalclient.EnvToken, wrongScope)
	if _, err := lander.Land(t.Context(), nil, mergepolicy.Request{}); err == nil {
		t.Fatal("wrong scope authorized merge")
	}
	t.Setenv(journalclient.EnvToken, token)
	return func() {

		if _, err := lander.Land(t.Context(), nil, mergepolicy.Request{}); err == nil {
			t.Fatal("issued credential survived authoritative merge revocation")
		}
		if err := requireInvocationMergeAuthority(workerContext, layout, invocation); err == nil {
			t.Fatal("worker stale admitted pin overrode daemon revocation")
		}
		if err := requireInvocationMergeAuthority(podContext, layout, invocation); err == nil {
			t.Fatal("agentic pod retained revoked merge authority")
		}
		if landed != 1 {
			t.Fatalf("provider land called %d times, want only pre-revocation call", landed)
		}
	}
}
