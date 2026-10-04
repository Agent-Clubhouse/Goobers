package eventexecution

import (
	"bytes"
	"context"
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Observe recognizes only the reserved run with the complete accepted event
// identity. Missing or mismatched evidence never authorizes another run.
func (s *Service) Observe(ctx context.Context, record triggerqueue.Record) (bool, string, error) {
	start, err := eventing.ParseStart(record.Payload)
	if err != nil {
		return false, "", err
	}
	verified, expected, err := s.Queue.VerifiedEventStart(ctx, start.Gaggle, start.GroupID)
	if err != nil {
		return false, "", err
	}
	if verified.ID != record.ID || !bytes.Equal(verified.Payload, record.Payload) || expected != start {
		return false, "", triggerqueue.ErrConflict
	}
	runID := strings.TrimPrefix(record.ID, "trigger-")
	if record.RunID != "" && record.RunID != runID {
		return false, "", triggerqueue.ErrConflict
	}
	if s.RunDirectory == nil {
		return false, "", errors.New("eventexecution: journal observer unavailable")
	}
	dir, err := s.RunDirectory(ctx, runID)
	if err != nil || dir == "" {
		return false, "", err
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return false, "", err
	}
	id, err := rd.Identity()
	if err != nil {
		return false, "", err
	}
	if err = VerifyIdentity(rd, id, verified, start); err != nil {
		return false, "", err
	}
	if err = s.VerifyMembership(ctx, rd, id, start); err != nil {
		return false, "", err
	}
	outcome, err := s.terminalOutcome(ctx, rd, id)
	return true, outcome, err
}

// VerifyIdentity also guards recovery: accepted source pins, manifest bytes and
// journal-owned inputs must agree before a retained runner can resume.
func VerifyIdentity(rd *journal.Reader, id journal.RunIdentity, record triggerqueue.Record, start eventing.StartEnvelope) error {
	if err := verifyExecutionPins(id, record, start); err != nil {
		return err
	}
	e := id.Event
	raw, err := inputBytes(rd, id, eventing.ManifestInputName)
	if err != nil {
		return err
	}
	m, err := eventing.ParseInputManifest(raw)
	if err != nil {
		return err
	}
	if journal.Digest(raw) != e.ManifestDigest || m.Start != start || m.AcceptanceID != record.ID || m.Actor != record.Actor {
		return errors.New("eventexecution: journal manifest differs")
	}
	bundle := eventing.ExecutionInputs{Manifest: m, Envelopes: map[string][]byte{}}
	for i, member := range m.Members {
		if !member.Selected {
			continue
		}
		payload, err := inputBytes(rd, id, eventing.EventInputName(i))
		if err != nil {
			return err
		}
		bundle.Envelopes[member.ReceiptID] = payload
	}
	canonical, err := bundle.Validate(id.RunID, id.Gaggle)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, raw) {
		return errors.New("eventexecution: noncanonical retained manifest")
	}
	return nil
}

func inputBytes(rd *journal.Reader, id journal.RunIdentity, name string) ([]byte, error) {
	for _, input := range id.Inputs {
		if input.Name == name {
			grade := apiv1.IntegrityUnapproved
			if name == eventing.ManifestInputName {
				grade = apiv1.IntegrityTrusted
			}
			if input.Integrity != grade || input.Ref.Integrity != grade {
				return nil, errors.New("eventexecution: retained input integrity differs")
			}
			return rd.ArtifactBytes(input.Ref)
		}
	}
	return nil, errors.New("eventexecution: retained input missing")
}

// VerifyMembership confirms retained producer provenance against durable receipt custody.
func (s *Service) VerifyMembership(ctx context.Context, rd *journal.Reader, id journal.RunIdentity, start eventing.StartEnvelope) error {
	raw, err := inputBytes(rd, id, eventing.ManifestInputName)
	if err != nil {
		return err
	}
	m, err := eventing.ParseInputManifest(raw)
	if err != nil {
		return err
	}
	var after int64
	index := 0
	for {
		page, err := s.Queue.EventMembers(ctx, id.Gaggle, start.GroupID, after, 100)
		if err != nil {
			return err
		}
		for _, member := range page {
			if index >= len(m.Members) {
				return errors.New("eventexecution: journal omitted event member")
			}
			want := m.Members[index]
			if member.ReceiptID != want.ReceiptID || member.Sequence != want.Sequence || member.Digest != want.Digest || member.Selected != want.Selected || member.Producer != want.Producer {
				return errors.New("eventexecution: retained membership differs")
			}
			index++
			after = member.Sequence
		}
		if len(page) < 100 {
			break
		}
	}
	if index != len(m.Members) {
		return errors.New("eventexecution: journal contains foreign event member")
	}
	return nil
}

func (s *Service) terminalOutcome(ctx context.Context, rd *journal.Reader, id journal.RunIdentity) (string, error) {
	if s.AcquireTerminal == nil {
		return "", nil
	}
	release, ok := s.AcquireTerminal(id.RunID)
	if !ok {
		return "", nil
	}
	defer release()
	phase, err := rd.PhaseBounded(ctx)
	if err != nil {
		return "", err
	}
	var outcome string
	switch phase {
	case journal.PhaseCompleted:
		outcome = "completed"
	case journal.PhaseFailed:
		outcome = "failed"
	case journal.PhaseAborted:
		outcome = "cancelled"
	default:
		return "", nil
	}
	events, err := rd.Events()
	if err != nil {
		return "", err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			if events[i].Status != string(phase) || events[i].Time.IsZero() {
				return "", errors.New("eventexecution: terminal event differs from phase")
			}
			return outcome, nil
		}
	}
	return "", nil
}

func verifyExecutionPins(id journal.RunIdentity, record triggerqueue.Record, start eventing.StartEnvelope) error {
	e := id.Event
	if e == nil || id.RunID != strings.TrimPrefix(record.ID, "trigger-") || id.Gaggle != start.Gaggle || id.Workflow != start.Workflow || id.WorkflowDigest != start.WorkflowDigest || id.GooberDigest != start.GooberDigest || id.ConfigGeneration != start.ConfigGeneration || id.Driver != "" {
		return errors.New("eventexecution: journal execution pins differ")
	}
	if e.GroupID != start.GroupID || e.Consumer != start.Consumer || e.Revision != start.Revision || e.AcceptanceID != record.ID || e.EnvelopeDigest != journal.Digest(record.Payload) {
		return errors.New("eventexecution: journal event provenance differs")
	}
	return nil
}
