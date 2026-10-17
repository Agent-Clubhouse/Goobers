package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

const (
	attributionRecordBytes   = 8 << 20
	attributionInputBytes    = 64 << 20
	attributionMetadataBytes = 1 << 20
	attributionMaxItems      = 10000
)

var errAttributionLimit = errors.New("attribution read limit exceeded")

// Match the existing runtime path-component contract, including spaces.
func validAttributionGaggle(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00")
}

// RunAttributionReader is a pure, bounded read of one persisted analysis.
type RunAttributionReader interface {
	RunAttribution(context.Context, string, string) (RunAttributionResult, error)
}

// RunAttributionResult keeps recorded analysis and current evidence availability separate.
// Evidence checks index the unchanged record's evidence array, never substitute references.
type RunAttributionResult struct {
	Schema        string                        `json:"schema"`
	RunID         string                        `json:"runId"`
	Gaggle        string                        `json:"gaggle"`
	ObservedAt    time.Time                     `json:"observedAt"`
	Availability  string                        `json:"availability"`
	RunPhase      journal.RunPhase              `json:"runPhase,omitempty"`
	Record        *creditgraph.RunRecord        `json:"record,omitempty"`
	GroundTruth   *creditgraph.GroundTruthLabel `json:"groundTruth,omitempty"`
	LabelRevision string                        `json:"labelRevision,omitempty"`
	SourceDigest  string                        `json:"sourceDigest,omitempty"`
	Evidence      []AttributionEvidenceCheck    `json:"evidence"`
	InputBytes    int64                         `json:"inputBytes"`
}

// AttributionEvidenceCheck distinguishes spans from artifact-route content.
type AttributionEvidenceCheck struct {
	Index  int    `json:"index"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// RunAttribution neither scans other runs nor computes attribution or audit state.
func (s *Local) RunAttribution(ctx context.Context, runID, gaggle string) (RunAttributionResult, error) {
	out := RunAttributionResult{Schema: "goobers.dev/backprop/read/v1", RunID: runID, Gaggle: gaggle, ObservedAt: time.Now().UTC(), Evidence: []AttributionEvidenceCheck{}}
	if !apiv1.ValidRunID(runID) || !validAttributionGaggle(gaggle) {
		return out, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	instanceRoot, err := os.OpenRoot(s.sources.Layout.Root)
	if err != nil {
		return out, err
	}
	defer func() { _ = instanceRoot.Close() }()
	runRoot, err := attributionRunRoot(instanceRoot, s.sources.Layout, runID, gaggle)
	if err != nil {
		return out, err
	}
	defer func() { _ = runRoot.Close() }()
	budget := &attributionReadBudget{ctx: ctx, root: runRoot, remaining: attributionInputBytes}
	err = loadRunAttribution(budget, &out)
	out.InputBytes = attributionInputBytes - budget.remaining
	if errors.Is(err, errAttributionLimit) || errors.Is(err, journal.ErrReadSnapshotLimit) {
		out.Availability = "limit-exceeded"
		out.Record = nil
		out.Evidence = []AttributionEvidenceCheck{}
		out.GroundTruth = nil
		return out, nil
	}
	if errors.Is(err, journal.ErrJournalMigrationRequired) {
		out.Availability = "unsupported"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	data, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	// The HTTP JSON encoder appends a newline.
	if len(data)+1 > attributionRecordBytes {
		out.Availability = "limit-exceeded"
		out.Record = nil
		out.Evidence = []AttributionEvidenceCheck{}
		out.GroundTruth = nil
	}
	return out, ctx.Err()
}

func attributionRunRoot(root *os.Root, layout instance.Layout, runID, gaggle string) (*os.Root, error) {
	legacy, err := attributionLegacyRunsDir(root, layout, gaggle)
	if err != nil {
		return nil, err
	}
	scoped, err := filepath.Rel(layout.Root, layout.ForGaggle(gaggle).RunsDir())
	if err != nil {
		return nil, err
	}
	candidates := []string{scoped}
	if legacy != "" {
		candidates = append(candidates, legacy)
	}
	var found *os.Root
	var foundInfo os.FileInfo
	defer func() {
		if found != nil {
			_ = found.Close()
		}
	}()
	for _, candidate := range candidates {
		runs, _, err := openAttributionDirectory(root, candidate)
		if err != nil {
			return nil, err
		}
		if runs == nil {
			continue
		}
		run, info, err := openAttributionDirectory(runs, runID)
		_ = runs.Close()
		if err != nil {
			return nil, err
		}
		if run == nil {
			continue
		}
		if found != nil {
			_ = run.Close()
			if !os.SameFile(foundInfo, info) {
				return nil, fmt.Errorf("%w: ambiguous attribution run", ErrArtifactIntegrity)
			}
			continue
		}
		found = run
		foundInfo = info
	}
	if found == nil {
		return nil, ErrNotFound
	}
	selected := found
	found = nil // Transfer the retained handle to the caller.
	return selected, nil
}

// Pin an ordinary directory and compare the opened identity with the no-follow
// observation. A missing candidate stays absent; later alias replacement cannot
// redirect a retained handle or turn absence into an authorized candidate.
func openAttributionDirectory(root *os.Root, name string) (*os.Root, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrArtifactIntegrity, err.Error())
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, ErrArtifactIntegrity
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrArtifactIntegrity, err.Error())
	}
	after, err := dir.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = dir.Close()
		return nil, nil, ErrArtifactIntegrity
	}
	return dir, after, nil
}

// Read the compatibility link itself without traversing it. Windows junctions
// store absolute targets, which os.Root correctly refuses to follow. Convert a
// contained target to a relative path and keep every subsequent lookup rooted;
// replacing the alias after this read cannot redirect a later open.
func attributionLegacyRunsDir(root *os.Root, layout instance.Layout, gaggle string) (string, error) {
	legacy, err := filepath.Rel(layout.Root, instance.NewLayout(layout.Root).RunsDir())
	if err != nil {
		return "", err
	}
	target, err := root.Readlink(legacy)
	if err != nil {
		// Ordinary directories and missing paths are handled by the rooted
		// candidate lookup; any other failure still fails closed there.
		return legacy, nil
	}
	if filepath.IsAbs(target) {
		base, err := filepath.Abs(layout.Root)
		if err != nil {
			return "", err
		}
		target, err = filepath.Rel(base, target)
		if err != nil {
			return "", ErrArtifactIntegrity
		}
	} else {
		target = filepath.Join(filepath.Dir(legacy), target)
	}
	if !filepath.IsLocal(target) {
		return "", ErrArtifactIntegrity
	}
	parts := strings.Split(filepath.Clean(target), string(filepath.Separator))
	if len(parts) != 3 || parts[0] != instance.GagglesDirName || !validAttributionGaggle(parts[1]) || parts[2] != instance.RunsDirName {
		return "", ErrArtifactIntegrity
	}
	if parts[1] != gaggle {
		// The original gaggle's generated alias can survive adding another
		// gaggle. It is not a candidate for the requested scoped run.
		return "", nil
	}
	return target, nil
}

func loadRunAttribution(b *attributionReadBudget, out *RunAttributionResult) error {
	id, bySeq, err := loadAttributionJournal(b, out)
	if err != nil {
		return err
	}
	raw, err := b.read(creditgraph.RecordFileName, attributionRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		out.Availability = "absent"
		if !out.RunPhase.IsTerminal() {
			out.Availability = "pending"
		}
		return nil
	}
	if err != nil {
		return err
	}
	record, err := creditgraph.DecodeRunRecord(raw)
	if err != nil {
		return err
	}
	if record.RunID != id.RunID || record.Gaggle != id.Gaggle || record.Workflow != id.Workflow || record.WorkflowVersion != id.WorkflowVersion || record.WorkflowDigest != id.WorkflowDigest || (record.Attribution.RunID != "" && record.Attribution.RunID != id.RunID) {
		return ErrArtifactIntegrity
	}
	switch record.Status {
	case creditgraph.RecordComplete, creditgraph.RecordInsufficientEvidence, creditgraph.RecordFailed:
	default:
		return ErrArtifactIntegrity
	}
	if len(record.Attribution.Contributions) > attributionMaxItems || len(record.Evidence) > attributionMaxItems {
		return errAttributionLimit
	}
	out.Availability = "recorded"
	out.Record = &record
	out.SourceDigest = journal.Digest(raw)
	if err := loadAttributionLabel(b, out); err != nil {
		return err
	}
	return verifyAttributionEvidence(b, out, bySeq)
}

func loadAttributionLabel(b *attributionReadBudget, out *RunAttributionResult) error {
	labels, err := b.read(creditgraph.LabelStoreFileName, attributionMetadataBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		store, err := creditgraph.DecodeLabelStore(labels)
		if err != nil {
			return err
		}
		if store.RunID != out.RunID || len(store.Labels) > 256 {
			return ErrArtifactIntegrity
		}
		for _, label := range store.Labels {
			if label.RunID != out.RunID {
				return ErrArtifactIntegrity
			}
		}
		out.LabelRevision = journal.Digest(labels)
		if label, ok := creditgraph.EffectiveLabel(store.Labels); ok {
			out.GroundTruth = &label
		}
	}
	return nil
}

func verifyAttributionEvidence(b *attributionReadBudget, out *RunAttributionResult, bySeq map[int64]journal.Event) error {
	record := out.Record
	checked := map[string]string{}
	redactions := attributionRedactions(bySeq)
	for i, link := range record.Evidence {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		event, ok := bySeq[link.JournalSequence]
		if !ok || link.JournalSequence <= 0 || link.RunID != out.RunID || link.JournalPath != "events.jsonl" {
			return ErrArtifactIntegrity
		}
		check := AttributionEvidenceCheck{Index: i, Kind: "journal", Status: "verified"}
		if link.ArtifactDigest != "" || link.ArtifactPath != "" {
			if event.Ref == nil || link.ArtifactDigest != event.Ref.Digest || link.ArtifactPath != event.Ref.Path || link.ArtifactMediaType != event.Ref.MediaType {
				return ErrArtifactIntegrity
			}
			switch event.Type {
			case journal.EventArtifactRecorded:
				check.Kind = "artifact"
			case journal.EventSpanRecorded:
				check.Kind = "span"
			default:
				return ErrArtifactIntegrity
			}
			key := link.ArtifactPath + "\x00" + link.ArtifactDigest
			if seq, redacted := redactions[key]; redacted && seq > event.Seq {
				check.Status = "redacted"
				out.Evidence = append(out.Evidence, check)
				continue
			}
			status, ok := checked[key]
			if !ok {
				data, err := b.read(link.ArtifactPath, attributionInputBytes)
				switch {
				case errors.Is(err, os.ErrNotExist):
					status = "unavailable"
				case err != nil:
					return err
				default:
					if int64(len(data)) != event.Ref.Size || journal.Digest(data) != link.ArtifactDigest {
						return ErrArtifactIntegrity
					}
					status = "verified"
				}
				checked[key] = status
			}
			check.Status = status
		}
		out.Evidence = append(out.Evidence, check)
	}
	return nil
}

func loadAttributionJournal(b *attributionReadBudget, out *RunAttributionResult) (journal.RunIdentity, map[int64]journal.Event, error) {
	manifest, err := b.read("schema.json", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return journal.RunIdentity{}, nil, journal.ErrJournalMigrationRequired
	}
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	identity, err := b.read("run.yaml", attributionMetadataBytes)
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	log, err := b.read("events.jsonl", attributionInputBytes)
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	id, records, err := journal.DecodeReadSnapshot(b.ctx, manifest, identity, log, 100000)
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	if id.RunID != out.RunID || id.Gaggle != out.Gaggle {
		return journal.RunIdentity{}, nil, fmt.Errorf("%w: attribution journal identity mismatch", ErrArtifactIntegrity)
	}
	if len(records) > 100000 {
		return journal.RunIdentity{}, nil, errAttributionLimit
	}
	events := make([]journal.Event, len(records))
	bySeq := make(map[int64]journal.Event, len(records))
	for i, r := range records {
		events[i] = r.Event
		if _, ok := bySeq[int64(r.Event.Seq)]; ok {
			return journal.RunIdentity{}, nil, ErrArtifactIntegrity
		}
		bySeq[int64(r.Event.Seq)] = r.Event
	}
	out.RunPhase = journal.PhaseFromEvents(events)
	return id, bySeq, nil
}

// A committed redaction is evidence that the original bytes are no longer
// available. Never substitute the replacement digest into the stored analysis.
func attributionRedactions(events map[int64]journal.Event) map[string]uint64 {
	out := map[string]uint64{}
	for _, event := range events {
		r := event.Redaction
		if event.Type == journal.EventRedaction && r != nil && event.Ref != nil && r.OldDigest != "" && r.NewDigest != r.OldDigest && event.Ref.Digest == r.NewDigest && event.Name == r.Target {
			key := r.Target + "\x00" + r.OldDigest
			out[key] = max(out[key], event.Seq)
		}
	}
	return out
}

type attributionReadBudget struct {
	ctx       context.Context
	root      *os.Root
	remaining int64
}

func (b *attributionReadBudget) read(name string, maximum int64) ([]byte, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	if filepath.IsAbs(name) || strings.Contains(name, "\\") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return nil, ErrArtifactIntegrity
	}
	f, err := safeopen.OpenRegularInRoot(b.root, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", ErrArtifactIntegrity, err.Error())
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrArtifactIntegrity
	}
	limit := min(maximum, b.remaining)
	if info.Size() > limit {
		return nil, errAttributionLimit
	}
	data, err := io.ReadAll(io.LimitReader(attributionContextReader{b.ctx, f}, limit+1))
	b.remaining -= int64(len(data))
	if int64(len(data)) > limit {
		return nil, errAttributionLimit
	}
	return data, err
}

type attributionContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r attributionContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
