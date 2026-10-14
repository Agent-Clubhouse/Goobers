package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

const podApplyPlanned = "isolated.pod.apply-planned"

// Reconcile imports an already stopped worker's output under exclusive host
// workspace/journal ownership. It neither launches work nor resolves secrets.
// Callers must first verify the exact unjoined host scope and retained receipt,
// and only join that scope after adopting all returned artifacts successfully.
func (e *Executor) Reconcile(ctx context.Context, request Request, retained RetainedAttempt, report dispatcher.Report) (dispatcher.SurrenderedResult, error) {
	var out dispatcher.SurrenderedResult
	if e == nil || e.Blobs == nil || e.Surrenders == nil || e.Recorder == nil || e.RecoveryReader == nil {
		return out, errors.New("isolated pod recovery dependencies incomplete")
	}
	if err := retained.validate(); err != nil {
		return out, err
	}
	if !reflect.DeepEqual(request.Attempt, retained.Input.Attempt) || !reflect.DeepEqual(request.Eligible, retained.Input.Eligible) {
		return out, errors.New("recovery request differs from retained dispatch")
	}
	if report.Local || !report.ChildCreateAttempted || report.ChildPodUID == "" || !report.WorkspaceWritersStopped || !report.SurrenderConfirmed {
		return out, dispatcher.ErrChildIsolation
	}
	custody, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	ctx = custody
	digest := retained.Input.Attempt.ChildExecutionDigest
	data, err := e.Blobs.Get(ctx, digest)
	if err != nil {
		return out, err
	}
	contract, err := DecodeContract(data, digest)
	if err != nil {
		return out, err
	}
	if err = verifyRetainedContract(request, retained, contract); err != nil {
		return out, err
	}
	plan, err := readRetainedPlan(e.RecoveryReader, request, digest)
	if err != nil {
		return out, err
	}
	// Exact stopped proof permits bounded custody settlement after cancellation.
	out, err = e.receive(custody, request, contract, digest, retained.HostSnapshot, plan)
	if err != nil {
		return out, err
	}
	_, err = e.keep(custody, request, "pod-proof", struct {
		UID     string `json:"uid"`
		Stopped bool   `json:"writersStopped"`
	}{report.ChildPodUID, true})
	return out, err
}

func verifyRetainedContract(r Request, retained RetainedAttempt, c Contract) error {
	a := retained.Input.Attempt
	if r.ChildBranch != c.ChildBranch || r.ParentBranch != c.ParentBranch || !reflect.DeepEqual(r.ParentOrigin, c.ParentOrigin) || !reflect.DeepEqual(r.Identity, c.Identity) || !reflect.DeepEqual(r.Ceiling, c.Ceiling) || !r.StartedAt.Equal(c.StartedAt) || c.Stage != a.Stage || c.Attempt != a.Number || c.PodAttempt != a.PodAttempt || c.KitDigest != a.KitDigest {
		return errors.New("retained contract source binding mismatch")
	}
	if (c.Workspace == nil) != (retained.HostSnapshot == nil) || (r.Workspace == nil) != (retained.HostSnapshot == nil) {
		return errors.New("retained contract workspace binding mismatch")
	}
	if retained.HostSnapshot != nil {
		s := retained.HostSnapshot
		if r.Workspace.Path == "" || s.TreeSHA != c.Workspace.Snapshot.TreeSHA || s.Record.RepositoryKey != c.Workspace.Snapshot.Record.RepositoryKey || !slices.Equal(s.Policy.ExcludedPaths, c.Workspace.Snapshot.Policy.ExcludedPaths) {
			return errors.New("retained host tree differs from worker input")
		}
	}
	return nil
}

func (e *Executor) retainPlan(ctx context.Context, r Request, plan recovery.ChildApplyPlan) error {
	ref, err := e.keepRef(ctx, r, "apply-plan", plan)
	if err != nil {
		return err
	}
	writer, ok := e.Recorder.(interface{ Append(journal.Event) error })
	if !ok {
		return errors.New("host application intent recorder unavailable")
	}
	return writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: r.Attempt.Stage, Attempt: r.Attempt.Number, Runner: map[string]any{"kind": podApplyPlanned, "contractDigest": r.Attempt.ChildExecutionDigest, "podAttempt": r.Attempt.PodAttempt, "plan": ref}})
}

func readRetainedPlan(reader *journal.Reader, r Request, digest string) (*recovery.ChildApplyPlan, error) {
	id, err := reader.Identity()
	if err != nil || !reflect.DeepEqual(id, r.Identity) {
		return nil, errors.Join(errors.New("recovery journal identity mismatch"), err)
	}
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	var plan *recovery.ChildApplyPlan
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != podApplyPlanned || event.Runner["contractDigest"] != digest {
			continue
		}
		if plan != nil || event.Stage != r.Attempt.Stage || event.Attempt != r.Attempt.Number {
			return nil, errors.New("duplicate or mismatched pod application intent")
		}
		var body struct {
			PodAttempt int         `json:"podAttempt"`
			Plan       journal.Ref `json:"plan"`
		}
		data, _ := json.Marshal(event.Runner)
		if json.Unmarshal(data, &body) != nil || body.PodAttempt != r.Attempt.PodAttempt {
			return nil, errors.New("pod application physical identity mismatch")
		}
		data, err = reader.ArtifactBytesBounded(body.Plan, MaxRetainedAttemptBytes)
		if err != nil {
			return nil, err
		}
		var value recovery.ChildApplyPlan
		if err = json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		plan = &value
	}
	return plan, nil
}

func verifyRetainedPlan(plan recovery.ChildApplyPlan, expected recovery.ChildSnapshot, c Carrier) error {
	if !reflect.DeepEqual(plan.Disposition.ExpectedParent, expected) || plan.Disposition.Action != recovery.ChildReplace || plan.Disposition.TreeSHA != c.Snapshot.TreeSHA {
		return fmt.Errorf("retained application intent differs from surrendered tree")
	}
	return nil
}
