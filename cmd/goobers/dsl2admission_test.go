package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Exercise the real loader, per-stage pinning, starter choice, and scheduler
// admission. DSL 2.0 capability tags are opaque: an image's structured OS alone
// does not satisfy os=linux, and two runners cannot combine claims for one task.
func TestDSL2ImageCapabilityAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstCaps string
		second    string
		wantPins  []string
	}{
		{name: "image satisfies legacy tag", firstCaps: "os=linux", wantPins: []string{"linux"}},
		{name: "each stage has its own eligible runner", firstCaps: "os=linux", second: "tool@1", wantPins: []string{"linux", "tool"}},
		{name: "claims cannot combine across runners", firstCaps: "os=linux, tool@1"},
		{name: "structured OS does not imply a legacy tag", firstCaps: "os=windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDeterministicDemo(t)
			replaceInFile(t, filepath.Join(root, "instance.yaml"), "runner: {}", `runner: {}
schemaVersion: 2
runners:
  - name: linux
    host: ghcr.io/example/linux:v1
    provides:
      os: linux
      capabilities: [os=linux]
  - name: tool
    host: ghcr.io/example/tool:v1
    provides:
      os: windows
      capabilities: [tool@1]
engine:
  hostPort: temporal.example:7233
`)
			definition := strings.Replace(remoteOnlyV30WorkflowYAML, `dslVersion: "3.0"`, `dslVersion: "2.0"`, 1)
			definition = strings.Replace(definition, "      runsOn:\n        os: windows", "      requiredCapabilities: ["+tc.firstCaps+"]", 1)
			if tc.second != "" {
				definition += "      next: second\n    - name: second\n      type: deterministic\n      goal: run second stage\n      requiredCapabilities: [" + tc.second + "]\n      run:\n        command: [\"goobers\", \"docs-churn\"]\n"
			}
			writeSecondWorkflow(t, root, definition)
			var wg sync.WaitGroup
			setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = setup.Shutdown(context.Background()) }()
			identity := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "win-build"}
			set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
			if err != nil {
				t.Fatal(err)
			}
			if report.HasErrors() {
				t.Fatalf("config report: %+v", report)
			}
			pins, pinErr := bootstrap.PinStagePlacements(setup.Config, set, identity.Gaggle, setup.Machines[identity].Def)
			var entry localscheduler.WorkflowEntry
			for _, candidate := range setup.Entries {
				if candidate.Workflow == identity.Workflow {
					entry = candidate
				}
			}
			if entry.Starter == nil {
				t.Fatal("workflow entry missing")
			}
			sched := localscheduler.New(setup.Entries, setup.InstanceLog,
				localscheduler.WithRunnerCapabilities(setup.Config.SelfRunnerCapabilities()))
			runID, triggerErr := sched.Trigger(context.Background(), identity.Workflow, time.Now())
			if tc.wantPins == nil {
				if pinErr == nil {
					t.Fatalf("ineligible stage was pinned: %+v", pins)
				}
				var rejected *localscheduler.TriggerRejectedError
				if !errors.As(triggerErr, &rejected) || !strings.HasPrefix(rejected.Reason, localscheduler.ReasonPlacementUnsatisfiable) {
					t.Fatalf("unplaceable stage must remain refused: %v", triggerErr)
				}
				return
			}
			if pinErr != nil {
				t.Fatal(pinErr)
			}
			var gotPins []string
			for _, pin := range pins {
				if pin.Self || len(pin.Eligible) != 1 {
					t.Fatalf("expected one remote eligible runner: %+v", pin)
				}
				gotPins = append(gotPins, pin.Eligible[0].Name)
			}
			if !reflect.DeepEqual(gotPins, tc.wantPins) {
				t.Fatalf("runner pins = %v, want %v", gotPins, tc.wantPins)
			}
			if _, ok := entry.Starter.(*engineStarter); !ok {
				t.Fatalf("remote-pinned DSL 2.0 entry must use engine, got %T", entry.Starter)
			}
			if triggerErr != nil {
				t.Fatalf("valid image-runner capabilities must admit: %v", triggerErr)
			}
			waitForConfigValue(t, "DSL 2.0 run to reach engine dispatch", func() (bool, bool) {
				events, err := journal.ReadInstanceLog(setup.InstanceLog.Dir())
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.RunID == runID && strings.Contains(event.Status, errEngineRuntimeUnattached.Error()) {
						return true, true // Reached the engine, not host execution.
					}
				}
				return false, false
			})
		})
	}
}

func TestDSL2LocalCapabilityAdmissionRetainsHostPreflight(t *testing.T) {
	for _, engineEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "engine disabled", true: "self-pinned with engine enabled"}[engineEnabled], func(t *testing.T) {
			root := initDeterministicDemo(t)
			replaceInFile(t, filepath.Join(root, "instance.yaml"), "runner: {}", "runner:\n  capabilities: [os=linux]")
			if engineEnabled {
				appendToFile(t, filepath.Join(root, "instance.yaml"), "engine:\n  hostPort: temporal.example:7233\n")
			}
			definition := strings.Replace(remoteOnlyV30WorkflowYAML, `dslVersion: "3.0"`, `dslVersion: "2.0"`, 1)
			definition = strings.Replace(definition, "      runsOn:\n        os: windows", "      requiredCapabilities: [os=linux]", 1)
			writeSecondWorkflow(t, root, definition)
			var wg sync.WaitGroup
			setup, err := buildSchedulerSetup(context.Background(), instance.NewLayout(root), &wg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = setup.Shutdown(context.Background()) }()
			for _, entry := range setup.Entries {
				if entry.Workflow != "win-build" {
					continue
				}
				fallback, ok := entry.Starter.(*runnerFallbackStarter)
				if !ok {
					t.Fatalf("local entry starter = %T", entry.Starter)
				}
				tracked, ok := fallback.next.(*trackedStarter)
				if !ok {
					t.Fatalf("local underlying starter = %T", fallback.next)
				}
				if !reflect.DeepEqual(tracked.requiredCaps, []string{"os=linux"}) {
					t.Fatalf("host preflight requirements lost: %v", tracked.requiredCaps)
				}
				// With no advertised self capabilities the original local guard
				// must still refuse before starting even when an engine exists.
				sched := localscheduler.New([]localscheduler.WorkflowEntry{entry}, setup.InstanceLog)
				_, err := sched.Trigger(context.Background(), entry.Workflow, time.Now())
				var rejected *localscheduler.TriggerRejectedError
				if !errors.As(err, &rejected) || !strings.HasPrefix(rejected.Reason, localscheduler.ReasonMissingCapability) {
					t.Fatalf("local capability enforcement lost: %v", err)
				}
				return
			}
			t.Fatal("workflow entry missing")
		})
	}
}
