package childpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

// MaxRetainedAttemptBytes bounds the existing 2 MiB dispatch input and 64 KiB
// host snapshot metadata. Workspace bundle bytes remain in scoped blob custody.
const MaxRetainedAttemptBytes = (2 << 20) + (64 << 10)

// RetainedAttempt is host-owned recovery custody, persisted before dispatch.
// Its exact artifact ref belongs on a host-only writer-started annotation.
// HostSnapshot is the current attempt tree, not the child's original fork.
type RetainedAttempt struct {
	Version      int                       `json:"version"`
	Input        engine.ChildDispatchInput `json:"input"`
	HostSnapshot *recovery.ChildSnapshot   `json:"hostSnapshot,omitempty"`
}

func (r RetainedAttempt) validate() error {
	if r.Version != 1 {
		return errors.New("unsupported retained pod attempt")
	}
	if err := r.Input.Validate(); err != nil {
		return err
	}
	a := r.Input.Attempt
	if a.Envelope != nil && a.Envelope.Workspace != "" {
		return errors.New("retained pod attempt contains host workspace path")
	}
	if (a.Workspace == "repo" || a.Workspace == "repo-readonly") != (r.HostSnapshot != nil) {
		return errors.New("retained pod workspace custody mismatch")
	}
	if r.HostSnapshot != nil {
		data, err := json.Marshal(r.HostSnapshot)
		if err != nil || len(data) > 64<<10 {
			return errors.New("retained host snapshot exceeds bound")
		}
	}
	return nil
}

// RecordRetainedAttempt records trusted, bounded, scrubbed host recovery input.
// Callers must persist the returned ref in an unforgeable host lifecycle marker.
func RecordRetainedAttempt(recorder runner.ArtifactRecorder, retained RetainedAttempt) (journal.Ref, error) {
	if err := retained.validate(); err != nil {
		return journal.Ref{}, err
	}
	data, err := json.Marshal(retained)
	if err != nil || len(data) > MaxRetainedAttemptBytes {
		return journal.Ref{}, errors.New("retained pod attempt exceeds byte bound")
	}
	bounded, ok := recorder.(interface {
		RecordArtifactBoundedWithIntegrity(string, []byte, apiv1.Integrity, int) (journal.Ref, error)
	})
	if !ok {
		return journal.Ref{}, errors.New("trusted bounded recovery recorder unavailable")
	}
	a := retained.Input.Attempt
	ref, err := bounded.RecordArtifactBoundedWithIntegrity(fmt.Sprintf("isolated-attempts/%s-%d-%d.json", a.Stage, a.Number, a.PodAttempt), data, apiv1.IntegrityTrusted, MaxRetainedAttemptBytes)
	if err == nil && ref.Digest != journal.Digest(data) {
		err = errors.New("retained pod input changed at journal boundary")
	}
	return ref, err
}

// ReadRetainedAttempt reads only the exact host marker's ref. Looking up by an
// artifact display name would allow a pod-authored artifact to replace custody.
func ReadRetainedAttempt(reader *journal.Reader, ref journal.Ref) (RetainedAttempt, error) {
	var retained RetainedAttempt
	data, err := reader.ArtifactBytesBounded(ref, MaxRetainedAttemptBytes)
	if err != nil {
		return retained, err
	}
	if err = json.Unmarshal(data, &retained); err != nil {
		return retained, err
	}
	if err = restoreRetainedInputs(data, &retained); err != nil {
		return retained, err
	}
	canonical, err := json.Marshal(retained)
	if err != nil || !bytes.Equal(canonical, data) {
		return retained, errors.New("noncanonical retained pod attempt")
	}
	return retained, retained.validate()
}

func (e *Executor) keepAttempt(ctx context.Context, request Request, expected *recovery.ChildSnapshot) error {
	if e.KeepAttempt == nil {
		return nil
	}
	source, ok := e.Dispatcher.(interface {
		RetainedInput(dispatcher.Attempt, []dispatcher.RunnerSpec) engine.ChildDispatchInput
	})
	if !ok {
		return errors.New("exact dispatch input retention unavailable")
	}
	retained := RetainedAttempt{Version: 1, Input: source.RetainedInput(request.Attempt, request.Eligible), HostSnapshot: expected}
	if err := retained.validate(); err != nil {
		return err
	}
	return e.KeepAttempt(ctx, retained)
}
