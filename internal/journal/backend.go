package journal

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// RunHandle is an opaque backend-owned run incarnation.
type RunHandle string

// InstanceHandle is an opaque backend-owned instance-log incarnation.
type InstanceHandle string

// OwnerToken fences writes to one open incarnation.
type OwnerToken string

// RunKey identifies one run without exposing a storage path.
type RunKey struct {
	Gaggle string
	RunID  string
}

// RunWriterOptions carries storage-independent writer policy.
type RunWriterOptions struct {
	Clock           func() time.Time
	Scrubber        Scrubber
	InputIntegrity  map[string]apiv1.Integrity
	InputSource     map[string]string
	AppendObserver  func(string, uint64)
	ContextObserver func(context.Context, string, uint64)
	ObserverContext context.Context
}

// CreateRunRequest creates one logical run.
type CreateRunRequest struct {
	Identity RunIdentity
	Inputs   map[string][]byte
	Options  RunWriterOptions
}

// RunStatus is the current backend-owned writer state.
type RunStatus struct {
	Seq    uint64
	Phase  RunPhase
	Closed bool
}

// BlobRecord describes a content-addressed artifact write and its journal event.
type BlobRecord struct {
	Event            Event
	Data             []byte
	MaxBytes         int
	Expected         *Ref
	OutboxRelPath    string
	OutboxOccurrence uint64
}

// SpanRecord describes a recorded span blob.
type SpanRecord struct {
	Branch      int
	Stage       string
	Name        string
	DataSchema  string
	Data        []byte
	Annotations map[string]any
}

// ChildStageStartRecord is the atomic child-start append contract.
type ChildStageStartRecord struct {
	Event        Event
	Continuation bool
}

// InstanceAppender is the compatibility surface for an already-open local log.
type InstanceAppender interface{ Append(Event) error }

// Backend is the shared journal write contract.
//
// AppendBatch has ordered-prefix semantics: a possibly empty prefix commits in
// argument order; a later event is never committed without every earlier
// event. Validation failures commit nothing. The returned count is the durable
// prefix length callers use to resume a retry.
type Backend interface {
	CreateRun(context.Context, CreateRunRequest) (RunHandle, OwnerToken, error)
	RecoverRun(context.Context, RunKey, RunWriterOptions) (RunHandle, OwnerToken, RecoverReport, error)
	RunStatus(context.Context, RunHandle) (RunStatus, error)
	AppendBatch(context.Context, RunHandle, OwnerToken, []Event) (int, error)
	AppendIfAbsent(context.Context, RunHandle, OwnerToken, Event, func(Event) bool) (bool, error)
	ClaimNotificationDelivery(context.Context, RunHandle, OwnerToken, apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error)
	Checkpoint(context.Context, RunHandle, OwnerToken) error
	RecordBlob(context.Context, RunHandle, OwnerToken, BlobRecord) (Ref, error)
	ReadBlob(context.Context, RunHandle, Ref) ([]byte, error)
	RecordSpan(context.Context, RunHandle, OwnerToken, SpanRecord) (Ref, error)
	AppendChildStageStarted(context.Context, RunHandle, OwnerToken, ChildStageStartRecord) (uint64, *apiv1.ChildWorkflowOrigin, error)
	BeginTranscript(context.Context, RunHandle, OwnerToken, int, string, string) (ExpectedTranscriptCheckpointSession, error)
	RepairAppendBoundary(context.Context, RunHandle, OwnerToken) error
	AcceptOperatorMessage(context.Context, RunHandle, OwnerToken, apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error)
	RejectOperatorMessage(context.Context, RunHandle, OwnerToken, apiv1.OperatorMessageRequest, string, string) (apiv1.OperatorMessageRecord, bool, error)
	AcknowledgeOperatorMessage(context.Context, RunHandle, OwnerToken, apiv1.OperatorMessageAcknowledgement) (apiv1.OperatorMessageRecord, error)
	CompleteOperatorMessage(context.Context, RunHandle, OwnerToken, apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageRecord, error)
	Retire(context.Context, RunHandle, OwnerToken) error
	OpenInstance(context.Context) (InstanceHandle, OwnerToken, RecoverReport, error)
	BindInstance(InstanceAppender) (InstanceHandle, OwnerToken, error)
	InstanceAppend(context.Context, InstanceHandle, OwnerToken, Event) error
	RetireInstance(context.Context, InstanceHandle, OwnerToken) error
}

// ErrRunOwned reports an attempt to open a second writer for one logical run.
var ErrRunOwned = errors.New("journal: run already has a writer")

// FileBackendConfig locates local journals without exposing paths to callers.
type FileBackendConfig struct {
	RunsDir        string
	ResolveRunsDir func(string) (string, bool)
	SchedulerDir   string
}

type fileRunOwner struct {
	run   *Run
	owner OwnerToken
	key   RunKey
}

type fileInstanceOwner struct {
	append InstanceAppender
	log    *InstanceLog
	owner  OwnerToken
}

type fileBackend struct {
	config    FileBackendConfig
	mu        sync.Mutex
	runs      map[RunHandle]fileRunOwner
	live      map[RunKey]RunHandle
	instances map[InstanceHandle]fileInstanceOwner
}

// NewFileBackend constructs a local backend.
func NewFileBackend(config ...FileBackendConfig) Backend {
	var cfg FileBackendConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	return &fileBackend{config: cfg, runs: map[RunHandle]fileRunOwner{}, live: map[RunKey]RunHandle{}, instances: map[InstanceHandle]fileInstanceOwner{}}
}

func (b *fileBackend) CreateRun(_ context.Context, request CreateRunRequest) (RunHandle, OwnerToken, error) {
	root, err := b.runsDir(request.Identity.Gaggle)
	if err != nil {
		return "", "", err
	}
	run, err := Create(root, request.Identity, request.Inputs, fileRunOptions(request.Options)...)
	if err != nil {
		return "", "", err
	}
	return b.keepRun(run, RunKey{Gaggle: request.Identity.Gaggle, RunID: request.Identity.RunID})
}

func (b *fileBackend) RecoverRun(ctx context.Context, key RunKey, options RunWriterOptions) (RunHandle, OwnerToken, RecoverReport, error) {
	return b.recoverRun(ctx, key, options)
}

func (b *fileBackend) recoverRun(ctx context.Context, key RunKey, options RunWriterOptions) (RunHandle, OwnerToken, RecoverReport, error) {
	b.mu.Lock()
	_, owned := b.live[key]
	b.mu.Unlock()
	if owned {
		return "", "", RecoverReport{}, ErrRunOwned
	}
	root, err := b.runsDir(key.Gaggle)
	if err != nil {
		return "", "", RecoverReport{}, err
	}
	dir := filepath.Join(root, key.RunID)
	deadline := time.Now().Add(journalLockTimeout)
	for {
		run, report, err := TryRecover(dir, fileRunOptions(options)...)
		if err == nil {
			handle, owner, keepErr := b.keepRun(run, key)
			return handle, owner, report, keepErr
		}
		if !errors.Is(err, ErrRecoveryBusy) {
			return "", "", report, err
		}
		wait := min(journalLockPollInterval, time.Until(deadline))
		if wait <= 0 {
			return "", "", report, errors.Join(ErrRunOwned, ErrLockTimeout)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", "", report, errors.Join(ErrRunOwned, ctx.Err())
		case <-timer.C:
		}
	}
}

func (b *fileBackend) RunStatus(_ context.Context, handle RunHandle) (RunStatus, error) {
	run, _, err := b.readRun(handle)
	if err != nil {
		return RunStatus{}, err
	}
	return RunStatus{Seq: run.Seq(), Phase: run.Phase(), Closed: run.Closed()}, nil
}

func (b *fileBackend) AppendBatch(_ context.Context, handle RunHandle, owner OwnerToken, events []Event) (int, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return 0, err
	}
	return run.appendBatch(events)
}

func (b *fileBackend) AppendIfAbsent(_ context.Context, handle RunHandle, owner OwnerToken, event Event, match func(Event) bool) (bool, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return false, err
	}
	return run.AppendIfAbsent(event, match)
}

func (b *fileBackend) ClaimNotificationDelivery(_ context.Context, handle RunHandle, owner OwnerToken, receipt apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return nil, err
	}
	return run.ClaimNotificationDelivery(receipt)
}

func (b *fileBackend) Checkpoint(_ context.Context, handle RunHandle, owner OwnerToken) error {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return err
	}
	return run.Checkpoint()
}

func (b *fileBackend) RecordBlob(_ context.Context, handle RunHandle, owner OwnerToken, record BlobRecord) (Ref, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return Ref{}, err
	}

	if record.Event.Integrity == "" {
		return Ref{}, errors.New("journal: blob integrity is required")
	}
	if record.OutboxRelPath != "" || record.OutboxOccurrence != 0 {
		if record.Expected == nil {
			return Ref{}, errors.New("journal: outbox blob requires an expected reference")
		}
		return run.RecordBranchExpectedOutboxFileAnnotated(
			record.Event.Branch, record.Event.Stage, record.Event.Attempt, record.Event.AttemptClass,
			record.OutboxOccurrence, record.OutboxRelPath, record.Data, *record.Expected, record.Event.Runner,
		)
	}
	if record.Event.Stage == "" && record.Event.Branch == 0 {
		record.Event.Attempt = 0
		record.Event.AttemptClass = ""
	}
	return run.recordExpectedArtifact(record.Event, record.Data, record.MaxBytes, record.Expected)
}

func (b *fileBackend) ReadBlob(_ context.Context, handle RunHandle, ref Ref) ([]byte, error) {
	run, _, err := b.readRun(handle)
	if err != nil {
		return nil, err
	}
	reader, err := OpenReadOnly(run.Dir())
	if err != nil {
		return nil, err
	}
	return reader.ArtifactBytes(ref)
}

func (b *fileBackend) RecordSpan(_ context.Context, handle RunHandle, owner OwnerToken, record SpanRecord) (Ref, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return Ref{}, err
	}
	return run.RecordBranchSpanAnnotated(record.Branch, record.Stage, record.Name, record.DataSchema, record.Data, record.Annotations)
}

func (b *fileBackend) AppendChildStageStarted(_ context.Context, handle RunHandle, owner OwnerToken, record ChildStageStartRecord) (uint64, *apiv1.ChildWorkflowOrigin, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return 0, nil, err
	}
	return run.AppendChildStageStarted(record.Event, record.Continuation)
}

func (b *fileBackend) BeginTranscript(_ context.Context, handle RunHandle, owner OwnerToken, branch int, stage, name string) (ExpectedTranscriptCheckpointSession, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return nil, err
	}
	return run.BeginBranchTranscriptCapture(branch, stage, name)
}

func (b *fileBackend) RepairAppendBoundary(_ context.Context, handle RunHandle, owner OwnerToken) error {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return err
	}
	return run.RepairAppendBoundary()
}

func (b *fileBackend) AcceptOperatorMessage(_ context.Context, handle RunHandle, owner OwnerToken, request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	return run.AcceptOperatorMessage(request)
}

func (b *fileBackend) RejectOperatorMessage(_ context.Context, handle RunHandle, owner OwnerToken, request apiv1.OperatorMessageRequest, code, detail string) (apiv1.OperatorMessageRecord, bool, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	return run.RejectOperatorMessage(request, code, detail)
}

func (b *fileBackend) AcknowledgeOperatorMessage(_ context.Context, handle RunHandle, owner OwnerToken, ack apiv1.OperatorMessageAcknowledgement) (apiv1.OperatorMessageRecord, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	return run.AcknowledgeOperatorMessage(ack)
}

func (b *fileBackend) CompleteOperatorMessage(_ context.Context, handle RunHandle, owner OwnerToken, outcome apiv1.OperatorMessageOutcome) (apiv1.OperatorMessageRecord, error) {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, err
	}
	return run.CompleteOperatorMessage(outcome)
}

func (b *fileBackend) Retire(_ context.Context, handle RunHandle, owner OwnerToken) error {
	run, _, err := b.writeRun(handle, owner)
	if err != nil {
		return err
	}
	if err := run.Close(); err != nil {
		return err
	}
	b.mu.Lock()
	current := b.runs[handle]
	delete(b.live, current.key)
	delete(b.runs, handle)
	b.mu.Unlock()
	return nil
}

func (b *fileBackend) OpenInstance(_ context.Context) (InstanceHandle, OwnerToken, RecoverReport, error) {
	if b.config.SchedulerDir == "" {
		return "", "", RecoverReport{}, errors.New("journal: instance journal location is unavailable")
	}
	log, report, err := OpenInstanceLog(b.config.SchedulerDir)
	if err != nil {
		return "", "", report, err
	}
	handle, owner := newInstanceIdentity()
	b.mu.Lock()
	b.instances[handle] = fileInstanceOwner{append: log, log: log, owner: owner}
	b.mu.Unlock()
	return handle, owner, report, nil
}

func (b *fileBackend) BindInstance(appender InstanceAppender) (InstanceHandle, OwnerToken, error) {
	if appender == nil {
		return "", "", errors.New("journal: instance appender is required")
	}
	handle, owner := newInstanceIdentity()
	b.mu.Lock()
	b.instances[handle] = fileInstanceOwner{append: appender, owner: owner}
	b.mu.Unlock()
	return handle, owner, nil
}

func (b *fileBackend) InstanceAppend(_ context.Context, handle InstanceHandle, owner OwnerToken, event Event) error {
	b.mu.Lock()
	current, ok := b.instances[handle]
	b.mu.Unlock()
	if !ok || current.owner != owner {
		return errors.New("journal: stale or invalid instance owner token")
	}
	return current.append.Append(event)
}

func (b *fileBackend) RetireInstance(_ context.Context, handle InstanceHandle, owner OwnerToken) error {
	b.mu.Lock()
	current, ok := b.instances[handle]
	if !ok || current.owner != owner {
		b.mu.Unlock()
		return errors.New("journal: stale or invalid instance owner token")
	}
	delete(b.instances, handle)
	b.mu.Unlock()
	if current.log != nil {
		return current.log.Close()
	}
	return nil
}

func (b *fileBackend) keepRun(run *Run, key RunKey) (RunHandle, OwnerToken, error) {
	handle := RunHandle(rand.Text())
	owner := OwnerToken(rand.Text())
	b.mu.Lock()
	b.runs[handle] = fileRunOwner{run: run, owner: owner, key: key}
	b.live[key] = handle
	b.mu.Unlock()
	return handle, owner, nil
}

func (b *fileBackend) readRun(handle RunHandle) (*Run, OwnerToken, error) {
	b.mu.Lock()
	current, ok := b.runs[handle]
	b.mu.Unlock()
	if !ok {
		return nil, "", fmtClosed()
	}
	if current.run.Closed() {
		return nil, "", fmtClosed()
	}
	return current.run, current.owner, nil
}

func (b *fileBackend) writeRun(handle RunHandle, owner OwnerToken) (*Run, OwnerToken, error) {
	if owner == "" {
		return nil, "", errors.New("journal: owner token is required")
	}
	run, current, err := b.readRun(handle)
	if err != nil {
		return nil, "", err
	}
	if current != owner {
		return nil, "", errors.New("journal: stale or invalid owner token")
	}
	return run, current, nil
}

func (b *fileBackend) runsDir(gaggle string) (string, error) {
	if b.config.ResolveRunsDir != nil {
		if dir, ok := b.config.ResolveRunsDir(gaggle); ok && dir != "" {
			return dir, nil
		}
	}
	if b.config.RunsDir != "" {
		return b.config.RunsDir, nil
	}
	return "", errors.New("journal: runs location is unavailable")
}

func fileRunOptions(options RunWriterOptions) []Option {
	var result []Option
	if options.Clock != nil {
		result = append(result, WithClock(options.Clock))
	}
	if options.Scrubber != nil {
		result = append(result, WithScrubber(options.Scrubber))
	}
	if options.InputIntegrity != nil {
		result = append(result, WithInputIntegrity(options.InputIntegrity))
	}
	if options.InputSource != nil {
		result = append(result, WithInputSource(options.InputSource))
	}
	if options.ContextObserver != nil {
		ctx := options.ObserverContext
		if ctx == nil {
			ctx = context.Background()
		}
		result = append(result, WithAsyncAppendObserver(ctx, options.ContextObserver))
	} else if options.AppendObserver != nil {
		result = append(result, WithAppendObserver(options.AppendObserver))
	}
	return result
}

func newInstanceIdentity() (InstanceHandle, OwnerToken) {
	return InstanceHandle(rand.Text()), OwnerToken(rand.Text())
}

func fmtClosed() error {
	return errors.Join(errors.New("journal: backend handle is closed"), ErrClosed)
}

var _ Backend = (*fileBackend)(nil)
