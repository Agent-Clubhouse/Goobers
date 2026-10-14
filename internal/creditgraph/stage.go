package creditgraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
)

const (
	// StageRecordSchemaVersion identifies the persisted stage checkpoint set.
	StageRecordSchemaVersion = "goobers.dev/backprop/stage-records/v1"
	// StageRecordsFileName holds a stage-granular run's stage checkpoints,
	// beside attribution.json. Checkpoints are only ever appended.
	StageRecordsFileName = "attribution-stages.json"
	// StageSegmentsDirName holds each checkpoint's persisted journal segment,
	// content-addressed by the record's SegmentDigest. The end-of-run record
	// is composed from these segments, not re-read from the journal.
	StageSegmentsDirName = "attribution-segments"
	// stagePublishLockName serializes publishing stage checkpoints with
	// composing and publishing the end-of-run record, across processes.
	stagePublishLockName = "attribution.lock"
)

// Lock waits: a checkpoint pass holds the lock only to re-check and write
// small files; the end-of-run record waits longer so it is not dropped.
var (
	stagePublishLockWait = 2 * time.Second
	runRecordLockWait    = 30 * time.Second
)

// stagePublishHook, when set by tests, runs just before a checkpoint pass
// takes the publication lock ("before-lock") and once it holds the lock and
// has re-checked the run ("locked").
var stagePublishHook func(phase string)

// lockStagePublication takes runDir's publication lock, retrying while
// another holder has it, for up to wait.
func lockStagePublication(runDir string, wait time.Duration) (*lock.Handle, error) {
	path := filepath.Join(runDir, stagePublishLockName)
	deadline := time.Now().Add(wait)
	for {
		handle, err := lock.TryAcquire(path)
		if err == nil || !errors.Is(err, lock.ErrHeld) || !time.Now().Before(deadline) {
			return handle, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// StageRecordSet is the persisted, ordered list of a run's stage checkpoints.
type StageRecordSet struct {
	Schema  string        `json:"schema"`
	RunID   string        `json:"runId"`
	Records []StageRecord `json:"records"`
}

// StageRecord is the read-only attribution checkpoint written after one stage
// attempt finished. It covers the contiguous journal segment since the
// previous checkpoint, ending at the stage's stage.finished event; segments of
// consecutive records tile the journal from its first event with no gap or
// overlap. Contributions, Causes and Evidence are provisional: they are the
// stage's slice of an attribution over the journal prefix, computed before the
// run's outcome is known, so they carry the outcome gap's uncertainty. The
// end-of-run record, not a checkpoint, is authoritative.
type StageRecord struct {
	Stage           string                    `json:"stage"`
	Attempt         int                       `json:"attempt"`
	Branch          int                       `json:"branch,omitempty"`
	Status          string                    `json:"status,omitempty"`
	FromSequence    uint64                    `json:"fromSequence"`
	ThroughSequence uint64                    `json:"throughSequence"`
	EventCount      int                       `json:"eventCount"`
	SegmentDigest   string                    `json:"segmentDigest"`
	Inputs          []StageRef                `json:"inputs,omitempty"`
	Outputs         map[string]any            `json:"outputs,omitempty"`
	Artifacts       []StageRef                `json:"artifacts,omitempty"`
	Verdicts        []StageGateVerdict        `json:"verdicts,omitempty"`
	Contributions   []Contribution            `json:"contributions,omitempty"`
	Causes          []CauseFinding            `json:"causes,omitempty"`
	Evidence        []AttributionEvidenceLink `json:"evidence,omitempty"`
	// RecordDigest is the content digest of the record itself (with this
	// field empty), so an edited checkpoint is refused rather than shown.
	RecordDigest string `json:"recordDigest"`
}

// StageRef is one content reference a stage consumed or produced.
type StageRef struct {
	Name      string `json:"name,omitempty"`
	Digest    string `json:"digest"`
	Path      string `json:"path,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}

// StageGateVerdict is one gate decision recorded inside a stage segment.
type StageGateVerdict struct {
	Gate            string `json:"gate"`
	Verdict         string `json:"verdict"`
	Target          string `json:"target,omitempty"`
	JournalSequence uint64 `json:"journalSequence"`
}

// StageCheckpointResult reports one checkpoint pass. Settled means the run
// will never need another pass: it is not stage-granular or its checkpoints
// are unusable. A finished run is not settled, since it may be resumed.
type StageCheckpointResult struct {
	Written int
	Settled bool
}

// WriteStageCheckpoints appends a stage record for every stage.finished event
// not yet checkpointed. It only reads the journal; the run is never changed.
// Runs that are not enrolled with granularity stage, and runs that are
// finished and not reopened (their end-of-run record supersedes checkpoints),
// write nothing.
func WriteStageCheckpoints(runDir string) (StageCheckpointResult, error) {
	run, _, enrolled, err := loadRunJournal(runDir, nil)
	if err != nil {
		return StageCheckpointResult{Settled: true}, err
	}
	if !enrolled || run.config.Granularity != apiv1.BackpropGranularityStage {
		return StageCheckpointResult{Settled: true}, nil
	}
	if runFinished(run.events) {
		return StageCheckpointResult{}, nil
	}
	set, err := ReadStageRecords(runDir)
	if errors.Is(err, os.ErrNotExist) {
		set, err = StageRecordSet{Schema: StageRecordSchemaVersion, RunID: run.identity.RunID}, nil
	}
	if err != nil {
		return StageCheckpointResult{Settled: true}, err
	}
	// Most appends finish no stage: return before reading span content or
	// re-verifying, so a pass costs one journal read until there is work.
	if !finishesStageAfter(set, run.events) {
		return StageCheckpointResult{}, nil
	}
	if run.spanData, err = readSpanData(run.reader, run.events); err != nil {
		return StageCheckpointResult{}, err
	}
	cursor, err := verifyStageSegments(set, run)
	if err != nil {
		return StageCheckpointResult{Settled: true}, fmt.Errorf("existing stage checkpoints: %w", err)
	}
	written, segments := 0, map[string][]byte{}
	for i := cursor; i < len(run.events); i++ {
		if run.events[i].Type != journal.EventStageFinished {
			continue
		}
		record, recordErr := stageRecord(run, cursor, i)
		if recordErr != nil {
			return StageCheckpointResult{Settled: true}, recordErr
		}
		set.Records = append(set.Records, record)
		segments[record.SegmentDigest] = segmentBytes(run.events[cursor : i+1])
		cursor = i + 1
		written++
	}
	if written == 0 {
		return StageCheckpointResult{}, nil
	}
	return publishStageCheckpoints(runDir, set, written, segments)
}

// publishStageCheckpoints writes a pass's new segments and its extended
// record set, holding the publication lock.
func publishStageCheckpoints(runDir string, set StageRecordSet, written int, segments map[string][]byte) (StageCheckpointResult, error) {
	data, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return StageCheckpointResult{}, fmt.Errorf("encode stage checkpoints: %w", err)
	}
	data = append(data, '\n')
	if stagePublishHook != nil {
		stagePublishHook("before-lock")
	}
	// The end-of-run record is composed and published under the same lock,
	// after run.finished is durable. Re-checking under the lock makes
	// check-and-publish atomic with it: either these checkpoints land before
	// composition reads them, or the run is seen finished and the write is
	// dropped, so a late checkpoint never disagrees with attribution.json.
	handle, err := lockStagePublication(runDir, stagePublishLockWait)
	if err != nil {
		// The end-of-run record holds the lock while it composes; losing
		// the lock to it is the expected end of the run, not a failure.
		if latest, _, _, loadErr := loadRunJournal(runDir, nil); loadErr == nil && runFinished(latest.events) {
			return StageCheckpointResult{}, nil
		}
		return StageCheckpointResult{}, fmt.Errorf("lock stage checkpoints: %w", err)
	}
	defer func() { _ = handle.Release() }()
	if latest, _, _, err := loadRunJournal(runDir, nil); err != nil || runFinished(latest.events) {
		return StageCheckpointResult{}, nil
	}
	// Another writer may have published since this pass read the set; leave
	// the work to the next pass rather than overwrite its records.
	current, err := ReadStageRecords(runDir)
	if errors.Is(err, os.ErrNotExist) {
		current, err = StageRecordSet{}, nil
	}
	if err != nil || len(current.Records) != len(set.Records)-written {
		return StageCheckpointResult{}, nil
	}
	if stagePublishHook != nil {
		stagePublishHook("locked")
	}
	// Segments land before the record set that references them, so a crash
	// in between leaves at most an unreferenced segment.
	for digest, segment := range segments {
		if err := writeStageSegment(runDir, digest, segment); err != nil {
			return StageCheckpointResult{}, err
		}
	}
	if err := journal.WriteFileAtomic(filepath.Join(runDir, StageRecordsFileName), data, 0o600); err != nil {
		return StageCheckpointResult{}, fmt.Errorf("write stage checkpoints: %w", err)
	}
	return StageCheckpointResult{Written: written}, nil
}

// ReadStageRecords reads a run's persisted stage checkpoints.
func ReadStageRecords(runDir string) (StageRecordSet, error) {
	data, err := os.ReadFile(filepath.Join(runDir, StageRecordsFileName))
	if err != nil {
		return StageRecordSet{}, err
	}
	var set StageRecordSet
	if err := json.Unmarshal(data, &set); err != nil {
		return StageRecordSet{}, fmt.Errorf("decode stage checkpoints: %w", err)
	}
	if set.Schema != StageRecordSchemaVersion {
		return StageRecordSet{}, fmt.Errorf("unsupported stage checkpoint schema %q", set.Schema)
	}
	return set, nil
}

// ReadVerifiedStageRecords reads a run's stage checkpoints and verifies them
// against its journal, so read surfaces never present an edited or stale
// checkpoint as recorded fact.
func ReadVerifiedStageRecords(runDir string) (StageRecordSet, error) {
	set, err := ReadStageRecords(runDir)
	if err != nil {
		return StageRecordSet{}, err
	}
	run, _, enrolled, err := loadRun(runDir, nil)
	if err != nil {
		return StageRecordSet{}, err
	}
	if !enrolled {
		return StageRecordSet{}, errors.New("run is not enrolled in Backprop")
	}
	if _, err := verifyStageSegments(set, run); err != nil {
		return StageRecordSet{}, err
	}
	if _, err := composeStageRecords(runDir, set, nil); err != nil {
		return StageRecordSet{}, err
	}
	return set, nil
}

// finishesStageAfter reports whether the journal has a stage.finished event
// past the events the set's records cover.
func finishesStageAfter(set StageRecordSet, events []journal.Event) bool {
	covered := 0
	for _, record := range set.Records {
		covered += max(record.EventCount, 0)
	}
	for i := min(covered, len(events)); i < len(events); i++ {
		if events[i].Type == journal.EventStageFinished {
			return true
		}
	}
	return false
}

func stageSegmentPath(runDir, digest string) (string, error) {
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if _, err := hex.DecodeString(hexDigest); !ok || err != nil || len(hexDigest) != sha256.Size*2 {
		return "", fmt.Errorf("invalid stage segment digest %q", digest)
	}
	return filepath.Join(runDir, StageSegmentsDirName, hexDigest+".jsonl"), nil
}

func writeStageSegment(runDir, digest string, segment []byte) error {
	path, err := stageSegmentPath(runDir, digest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create stage segment directory: %w", err)
	}
	if err := journal.WriteFileAtomic(path, segment, 0o600); err != nil {
		return fmt.Errorf("write stage segment: %w", err)
	}
	return nil
}

// readStageSegment loads one record's persisted segment and proves it is the
// segment the record describes.
func readStageSegment(runDir string, record StageRecord) ([]journal.Event, error) {
	path, err := stageSegmentPath(runDir, record.SegmentDigest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read stage segment for %s#%d: %w", record.Stage, record.Attempt, err)
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != record.SegmentDigest {
		return nil, fmt.Errorf("stage segment for %s#%d does not match its digest", record.Stage, record.Attempt)
	}
	events := make([]journal.Event, 0, record.EventCount)
	decoder := json.NewDecoder(bytes.NewReader(data))
	for decoder.More() {
		var event journal.Event
		if err := decoder.Decode(&event); err != nil {
			return nil, fmt.Errorf("decode stage segment for %s#%d: %w", record.Stage, record.Attempt, err)
		}
		events = append(events, event)
	}
	if len(events) != record.EventCount {
		return nil, fmt.Errorf("stage segment for %s#%d holds %d events, record says %d", record.Stage, record.Attempt, len(events), record.EventCount)
	}
	return events, nil
}

// runFinished reports whether the run's latest run.finished has not been
// reopened by a resume, gate override, or stage rerun request.
func runFinished(events []journal.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case journal.EventRunResumed, journal.EventGateOverridden, journal.EventStageRerunRequested:
			return false
		case journal.EventRunFinished:
			return true
		}
	}
	return false
}

// stageRecord checkpoints the stage whose stage.finished event is at
// events[end], covering the segment events[start:end+1].
func stageRecord(run loadedRun, start, end int) (StageRecord, error) {
	record := stageJournalFacts(run.events, start, end)
	finished := run.events[end]
	prefix := run.events[:end+1]
	graph, err := Build(Input{
		RunID: run.identity.RunID, Gaggle: run.identity.Gaggle, Workflow: run.identity.Workflow,
		Events: prefix, SpanData: run.spanData,
	})
	if err != nil {
		return StageRecord{}, fmt.Errorf("build provisional credit graph for stage %s: %w", finished.Stage, err)
	}
	attribution := Attribute(graph)
	attempt := max(finished.Attempt, 1)
	for _, contribution := range attribution.Contributions {
		if contribution.Stage == finished.Stage && max(contribution.Attempt, 1) == attempt {
			record.Contributions = append(record.Contributions, contribution)
		}
	}
	for _, cause := range attribution.Causes {
		if cause.Stage == finished.Stage {
			record.Causes = append(record.Causes, cause)
		}
	}
	for _, link := range recordEvidence(prefix, graph, attribution) {
		if link.Stage == finished.Stage && link.JournalSequence >= int64(record.FromSequence) {
			record.Evidence = append(record.Evidence, link)
		}
	}
	record.RecordDigest = stageRecordDigest(record)
	return record, nil
}

// stageJournalFacts derives the journal-recorded facts of the stage whose
// stage.finished event is at events[end], covering events[start:end+1].
// They are deterministic in the segment, so verification re-derives them.
func stageJournalFacts(events []journal.Event, start, end int) StageRecord {
	segment := events[start : end+1]
	finished := events[end]
	record := StageRecord{
		Stage: finished.Stage, Attempt: finished.Attempt, Branch: finished.Branch, Status: finished.Status,
		FromSequence: segment[0].Seq, ThroughSequence: finished.Seq, EventCount: len(segment),
		SegmentDigest: segmentDigest(segment), Outputs: finished.Outputs,
	}
	outputs := map[string]bool{}
	for _, artifact := range finished.Artifacts {
		record.Artifacts = append(record.Artifacts, stageRef("", artifact))
		outputs[artifact.Digest] = true
	}
	for _, event := range segment {
		switch event.Type {
		case journal.EventInputSnapshot:
			if event.Ref != nil && (event.Stage == "" || event.Stage == finished.Stage) {
				record.Inputs = append(record.Inputs, stageRef(event.Name, *event.Ref))
			}
		case journal.EventArtifactRecorded:
			// Content the attempt recorded but did not declare as a result
			// artifact is what it consumed (context manifests, snapshots).
			if event.Ref != nil && event.Stage == finished.Stage && event.Attempt == finished.Attempt && !outputs[event.Ref.Digest] {
				record.Inputs = append(record.Inputs, stageRef(event.Name, *event.Ref))
			}
		case journal.EventGateEvaluated, journal.EventGateOverridden:
			record.Verdicts = append(record.Verdicts, StageGateVerdict{
				Gate: event.Gate, Verdict: event.Verdict, Target: event.Target, JournalSequence: event.Seq,
			})
		}
	}
	return record
}

// stageRecordDigest is the content digest of a record's canonical JSON with
// RecordDigest cleared.
func stageRecordDigest(record StageRecord) string {
	record.RecordDigest = ""
	data, err := json.Marshal(record)
	if err != nil {
		data = []byte(err.Error())
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sameStageFacts reports whether a persisted record's journal-derived facts
// match those re-derived from its segment.
func sameStageFacts(record, derived StageRecord) bool {
	derived.Contributions, derived.Causes, derived.Evidence = record.Contributions, record.Causes, record.Evidence
	derived.RecordDigest = record.RecordDigest
	return stageRecordDigest(derived) == stageRecordDigest(record)
}

func stageRef(name string, ref journal.Ref) StageRef {
	return StageRef{Name: name, Digest: ref.Digest, Path: ref.Path, MediaType: ref.MediaType}
}

// segmentDigest is the content digest of a journal segment, so a checkpoint
// can prove the exact events it covered.
func segmentDigest(events []journal.Event) string {
	sum := sha256.Sum256(segmentBytes(events))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// segmentBytes is a segment's persisted JSON Lines encoding.
func segmentBytes(events []journal.Event) []byte {
	var out []byte
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			// journal.Event always encodes; keep the encoding total rather
			// than silently skipping an event.
			data, _ = json.Marshal(err.Error())
		}
		out = append(append(out, data...), '\n')
	}
	return out
}

// verifyStageSegments checks that a checkpoint set belongs to this run and
// that its records tile events from the first event, in order, each ending at
// its stage's stage.finished and matching its digests and journal facts. It
// returns the position of the first uncheckpointed event.
// Each record is also rebuilt from its journal prefix and must match exactly,
// provisional attribution included, so a re-digested edit is refused.
func verifyStageSegments(set StageRecordSet, run loadedRun) (int, error) {
	events := run.events
	if set.RunID != run.identity.RunID {
		return 0, fmt.Errorf("stage checkpoints belong to run %q, not %q", set.RunID, run.identity.RunID)
	}
	cursor := 0
	for i, record := range set.Records {
		end := cursor + record.EventCount
		if record.EventCount < 1 || end > len(events) {
			return 0, fmt.Errorf("stage checkpoint %d (%s) covers %d events beyond the journal", i, record.Stage, record.EventCount)
		}
		segment := events[cursor:end]
		last := segment[len(segment)-1]
		switch {
		case segment[0].Seq != record.FromSequence || last.Seq != record.ThroughSequence:
			return 0, fmt.Errorf("stage checkpoint %d (%s) sequence range %d-%d does not match journal %d-%d",
				i, record.Stage, record.FromSequence, record.ThroughSequence, segment[0].Seq, last.Seq)
		case last.Type != journal.EventStageFinished || last.Stage != record.Stage || last.Attempt != record.Attempt:
			return 0, fmt.Errorf("stage checkpoint %d (%s#%d) does not end at its stage.finished event", i, record.Stage, record.Attempt)
		case segmentDigest(segment) != record.SegmentDigest:
			return 0, fmt.Errorf("stage checkpoint %d (%s) segment digest does not match the journal", i, record.Stage)
		case stageRecordDigest(record) != record.RecordDigest:
			return 0, fmt.Errorf("stage checkpoint %d (%s) record digest does not match its contents", i, record.Stage)
		case !sameStageFacts(record, stageJournalFacts(events, cursor, end-1)):
			return 0, fmt.Errorf("stage checkpoint %d (%s) facts do not match the journal", i, record.Stage)
		}
		derived, err := stageRecord(run, cursor, end-1)
		if err != nil {
			return 0, err
		}
		if derived.RecordDigest != record.RecordDigest {
			return 0, fmt.Errorf("stage checkpoint %d (%s) attribution does not match the journal", i, record.Stage)
		}
		cursor = end
	}
	return cursor, nil
}

// composeStageEvents builds the end-of-run analysis input from the persisted
// stage checkpoints. Once the set is verified against this run's journal, the
// checkpointed prefix is taken from the checkpoints' persisted segments, not
// from the journal; only the tail no checkpoint covers comes from the journal.
// Any unverifiable checkpoint or segment refuses composition: the caller then
// analyzes the whole journal and the record discloses the refusal, so a
// partial or stale checkpoint set never stands in for the run.
func composeStageEvents(runDir string, run loadedRun) ([]journal.Event, StageComposition) {
	events := run.events
	set, err := ReadStageRecords(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return events, StageComposition{}
	}
	if err != nil {
		return events, StageComposition{Error: err.Error()}
	}
	cursor, err := verifyStageSegments(set, run)
	if err != nil {
		return events, StageComposition{Error: err.Error()}
	}
	composed, err := composeStageRecords(runDir, set, events[cursor:])
	if err != nil {
		return events, StageComposition{Error: err.Error()}
	}
	composition := StageComposition{Checkpoints: len(set.Records)}
	if len(set.Records) > 0 {
		composition.ThroughSequence = set.Records[len(set.Records)-1].ThroughSequence
	}
	return composed, composition
}

// composeStageRecords concatenates the checkpoints' persisted segments, in
// record order, followed by tail. It reads nothing from the journal.
func composeStageRecords(runDir string, set StageRecordSet, tail []journal.Event) ([]journal.Event, error) {
	var composed []journal.Event
	for _, record := range set.Records {
		segment, err := readStageSegment(runDir, record)
		if err != nil {
			return nil, err
		}
		composed = append(composed, segment...)
	}
	return append(composed, tail...), nil
}
