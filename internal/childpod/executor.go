package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

// Dispatch is the production dispatcher seam, injectable for custody tests.
type Dispatch interface {
	Dispatch(context.Context, dispatcher.Attempt, []dispatcher.RunnerSpec) (dispatcher.Report, error)
}

// Executor returns only after surrendered tree custody is recorded and the
// exact pod's writers have stopped. Recorder is the owning run's journal.
type Executor struct {
	Dispatcher Dispatch
	Surrenders dispatcher.SurrenderPlane
	Blobs      blobstore.Store
	Recorder   runner.ArtifactRecorder
}

// Execute runs one accepted generated stage. Callers hold the managed child's
// exclusive stage workspace lease throughout this method. No local fallback
// exists; missing isolation, transport or journal custody refuses execution.
func (e *Executor) Execute(ctx context.Context, request Request) (out dispatcher.SurrenderedResult, report dispatcher.Report, err error) {
	if err = e.validate(ctx, request); err != nil {
		return out, report, err
	}
	contract, expected, err := makeContract(ctx, request)
	if err != nil {
		return out, report, err
	}
	digest, err := e.keep(ctx, request, "input", contract)
	if err != nil {
		return out, report, err
	}
	request.Attempt.ChildExecutionDigest = digest
	ack := invoke.RegisterWorkspaceWriter(ctx)
	defer func() {
		if ack == nil {
			return
		}
		if !report.ChildCreateAttempted || report.WorkspaceWritersStopped {
			ack(nil)
		} else {
			ack(dispatcher.ErrChildIsolation)
		}
	}()
	report, err = e.Dispatcher.Dispatch(ctx, request.Attempt, request.Eligible)
	if report.Local || !report.ChildCreateAttempted || report.ChildPodUID == "" || !report.WorkspaceWritersStopped {
		return out, report, errors.Join(err, dispatcher.ErrChildIsolation)
	}
	if err != nil && !errors.Is(err, dispatcher.ErrStageFailed) {
		return out, report, err
	}
	if !report.SurrenderConfirmed {
		return out, report, dispatcher.ErrSurrenderUnconfirmed
	}
	out, err = e.receive(ctx, request, contract, digest, expected)
	if err != nil {
		return out, report, err
	}

	_, err = e.keep(ctx, request, "pod-proof", struct {
		UID     string `json:"uid"`
		Stopped bool   `json:"writersStopped"`
	}{report.ChildPodUID, true})
	return out, report, err
}

func (e *Executor) validate(ctx context.Context, r Request) error {
	if e == nil || e.Dispatcher == nil || e.Surrenders == nil || e.Blobs == nil || e.Recorder == nil {
		return fmt.Errorf("isolated child executor dependencies are incomplete")
	}
	ceiling, ok := credentials.ChildCeilingFromContext(ctx)
	if !ok || !reflect.DeepEqual(ceiling, r.Ceiling) {
		return credentials.ErrChildAuthenticationIsolation
	}
	if r.Attempt.RunID != r.Identity.RunID || r.Attempt.Gaggle != r.Identity.Gaggle || r.Attempt.Workflow != r.Identity.Workflow || r.Attempt.InstanceID != r.Identity.InstanceID || r.Attempt.ChildExecutionDigest != "" || r.Attempt.PodAttempt < 1 {
		return fmt.Errorf("child attempt does not match pinned run identity")
	}
	if r.Attempt.Agentic && !blobstore.ValidDigest(r.Attempt.KitDigest) {
		return fmt.Errorf("child agentic kit was not published under retained source authority")
	}
	if r.Workspace != nil && r.Workspace.Path == "" {
		return fmt.Errorf("child workspace custody path is missing")
	}
	if (r.Attempt.Workspace == "repo" || r.Attempt.Workspace == "repo-readonly") != (r.Workspace != nil) {
		return fmt.Errorf("child workspace mode does not match retained custody")
	}
	return nil
}

func makeContract(ctx context.Context, r Request) (Contract, *recovery.ChildSnapshot, error) {
	c := Contract{KitDigest: r.Attempt.KitDigest, Version: 1, Identity: r.Identity, Stage: r.Attempt.Stage, Attempt: r.Attempt.Number, PodAttempt: r.Attempt.PodAttempt, StartedAt: r.StartedAt, Ceiling: r.Ceiling}
	if err := c.Validate(); err != nil {
		return c, nil, err
	}
	var expected *recovery.ChildSnapshot
	if r.Workspace != nil {
		// This ancestry check binds the managed checkout to its accepted fork,
		// independently of whatever commits a prior child stage created.
		if _, err := recovery.CaptureChildResult(ctx, r.Workspace.Path, r.Identity.RunID, r.Workspace.Fork, r.StartedAt, r.StartedAt.AddDate(0, 0, 30)); err != nil {
			return c, nil, err
		}
		carrier, snapshot, err := CaptureCarrier(ctx, r.Workspace.Path, r.Workspace.Fork.Record.RepositoryKey, r.Identity.RunID, r.StartedAt, r.Workspace.Fork.Policy)
		if err != nil {
			return c, nil, err
		}
		c.Workspace, expected = &carrier, &snapshot
	}
	return c, expected, c.Validate()
}

func (e *Executor) keep(ctx context.Context, r Request, kind string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > MaxContractBytes {
		return "", fmt.Errorf("isolated child custody exceeds byte budget")
	}
	digest := journal.Digest(data)
	name := fmt.Sprintf("child-pods/%s-%d-%s.json", r.Attempt.Stage, r.Attempt.PodAttempt, kind)
	if err = e.Blobs.Put(ctx, digest, data); err != nil {
		return "", err
	}
	ref, err := e.Recorder.RecordArtifact(name, data)
	if err != nil {
		return "", err
	}
	if ref.Digest != digest {
		return "", fmt.Errorf("child custody bytes changed at journal boundary")
	}
	return digest, nil
}

func (e *Executor) applyReturn(ctx context.Context, r Request, expected recovery.ChildSnapshot, c Carrier) error {
	op := fmt.Sprintf("child-pod-%s-%d", r.Identity.RunID, r.Attempt.PodAttempt)
	plan, err := prepareReturn(ctx, r.Workspace.Path, expected, c, op, r.StartedAt)
	if err != nil {
		return err
	}
	if _, err = e.keep(ctx, r, "apply-plan", plan); err != nil {
		return err
	}
	return recovery.ApplyChildApplication(ctx, r.Workspace.Path, plan)
}

func (e *Executor) receive(ctx context.Context, request Request, contract Contract, digest string, expected *recovery.ChildSnapshot) (out dispatcher.SurrenderedResult, err error) {
	data, err := e.Surrenders.Get(ctx, request.Identity.RunID, request.Attempt.Stage, request.Attempt.PodAttempt)
	if err != nil {
		return out, err
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &out) != nil || out.Validate() != nil || out.ChildWorkspaceDigest == "" || out.WorkspaceDelta != "" || out.WorkspaceDeltaUnchanged {
		return out, fmt.Errorf("invalid isolated child surrender")
	}
	data, err = e.Blobs.Get(ctx, out.ChildWorkspaceDigest)
	if err != nil {
		return out, err
	}
	returned, err := DecodeOutput(data, out.ChildWorkspaceDigest, digest, contract)
	if err != nil {
		return out, err
	}
	if _, err = e.keep(ctx, request, "output", returned); err != nil {
		return out, err
	}
	if returned.Workspace != nil && (request.Attempt.Review || request.Attempt.Workspace == "repo-readonly") && returned.Workspace.Snapshot.TreeSHA != expected.TreeSHA {
		return out, fmt.Errorf("read-only child invocation changed workspace")
	}
	if returned.Workspace != nil {
		if err = e.applyReturn(ctx, request, *expected, *returned.Workspace); err != nil {
			return out, err
		}
	}
	return out, nil
}
