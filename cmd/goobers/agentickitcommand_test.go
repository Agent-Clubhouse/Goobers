package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func TestWorkerKitCarriesSelectedHarnessCommandToPod(t *testing.T) {
	for _, selected := range []apiv1.Harness{apiv1.HarnessCopilot, apiv1.HarnessClaudeCode} {
		for _, choice := range []string{"omitted", "explicit-default", "override"} {
			name := string(selected) + "/" + choice
			t.Run(name, func(t *testing.T) {
				root := initDemo(t)
				definition := filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", pinGaggle, "goobers", pinGoober, "goober.yaml")
				writeFileContent(t, definition, strings.Replace(readFileContent(t, definition), "harness: copilot", "harness: "+string(selected), 1))
				cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
				if err != nil {
					t.Fatal(err)
				}
				other := apiv1.HarnessClaudeCode
				if selected == other {
					other = apiv1.HarnessCopilot
				}
				cfg.Runner.HarnessCommand = map[string][]string{string(other): {"unrelated-launcher-must-not-travel"}}
				var declared []string
				switch choice {
				case "override":
					declared = []string{"/opt/fixture wrapper/launcher", "literal $(PATH)", "a b", "quote\"value", "$HOME; untouched"}
				case "explicit-default":
					declared = []string{"copilot"}
					if selected == apiv1.HarnessClaudeCode {
						declared = []string{"claude"}
					}
				}
				if len(declared) > 0 {
					cfg.Runner.HarnessCommand[string(selected)] = slices.Clone(declared)
				}
				if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
					t.Fatal(err)
				}
				seams := workerReloadSeams(t, root)
				endpoint, _ := fakeBlobPlane(t)
				writer := agenticKitWriter{instanceRoot: root, seams: seams, blobEndpoint: endpoint}
				for _, review := range []bool{false, true} {
					env := apiv1.InvocationEnvelope{RunID: "launcher-run", TaskID: "stage", WorkflowID: pinWorkflow, Gaggle: pinGaggle, Goober: pinGoober}
					digest, err := writer.WriteKit(context.Background(), dispatcher.Attempt{RunID: env.RunID, Stage: env.TaskID, Number: 1, Agentic: true, Review: review, Envelope: &env})
					if err != nil {
						t.Fatal(err)
					}
					data, err := (&dispatcher.BlobClient{BaseURL: endpoint}).Get(context.Background(), digest)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(data, []byte("unrelated-launcher-must-not-travel")) {
						t.Fatal("kit disclosed an unrelated harness launcher")
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(data, &fields); err != nil {
						t.Fatal(err)
					}
					if len(declared) > 0 {
						var argv []string
						if err := json.Unmarshal(fields["harnessCommand"], &argv); err != nil {
							t.Fatalf("selected launcher missing from produced kit: %v", err)
						}
						if !slices.Equal(argv, declared) {
							t.Fatalf("kit argv=%q, want literal %q", argv, declared)
						}
					} else if _, ok := fields["harnessCommand"]; ok {
						t.Fatal("default launcher changed legacy kit JSON")
					}
					kit, err := agentickit.Unmarshal(data, digest)
					if err != nil {
						t.Fatal(err)
					}
					if kit.IsReview() != review {
						t.Fatal("review completion contract changed")
					}
					assertPodLauncher(t, kit, selected, declared)
				}
			})
		}
	}
}

func assertPodLauncher(t *testing.T, kit *agentickit.Kit, selected apiv1.Harness, declared []string) {
	t.Helper()
	previous := podHarnessRegistry
	defer func() { podHarnessRegistry = previous }()
	fake := &harnesstest.FakeAdapter{}
	podHarnessRegistry = func(caps map[string]string, allow []string, commands map[string][]string, root, bin string, deferDiscovery bool, credential func(context.Context) (string, error), ephemeral bool) (*harness.Registry, error) {
		// Build the real adapters from the actual pod-constructor arguments, then
		// replace only the external process adapter before its preflight executes.
		actual, err := buildHarnessRegistry(caps, allow, commands, root, bin, deferDiscovery, credential, ephemeral)
		if err != nil {
			return nil, err
		}
		if len(commands) > 1 {
			t.Fatalf("pod received unrelated launchers: %v", commands)
		}
		adapter, err := actual.Get(string(selected))
		if err != nil {
			return nil, err
		}
		var argv []string
		switch a := adapter.(type) {
		case *harness.CopilotAdapter:
			argv = a.Command
			expectedAuthArgs := copilotAuthCheckArgs
			if len(declared) > 0 && !slices.Equal(declared, []string{"copilot"}) {
				expectedAuthArgs = forwardingLauncherAuthCheckArgs()
			}
			if !slices.Equal(a.AuthCheckArgs, expectedAuthArgs) {
				t.Fatal("model authentication preflight was altered")
			}
			if a.RequireLauncherContract != (len(declared) > 0 && !slices.Equal(declared, []string{"copilot"})) {
				t.Fatal("launcher contract admission changed")
			}
		case *harness.ClaudeAdapter:
			argv = a.Command
		default:
			t.Fatalf("unexpected real adapter %T", adapter)
		}
		want := declared
		if len(want) == 0 {
			want = []string{"copilot"}
			if selected == apiv1.HarnessClaudeCode {
				want = []string{"claude"}
			}
		}
		if !slices.Equal(argv, want) {
			t.Fatalf("pod launcher=%q, want selected prefix %q", argv, want)
		}
		registry := harness.NewRegistry()
		if err := registry.RegisterAs(string(selected), fake); err != nil {
			return nil, err
		}
		return registry, nil
	}
	if _, err := buildPodAgenticExecutor(kit, io.Discard, nil, t.TempDir()); err != nil {
		t.Fatalf("pod constructor: %v", err)
	}
	refused := errors.New("fixture preflight refusal")
	fake.PreflightErr = refused
	if _, err := buildPodAgenticExecutor(kit, io.Discard, nil, t.TempDir()); !errors.Is(err, refused) {
		t.Fatalf("pod skipped preflight refusal: %v", err)
	}
}
