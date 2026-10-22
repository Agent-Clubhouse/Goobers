package journal

import (
	"crypto/rand"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// OwnerToken fences writes to one open run incarnation.
type OwnerToken string

// BlobRecord describes a content-addressed artifact write and its journal event.
type BlobRecord struct {
	Event    Event
	Data     []byte
	MaxBytes int
	Expected *Ref
}

// Backend is the shared journal write contract. FileBackend preserves today's
// bytes, fsyncs, atomic renames, and lifetime flock ownership.
type Backend interface {
	CreateRun(string, RunIdentity, map[string][]byte, ...Option) (*Run, OwnerToken, error)
	RecoverRun(string, ...Option) (*Run, OwnerToken, RecoverReport, error)
	AppendBatch(*Run, OwnerToken, []Event) error
	AppendIfAbsent(*Run, OwnerToken, Event, func(Event) bool) (bool, error)
	ClaimNotificationDelivery(*Run, OwnerToken, apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error)
	Checkpoint(*Run, OwnerToken) error
	RecordBlob(*Run, OwnerToken, BlobRecord) (Ref, error)
	Retire(*Run, OwnerToken) error
	OpenInstance(string, ...Option) (*InstanceLog, RecoverReport, error)
	InstanceAppend(*InstanceLog, Event) error
}

// FileBackend is the POSIX/Windows filesystem implementation.
type FileBackend struct{}

// NewFileBackend returns the primary local backend.
func NewFileBackend() Backend { return FileBackend{} }

// CreateRun creates and opens a file-backed run.
func (FileBackend) CreateRun(dir string, id RunIdentity, inputs map[string][]byte, opts ...Option) (*Run, OwnerToken, error) {
	run, err := Create(dir, id, inputs, opts...)
	if err != nil {
		return nil, "", err
	}
	run.backendOwner = OwnerToken(rand.Text())
	return run, fileOwner(run), nil
}

// RecoverRun reopens a file-backed run under its lifetime lock.
func (FileBackend) RecoverRun(dir string, opts ...Option) (*Run, OwnerToken, RecoverReport, error) {
	run, report, err := Recover(dir, opts...)
	if err != nil {
		return nil, "", report, err
	}
	run.backendOwner = OwnerToken(rand.Text())
	return run, fileOwner(run), report, nil
}

// AppendBatch appends a batch through the current owner.
func (FileBackend) AppendBatch(run *Run, owner OwnerToken, events []Event) error {
	if err := requireFileOwner(run, owner); err != nil {
		return err
	}
	return run.AppendBatch(events)
}

// AppendIfAbsent atomically checks and appends through the current owner.
func (FileBackend) AppendIfAbsent(run *Run, owner OwnerToken, event Event, match func(Event) bool) (bool, error) {
	if err := requireFileOwner(run, owner); err != nil {
		return false, err
	}
	return run.AppendIfAbsent(event, match)
}

// ClaimNotificationDelivery reserves one notification delivery attempt.
func (FileBackend) ClaimNotificationDelivery(run *Run, owner OwnerToken, receipt apiv1.NotificationReceipt) (*apiv1.NotificationReceipt, error) {
	if err := requireFileOwner(run, owner); err != nil {
		return nil, err
	}
	return run.ClaimNotificationDelivery(receipt)
}

// Checkpoint persists the current run checkpoint.
func (FileBackend) Checkpoint(run *Run, owner OwnerToken) error {
	if err := requireFileOwner(run, owner); err != nil {
		return err
	}
	return run.Checkpoint()
}

// RecordBlob records content-addressed bytes and their event.
func (FileBackend) RecordBlob(run *Run, owner OwnerToken, record BlobRecord) (Ref, error) {
	if err := requireFileOwner(run, owner); err != nil {
		return Ref{}, err
	}
	if record.Event.Integrity == "" {
		record.Event.Integrity = apiv1.IntegrityDerived
	}
	return run.recordExpectedArtifact(record.Event, record.Data, record.MaxBytes, record.Expected)
}

// Retire closes the current run owner.
func (FileBackend) Retire(run *Run, owner OwnerToken) error {
	if err := requireFileOwner(run, owner); err != nil {
		return err
	}
	return run.Close()
}

// OpenInstance opens the file-backed instance journal.
func (FileBackend) OpenInstance(dir string, opts ...Option) (*InstanceLog, RecoverReport, error) {
	return OpenInstanceLog(dir, opts...)
}

// InstanceAppend appends one instance event.
func (FileBackend) InstanceAppend(log *InstanceLog, event Event) error {
	if log == nil {
		return errors.New("journal: instance log is required")
	}
	return log.Append(event)
}

func fileOwner(run *Run) OwnerToken {
	if run == nil {
		return ""
	}
	return run.backendOwner
}

func requireFileOwner(run *Run, owner OwnerToken) error {
	if run == nil || owner == "" || owner != fileOwner(run) || run.Closed() {
		return errors.New("journal: stale or invalid owner token")
	}
	return nil
}

var _ Backend = FileBackend{}
