package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
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
	set, _, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
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
		current, _, err := loadConfigDirectory(layout.ConfigDir())
		if err != nil {
			t.Fatal(err)
		}
		service.Replace(credentialPlaneDefinitionsFromSet(current))
	}
	writeFixture(t, workflowPath, deterministicWorkflowYAML)
	_, err = service.Resolve(t.Context(), request)
	if refusal := planeErrorOf(t, err); refusal.Code != "merge_authority_revoked" {
		t.Fatalf("unexpected revocation: %+v", refusal)
	}
	if materializations != 2 {
		t.Fatalf("revoked grant materialized credentials: %d", materializations)
	}
}
