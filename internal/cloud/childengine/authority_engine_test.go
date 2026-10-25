package childengine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

type recordingParentAuthority struct {
	engine.ChildWorkflowAuthority
	parent ParentAuthority
	got    error
	called bool
}

func (a *recordingParentAuthority) AcquireChildAuthority(ctx context.Context, req engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	a.called = true
	_, a.got = a.parent.PrepareOrigin(ctx, req.RunID, req.Origin)
	return engine.ChildAuthorityGrant{}, errors.New("parent authority recorded")
}

type unreachableParentDispatcher struct{ engine.StageDispatcher }

// The real engine must journal the same complete definition the control
// plane pinned. Omitting frozen annotations still fails the identity guard.
func TestParentAuthorityThroughEngineDefinitionAnnotations(t *testing.T) {
	for name, preserved := range map[string]bool{"preserved": true, "dropped": false} {
		t.Run(name, func(t *testing.T) {
			f := newParentFixture(t)
			root := t.TempDir()
			writer, err := livejournal.NewWriter(func(string) (string, bool) { return root, true })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(writer.Close)
			p := f.authority()
			p.Journal = func(_ context.Context, id string) (journal.RunReader, error) {
				return journal.OpenReadOnly(filepath.Join(root, id))
			}
			a := &recordingParentAuthority{parent: p}
			s, err := dispatcher.NewSurrenderDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&engine.Activities{Journal: writer, ChildAuthority: a, Dispatcher: unreachableParentDispatcher{}, Surrenders: s})
			var def wf.Definition
			if err := json.Unmarshal(f.definition, &def); err != nil {
				t.Fatal(err)
			}
			enabled := true
			in := engine.RunInput{InstanceID: parentInstance, RunID: f.pins.RunID, Gaggle: f.pins.Gaggle, WorkflowName: def.Name, Version: def.Version, DSLVersion: def.DSLVersion, Spec: def.Spec, WorkflowDigest: f.pins.WorkflowDigest, GooberDigest: f.pins.GooberDigest, ConfigGeneration: f.pins.ConfigGeneration, RepoRef: f.pins.RepoRef, BranchNamespace: f.pins.BranchNamespace, LiveJournal: true, PreviewFeaturesEnabled: &enabled, ChildWorkflowsEnabled: true, Placements: []engine.PinnedPlacement{{Stage: "plan", Queue: "q", Eligible: []dispatcher.RunnerSpec{{Name: "r", HostKind: "image", OS: "linux"}}}}}
			if preserved {
				in.Annotations = def.Annotations
			}
			env.ExecuteWorkflow(engine.Run, in)
			if !a.called {
				t.Fatalf("authority was not checked: %v", env.GetWorkflowError())
			}
			if preserved {
				if a.got != nil {
					t.Fatalf("preserved annotations refused: %v", a.got)
				}
			} else if !errors.Is(a.got, childworkflow.ErrAuthorityUnavailable) || !strings.Contains(a.got.Error(), "journal identity") {
				t.Fatalf("dropped annotations did not fail identity guard: %v", a.got)
			}
		})
	}
}
