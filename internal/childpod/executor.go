package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

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
	// KeepAttempt persists exact host custody before a writer-started marker or dispatch.
	KeepAttempt func(context.Context, RetainedAttempt) error
	// KeepContribution publishes host custody only after exact return import.
	// A failure poisons writer acknowledgement and preserves the checkout.
	KeepContribution func(context.Context, Request, journal.Ref) error
	// RecoveryReader supplies host-only application intent for exact replay.
	RecoveryReader *journal.Reader
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
	if err = e.keepAttempt(ctx, request, expected); err != nil {
		return out, report, err
	}
	binder, canBind := e.Blobs.(interface {
		BindContract(context.Context, string) error
	})
	if canBind {
		if err = binder.BindContract(ctx, digest); err != nil {
			return out, report, err
		}
	}
	custodyImported := false
	ack := invoke.RegisterWorkspaceWriter(ctx)
	defer func() {
		if ack == nil {
			return
		}
		if !report.ChildCreateAttempted || (report.WorkspaceWritersStopped && custodyImported) {
			ack(nil)
		} else {
			ack(dispatcher.ErrChildIsolation)
		}
	}()
	report, dispatchErr := e.Dispatcher.Dispatch(ctx, request.Attempt, request.Eligible)
	if report.Local || !report.ChildCreateAttempted || report.ChildPodUID == "" || !report.WorkspaceWritersStopped {
		return out, report, errors.Join(dispatchErr, dispatcher.ErrChildIsolation)
	}
	if !report.SurrenderConfirmed {
		return out, report, errors.Join(dispatchErr, dispatcher.ErrSurrenderUnconfirmed)
	}
	// The exact pod has stopped and surrendered. Cancellation must not drop
	// this last tree: parent handoff deliberately cancels the invocation.
	// The original dispatch outcome is returned after bounded custody import.
	custodyCtx, cancelCustody := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancelCustody()
	out, err = e.receive(custodyCtx, request, contract, digest, expected, nil)
	if err != nil {
		return out, report, err
	}

	_, err = e.keep(custodyCtx, request, "pod-proof", struct {
		UID     string `json:"uid"`
		Stopped bool   `json:"writersStopped"`
	}{report.ChildPodUID, true})
	custodyImported = err == nil
	if errors.Is(dispatchErr, dispatcher.ErrStageFailed) {
		dispatchErr = nil // the validated surrendered result carries stage failure
	}
	return out, report, errors.Join(err, dispatchErr)
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
	if r.Attempt.Envelope != nil {
		for _, pointer := range r.Attempt.Envelope.ContextPointers {
			if pointer.Artifact != nil {
				c.ContextDigests = append(c.ContextDigests, pointer.Artifact.Digest)
			}
		}
		slices.Sort(c.ContextDigests)
		c.ContextDigests = slices.Compact(c.ContextDigests)
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
	ref, err := e.keepRef(ctx, r, kind, value)
	return ref.Digest, err
}

func (e *Executor) keepRef(ctx context.Context, r Request, kind string, value any) (journal.Ref, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return journal.Ref{}, err
	}
	if len(data) > MaxContractBytes {
		return journal.Ref{}, fmt.Errorf("isolated child custody exceeds byte budget")
	}
	digest := journal.Digest(data)
	name := fmt.Sprintf("child-pods/%s-%d-%d-%s.json", r.Attempt.Stage, r.Attempt.Number, r.Attempt.PodAttempt, kind)
	if err = e.Blobs.Put(ctx, digest, data); err != nil {
		return journal.Ref{}, err
	}
	ref, err := e.Recorder.RecordArtifact(name, data)
	if err != nil {
		return journal.Ref{}, err
	}
	if ref.Digest != digest {
		return journal.Ref{}, fmt.Errorf("child custody bytes changed at journal boundary")
	}
	return ref, nil
}

func (e *Executor) applyReturn(ctx context.Context, r Request, expected recovery.ChildSnapshot, c Carrier, retained *recovery.ChildApplyPlan) error {
	if retained != nil {
		if err := verifyRetainedPlan(*retained, expected, c); err != nil {
			return err
		}
		return recovery.ApplyChildApplication(ctx, r.Workspace.Path, *retained)
	}
	identity, _ := json.Marshal([]any{r.Identity.RunID, r.Attempt.Stage, r.Attempt.Number, r.Attempt.PodAttempt})
	op := "child-pod-" + strings.TrimPrefix(journal.Digest(identity), "sha256:")
	plan, err := prepareReturn(ctx, r.Workspace.Path, expected, c, op, r.StartedAt)
	if err != nil {
		return err
	}
	if err = e.retainPlan(ctx, r, plan); err != nil {
		return err
	}
	return recovery.ApplyChildApplication(ctx, r.Workspace.Path, plan)
}

func (e *Executor) receive(ctx context.Context, request Request, contract Contract, digest string, expected *recovery.ChildSnapshot, plan *recovery.ChildApplyPlan) (out dispatcher.SurrenderedResult, err error) {
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
	outputRef, err := e.keepRef(ctx, request, "output", returned)
	if err != nil {
		return out, err
	}
	if returned.Workspace != nil && (request.Attempt.Review || request.Attempt.Workspace == "repo-readonly") && returned.Workspace.Snapshot.TreeSHA != expected.TreeSHA {
		return out, fmt.Errorf("read-only child invocation changed workspace")
	}
	if returned.Workspace != nil {
		if err = e.applyReturn(ctx, request, *expected, *returned.Workspace, plan); err != nil {
			return out, err
		}
	}
	if e.KeepContribution != nil {
		if err := e.KeepContribution(ctx, request, outputRef); err != nil {
			return out, err
		}
	}
	return out, nil
}
