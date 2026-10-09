package childpod

import (
	"context"
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func retainedFixture() RetainedAttempt {
	r := requestFixture()
	r.Attempt.ChildExecutionDigest = journal.Digest([]byte("contract"))
	return RetainedAttempt{Version: 1, Input: engine.ChildDispatchInput{Attempt: r.Attempt, Eligible: []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage}}, Queue: "queue"}}
}

func TestRetainedAttemptExactRefAndBounds(t *testing.T) {
	id := requestFixture().Identity
	j, err := journal.Create(t.TempDir(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	retained := retainedFixture()
	ref, err := RecordRetainedAttempt(j, retained)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(j.Dir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadRetainedAttempt(reader, ref)
	if err != nil || got.Input.BindingDigest() != retained.Input.BindingDigest() {
		t.Fatal(got, err)
	}
	if _, err = RecordRetainedAttempt(&recordFake{}, retained); err == nil {
		t.Fatal("unbounded untrusted recorder accepted")
	}
	retained.Input.Attempt.PodToken = "secret"
	if _, err = RecordRetainedAttempt(j, retained); err == nil {
		t.Fatal("embedded credential accepted")
	}
	ref.Digest = journal.Digest([]byte("foreign"))
	if _, err = ReadRetainedAttempt(reader, ref); err == nil {
		t.Fatal("foreign ref accepted")
	}
}

func TestTemporalReconcileNeverLaunchesOrReacquiresAuthority(t *testing.T) {
	in := retainedFixture().Input
	run := &childTransportRun{result: engine.ChildDispatchResult{BindingDigest: in.BindingDigest(), Report: dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: "exact", WorkspaceWritersStopped: true, SurrenderConfirmed: true}}}
	c := &childTransportClient{run: run}
	d := TemporalDispatch{Client: c, Admit: func(context.Context) (func(), error) {
		t.Fatal("recovery requested new launch authority")
		return nil, nil
	}}
	report, err := d.Reconcile(t.Context(), in)
	if err != nil || !report.WorkspaceWritersStopped || c.starts != 0 || c.signals != 1 {
		t.Fatal(report, err, c.starts, c.signals)
	}
	run.result.BindingDigest = "foreign"
	if report, err = d.Reconcile(t.Context(), in); err == nil || !report.ChildCreateAttempted || report.WorkspaceWritersStopped {
		t.Fatal("foreign result accepted", report, err)
	}
	run.get = func(context.Context) error { return errors.New("workflow not found") }
	if report, err = d.Reconcile(t.Context(), in); err == nil || !report.ChildCreateAttempted || report.WorkspaceWritersStopped || c.starts != 0 {
		t.Fatal("missing history treated as no create", report, err)
	}
}

type retainedDispatch struct{ dispatchFunc }

func (d retainedDispatch) RetainedInput(a dispatcher.Attempt, eligible []dispatcher.RunnerSpec) engine.ChildDispatchInput {
	return engine.ChildDispatchInput{Attempt: a, Eligible: eligible, Queue: "queue"}
}

func TestExecutorRetainsBeforeDispatchAndRefusesRetentionFailure(t *testing.T) {
	r := requestFixture()
	r.Eligible = retainedFixture().Input.Eligible
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := Executor{Blobs: blobs, Surrenders: plane, Recorder: &recordFake{}}
	kept := false
	e.Dispatcher = retainedDispatch{dispatchFunc: func(context.Context, dispatcher.Attempt, []dispatcher.RunnerSpec) (dispatcher.Report, error) {
		if !kept {
			t.Fatal("dispatch before retention")
		}
		return dispatcher.Report{ChildCreateAttempted: true}, errors.New("unknown outcome")
	}}
	e.KeepAttempt = func(ctx context.Context, retained RetainedAttempt) error {
		if retained.Input.Attempt.ChildExecutionDigest == "" || retained.HostSnapshot != nil {
			t.Fatal(retained)
		}
		if _, err := blobs.Get(ctx, retained.Input.Attempt.ChildExecutionDigest); err != nil {
			t.Fatal(err)
		}
		kept = true
		return nil
	}
	ctx, err := credentials.WithChildCeiling(t.Context(), r.Ceiling)
	if err != nil {
		t.Fatal(err)
	}
	if _, report, err := e.Execute(ctx, r); err == nil || !report.ChildCreateAttempted || !kept {
		t.Fatal(report, err)
	}
	e.KeepAttempt = func(context.Context, RetainedAttempt) error { return errors.New("retention refused") }
	if _, report, err := e.Execute(ctx, r); err == nil || report.ChildCreateAttempted {
		t.Fatal("dispatched without custody", report, err)
	}
}
