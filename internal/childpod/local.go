package childpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

// LocalExecutor uses the same bounded snapshot and return contract as workers,
// but the admitted callback invokes the ordinary host executor. It never sends
// a dispatch, invents a pod surrender, or uses remote placement as local authority.
// The daemon must validate retained source, placement and authentication before
// constructing it. Each Execute owns a fresh checkout and authentication home.
type LocalExecutor struct {
	Recorder runner.ArtifactRecorder
	Blobs    blobstore.Store
	// Root is a host-owned staging directory outside every task workspace.
	Root           string
	Authentication []credentials.TrustedLocalAuthentication
	ModelRef       string
	// BeforeStart persists the physical custody marker before the callback can
	// launch. Returned records the exact imported output and writer join.
	BeforeStart func(context.Context, Request, Contract, string) error
	Returned    func(context.Context, Request, journal.Ref) error
}

// Execute returns only after actual process owners prove quiescence and the
// verified return is imported. Failed proof retains the attempt directory for
// diagnosis; a later attempt cannot treat cancellation as writer acknowledgement.
func (e *LocalExecutor) Execute(ctx context.Context, request Request, call func(context.Context, string) (dispatcher.SurrenderedResult, error)) (out dispatcher.SurrenderedResult, err error) {
	if !e.ready(call) {
		return out, errors.New("trusted-local execution custody is incomplete")
	}

	ceiling, ok := credentials.ChildCeilingFromContext(ctx)
	if !ok || !reflect.DeepEqual(ceiling.ModelOnly(), request.Ceiling) {
		return out, credentials.ErrChildAuthenticationIsolation
	}
	contract, expected, err := makeContract(ctx, request)
	if err != nil {
		return out, err
	}
	if err := os.MkdirAll(e.Root, 0700); err != nil {
		return out, err
	}
	dir, err := os.MkdirTemp(e.Root, "attempt-")
	if err != nil {
		return out, err
	}
	removable := true
	defer func() {
		if removable {
			_ = os.RemoveAll(dir)
		}
	}()
	workspace, home := filepath.Join(dir, "worktree"), filepath.Join(dir, "home")
	for _, path := range []string{workspace, home} {
		if err := os.Mkdir(path, 0700); err != nil {
			return out, err
		}
	}
	if err := MaterializeContract(ctx, workspace, contract); err != nil {
		return out, err
	}
	localCtx, err := credentials.WithTrustedLocalProcess(ctx, home, e.Authentication...)
	if err != nil {
		return out, err
	}
	if e.ModelRef != "" {
		localCtx, err = credentials.WithTrustedLocalModelCredential(localCtx, e.ModelRef)
		if err != nil {
			return out, err
		}
	}
	inputRef, err := e.keep(ctx, request, "input", contract)
	if err != nil {
		return out, err
	}
	request.Attempt.ChildExecutionDigest = inputRef.Digest
	if err := e.BeforeStart(ctx, request, contract, inputRef.Digest); err != nil {
		return out, err
	}
	removable = false
	// This outer owner joins only after output import. The inner scope requires
	// evidence from real process owners, so a returned fake/unknown adapter cannot
	// manufacture custody by merely returning a result.
	acknowledge := invoke.RegisterWorkspaceWriter(ctx)
	localCtx, proof := invoke.WithWorkspaceQuiescence(localCtx)
	out, callErr := call(localCtx, workspace)
	if err := proof.Verify(); err != nil {
		if acknowledge != nil {
			acknowledge(err)
		}
		return out, errors.Join(callErr, err, invoke.ErrChildCustodyPending)
	}
	// A deliberate parent handoff cancels its invocation. Still preserve the
	// last acknowledged tree and its Git state under a bounded independent scope.
	owned, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	err = e.importReturn(owned, request, workspace, contract, expected, inputRef)

	if acknowledge != nil {
		acknowledge(err)
	}
	if err != nil {
		return out, errors.Join(callErr, err, invoke.ErrChildCustodyPending)
	}
	removable = true
	return out, callErr
}

func (e *LocalExecutor) keep(ctx context.Context, request Request, kind string, value any) (journal.Ref, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return journal.Ref{}, err
	}
	if len(data) > MaxContractBytes {
		return journal.Ref{}, blobstore.ErrTooLarge
	}
	digest := journal.Digest(data)
	if err := e.Blobs.Put(ctx, digest, data); err != nil {
		return journal.Ref{}, err
	}
	ref, err := e.Recorder.RecordArtifact(fmt.Sprintf("local-attempts/%s-%d-%d-%s.json", request.Attempt.Stage, request.Attempt.Number, request.Attempt.PodAttempt, kind), data)
	if err == nil && ref.Digest != digest {
		err = errors.New("trusted-local custody changed at journal boundary")
	}
	return ref, err
}

func (e *LocalExecutor) importReturn(ctx context.Context, request Request, workspace string, contract Contract, expected *recovery.ChildSnapshot, inputRef journal.Ref) error {
	returned, err := CaptureOutput(ctx, workspace, contract, inputRef.Digest)
	if err == nil && returned.Workspace != nil && (request.Attempt.Review || request.Attempt.Workspace == "repo-readonly") && returned.Workspace.Snapshot.TreeSHA != expected.TreeSHA {
		err = errors.New("read-only trusted-local invocation changed workspace")
	}
	var outputRef journal.Ref
	if err == nil {
		outputRef, err = e.keep(ctx, request, "output", returned)
	}
	if err == nil && returned.Workspace != nil {
		transport := Executor{Recorder: e.Recorder, Blobs: e.Blobs}
		err = transport.applyReturn(ctx, request, *expected, contract, returned, nil)
	}
	if err == nil {
		err = e.Returned(ctx, request, outputRef)
	}
	return err
}

func (e *LocalExecutor) ready(call func(context.Context, string) (dispatcher.SurrenderedResult, error)) bool {
	return e != nil && e.Recorder != nil && e.Blobs != nil && e.BeforeStart != nil && e.Returned != nil && call != nil && e.Root != ""
}
