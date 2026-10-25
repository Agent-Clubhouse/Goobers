package childengine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

type annotationOriginAuthority struct {
	engine.ChildWorkflowAuthority
	parent ParentAuthority
	got    error
	called bool
}

func (a *annotationOriginAuthority) AcquireChildAuthority(ctx context.Context, req engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	a.called = true
	_, a.got = a.parent.PrepareOrigin(ctx, req.RunID, req.Origin)
	return engine.ChildAuthorityGrant{}, errors.New("stop after origin assertion")
}

type annotationOriginDispatcher struct{ engine.StageDispatcher }

func TestAnnotatedRunJournalMatchesParentAuthority(t *testing.T) {
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
	a := &annotationOriginAuthority{parent: p}
	s, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&engine.Activities{Journal: writer, ChildAuthority: a, Dispatcher: annotationOriginDispatcher{}, Surrenders: s})
	var def wf.Definition
	if err := json.Unmarshal(f.definition, &def); err != nil {
		t.Fatal(err)
	}
	if def.Annotations["goobers.dev/allow-preview-features"] != "true" {
		t.Fatal("fixture must exercise an annotated StageCatalog definition")
	}
	enabled := true
	in := engine.RunInput{InstanceID: parentInstance, RunID: f.pins.RunID, Gaggle: f.pins.Gaggle, WorkflowName: def.Name, Version: def.Version, DSLVersion: def.DSLVersion, Annotations: def.Annotations, Spec: def.Spec, WorkflowDigest: f.pins.WorkflowDigest, GooberDigest: f.pins.GooberDigest, ConfigGeneration: f.pins.ConfigGeneration, RepoRef: f.pins.RepoRef, BranchNamespace: f.pins.BranchNamespace, LiveJournal: true, PreviewFeaturesEnabled: &enabled, ChildWorkflowsEnabled: true, Placements: []engine.PinnedPlacement{{Stage: "plan", Queue: "q", Eligible: []dispatcher.RunnerSpec{{Name: "r", HostKind: "image", OS: "linux"}}}}}
	env.ExecuteWorkflow(engine.Run, in)
	if !a.called || a.got != nil {
		t.Fatalf("called=%v authority=%v workflow=%v", a.called, a.got, env.GetWorkflowError())
	}
}
