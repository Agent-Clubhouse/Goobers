package journalclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// File is the same-host backend: the instance's own run directory under
// journal.OpenRead, byte-for-byte the discipline every CLI reader used before
// this package existed.
type File struct {
	layout  instance.Layout
	runID   string
	dir     string
	reader  *journal.Reader
	offline readservice.OfflineRuns
}

// OpenFile resolves runID's directory under layout and opens it for reading.
// A run with no directory on this host is ErrRunNotFound, distinguishable by
// callers whose pre-seam behaviour tolerated a missing journal (validate-plan's
// optional decomposition input) from callers for whom it is fatal.
func OpenFile(layout instance.Layout, runID string) (*File, error) {
	dir, err := layout.FindRunDir(runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRunNotFound, runID, err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return nil, err
	}
	return &File{layout: layout, runID: runID, dir: dir, reader: reader}, nil
}

// RunID implements Reader.
func (f *File) RunID() string { return f.runID }

// Dir is the resolved run directory. Callers that still need a path (a
// harness, a diagnostic) have one; nothing on the plane side does.
func (f *File) Dir() string { return f.dir }

// Events implements Reader.
func (f *File) Events() ([]journal.Event, error) { return f.reader.Events() }

// ArtifactBytes implements Reader.
func (f *File) ArtifactBytes(ref journal.Ref) ([]byte, error) { return f.reader.ArtifactBytes(ref) }

// ArtifactByDigest implements Reader.
func (f *File) ArtifactByDigest(digest string) ([]byte, error) {
	return f.reader.ArtifactByDigest(digest)
}

// Phase implements Reader.
func (f *File) Phase() (journal.RunPhase, error) { return f.reader.Phase() }

// StageAttempts implements Reader through the same offline projection the
// daemon's route serves, so the two backends answer one shape.
func (f *File) StageAttempts(stage string) ([]StageAttempt, error) {
	if f.offline == nil {
		offline, err := readservice.NewOfflineRuns(f.layout)
		if err != nil {
			return nil, err
		}
		f.offline = offline
	}
	list, err := f.offline.StageAttempts(context.Background(), f.runID, stage)
	if err != nil {
		return nil, err
	}
	return StageAttemptsFromReadService(list.Attempts), nil
}

// StageAttemptsFromReadService converts the daemon's read projection into the
// client shape. Exported so the server can serve the identical conversion
// rather than a second spelling of it.
func StageAttemptsFromReadService(attempts []readservice.StageAttempt) []StageAttempt {
	out := make([]StageAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		converted := StageAttempt{
			ID:             attempt.ID,
			Visit:          attempt.Visit,
			Number:         attempt.Number,
			Class:          attempt.Class,
			Status:         attempt.Status,
			StartedSeq:     attempt.StartedSeq,
			FinishedSeq:    attempt.FinishedSeq,
			StartedAt:      attempt.StartedAt,
			FinishedAt:     attempt.FinishedAt,
			DurationMillis: attempt.DurationMillis,
			Outputs:        attempt.Outputs,
			Artifacts:      artifactMetadataFromReadService(attempt.Artifacts),
			Error:          attempt.Error,
		}
		out = append(out, converted)
	}
	return out
}

func artifactMetadataFromReadService(metadata []readservice.ArtifactMetadata) []ArtifactMetadata {
	out := make([]ArtifactMetadata, 0, len(metadata))
	for _, item := range metadata {
		out = append(out, ArtifactMetadata{
			Name:         item.Name,
			Digest:       item.Digest,
			Size:         item.Size,
			MediaType:    item.MediaType,
			Stage:        item.Stage,
			Attempt:      item.Attempt,
			AttemptClass: item.AttemptClass,
			RecordedSeq:  item.RecordedSeq,
		})
	}
	return out
}

var _ Reader = (*File)(nil)

// --- cross-run, same-host ---------------------------------------------------

// FileCrossRun answers the three cross-run questions from the instance's own
// run directories. It is BOTH the same-host CLI path and the daemon's own
// implementation behind the plane routes, so the two cannot drift: the plane
// handler contains the request to a gaggle and then asks exactly this.
type FileCrossRun struct {
	layout instance.Layout
	// Warn receives non-fatal discovery notes (a corrupt sidecar, an
	// unreadable blob). Nil discards them.
	Warn func(string)
}

// NewFileCrossRun builds the same-host cross-run reader over an instance root
// layout. Per-request gaggle scoping is applied by each method.
func NewFileCrossRun(layout instance.Layout) *FileCrossRun {
	return &FileCrossRun{layout: layout}
}

func (f *FileCrossRun) warn(format string, args ...any) {
	if f.Warn != nil {
		f.Warn(fmt.Sprintf(format, args...))
	}
}

// scoped returns the layout narrowed to gaggle, or the instance root layout
// when gaggle is empty.
func (f *FileCrossRun) scoped(gaggle string) instance.Layout {
	if gaggle == "" {
		return f.layout
	}
	return f.layout.ForGaggle(gaggle)
}

// ErrRunNotFound reports a target run with no readable journal on this
// instance — an explicit answer, never a phase the caller can mistake for a
// real one.
var ErrRunNotFound = errors.New("journalclient: run journal not found")

// RunPhase implements CrossRun. Errors are returned rather than swallowed:
// backlog-query's failure-streak walk must be able to tell "this run did not
// fail" from "this run could not be read".
func (f *FileCrossRun) RunPhase(ctx context.Context, targetRunID string) (journal.RunPhase, error) {
	dir, err := f.layout.FindRunDir(targetRunID)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrRunNotFound, targetRunID, err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrRunNotFound, targetRunID, err)
	}
	phase, err := reader.PhaseBounded(ctx)
	if err != nil {
		return "", fmt.Errorf("journalclient: read phase of %s: %w", targetRunID, err)
	}
	return phase, nil
}

// ConflictArtifactSuffix names the base-sync conflict sidecar
// gather-implement-context's hot-file history is built from.
const ConflictArtifactSuffix = "/base-sync-conflict.json"

// ConflictArtifactCode is the only artifact code the history counts.
const ConflictArtifactCode = "base_sync_conflict"

type conflictArtifact struct {
	Code             string   `json:"code"`
	ConflictingFiles []string `json:"conflictingFiles"`
}

// ConflictTouches implements CrossRun over the gaggle's run directories.
func (f *FileCrossRun) ConflictTouches(ctx context.Context, req ConflictTouchRequest) ([]ConflictTouch, error) {
	layout := f.scoped(req.Gaggle)
	runDirs, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}
	byRun := make(map[string]map[string]struct{})
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read runs directory %s: %w", runsDir, err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.IsDir() {
				continue
			}
			reader, err := journal.OpenRead(filepath.Join(runsDir, entry.Name()))
			if err != nil {
				continue
			}
			events, err := reader.Events()
			if err != nil {
				return nil, err
			}
			for _, event := range events {
				if !event.KnownSchema() ||
					event.Type != journal.EventArtifactRecorded ||
					event.Ref == nil ||
					event.Time.Before(req.Since) ||
					!strings.HasSuffix(event.Name, ConflictArtifactSuffix) {
					continue
				}
				data, err := reader.ArtifactBytes(*event.Ref)
				if err != nil {
					return nil, err
				}
				var artifact conflictArtifact
				if err := json.Unmarshal(data, &artifact); err != nil {
					return nil, fmt.Errorf("decode conflict artifact for run %s: %w", entry.Name(), err)
				}
				if artifact.Code != ConflictArtifactCode || len(artifact.ConflictingFiles) == 0 {
					continue
				}
				files := byRun[entry.Name()]
				if files == nil {
					files = make(map[string]struct{})
					byRun[entry.Name()] = files
				}
				for _, path := range artifact.ConflictingFiles {
					if path != "" {
						files[path] = struct{}{}
					}
				}
			}
		}
	}
	runIDs := make([]string, 0, len(byRun))
	for runID := range byRun {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	touches := make([]ConflictTouch, 0, len(runIDs))
	for _, runID := range runIDs {
		files := make([]string, 0, len(byRun[runID]))
		for path := range byRun[runID] {
			files = append(files, path)
		}
		sort.Strings(files)
		touches = append(touches, ConflictTouch{RunID: runID, Files: files})
	}
	return touches, nil
}

// UnpushedDiffMetaArtifactSuffix / UnpushedDiffSchemaPrefix mirror
// internal/runner's unpushed-diff artifact contract (recordUnpushedDiff): the
// runner persists a run branch's committed-but-never-published diff plus this
// discovery sidecar the moment an implement attempt ends, and this package
// reads them back for the next run on the same item.
const (
	UnpushedDiffMetaArtifactSuffix = "/unpushed-diff.json"
	UnpushedDiffSchemaPrefix       = "goobers.dev/unpushed-diff/"
)

// unpushedDiffArtifact mirrors internal/runner's unpushedDiffMetadata JSON.
type unpushedDiffArtifact struct {
	Schema    string   `json:"schema"`
	RunID     string   `json:"runId"`
	Stage     string   `json:"stage"`
	Attempt   int      `json:"attempt"`
	ItemIDs   []string `json:"itemIds"`
	Branch    string   `json:"branch"`
	BaseRef   string   `json:"baseRef"`
	DiffBytes int      `json:"diffBytes"`
	Diff      struct {
		Path   string `json:"path"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"diff"`
}

// UnpushedWork implements CrossRun. Best-effort per candidate run (a corrupt
// or foreign directory is skipped with a warning) but never silent about a
// failure it cannot localize: a runs-root that cannot be listed is an error.
func (f *FileCrossRun) UnpushedWork(ctx context.Context, req UnpushedWorkRequest) (*UnpushedWork, error) {
	if len(req.ItemIDs) == 0 {
		return nil, nil
	}
	limit := req.MaxInlineDiffBytes
	if limit <= 0 {
		limit = DefaultMaxInlineDiffBytes
	}
	layout := f.scoped(req.Gaggle)
	runDirs, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}
	var best *UnpushedWork
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if !os.IsNotExist(err) {
				f.warn("prior unpushed work discovery: read %s: %v", runsDir, err)
			}
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !entry.IsDir() || entry.Name() == req.RunID {
				continue
			}
			candidate := f.unpushedWorkFromRun(filepath.Join(runsDir, entry.Name()), req.ItemIDs, req.Since, limit)
			if candidate == nil {
				continue
			}
			if best == nil || candidate.RecordedAt.After(best.RecordedAt) {
				best = candidate
			}
		}
	}
	return best, nil
}

// unpushedWorkFromRun inspects one run journal for a stranded diff matching
// itemIDs; nil when the run has none, published its work, or cannot be read.
func (f *FileCrossRun) unpushedWorkFromRun(runDir string, itemIDs []string, since time.Time, maxInline int) *UnpushedWork {
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return nil // not a run journal (partial/foreign directory) — skip silently
	}
	events, err := reader.Events()
	if err != nil {
		f.warn("prior unpushed work discovery: read events of %s: %v", runDir, err)
		return nil
	}
	// Events are journal-ordered, so a single pass answers both questions:
	// which unpushed-diff sidecar is newest, and whether anything published
	// the branch AFTER it. Ordering matters — a run that pushed, then
	// remediated and died mid-cycle has publication events that predate its
	// newest stranded diff, and that diff is genuinely still unpublished.
	var meta *journal.Event
	publishedAfterDiff := false
	for i := range events {
		event := events[i]
		if !event.KnownSchema() {
			continue
		}
		switch event.Type {
		case journal.EventRefTouched:
			if event.ExternalRef != nil &&
				(event.ExternalRef.Kind == "branch" || event.ExternalRef.Kind == "pr") &&
				meta != nil {
				publishedAfterDiff = true
			}
		case journal.EventArtifactRecorded:
			if event.Ref != nil &&
				strings.HasSuffix(event.Name, UnpushedDiffMetaArtifactSuffix) &&
				!event.Time.Before(since) {
				meta = &events[i]
				publishedAfterDiff = false
			}
		}
	}
	if publishedAfterDiff || meta == nil {
		return nil
	}
	data, err := reader.ArtifactBytes(*meta.Ref)
	if err != nil {
		f.warn("prior unpushed work discovery: read %s of %s: %v", meta.Name, runDir, err)
		return nil
	}
	var artifact unpushedDiffArtifact
	if err := json.Unmarshal(data, &artifact); err != nil || !strings.HasPrefix(artifact.Schema, UnpushedDiffSchemaPrefix) {
		return nil
	}
	if !itemIDsIntersect(itemIDs, artifact.ItemIDs) {
		return nil
	}
	work := &UnpushedWork{
		RunID:   artifact.RunID,
		Stage:   artifact.Stage,
		Attempt: artifact.Attempt,
		// From the journal event, not the sidecar bytes: the sidecar carries
		// no timestamp, by design.
		RecordedAt: meta.Time,
		Branch:     artifact.Branch,
		BaseRef:    artifact.BaseRef,
		ItemIDs:    artifact.ItemIDs,
		DiffBytes:  artifact.DiffBytes,
		DiffDigest: artifact.Diff.Digest,
	}
	diff, err := reader.ArtifactByDigest(artifact.Diff.Digest)
	if err != nil {
		f.warn("prior unpushed work discovery: read diff %s of %s: %v", artifact.Diff.Digest, runDir, err)
		return work // still discoverable by digest even without the inline copy
	}
	if len(diff) > maxInline {
		diff = diff[:maxInline]
		work.DiffTruncated = true
	}
	work.Diff = string(diff)
	return work
}

func itemIDsIntersect(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

var _ CrossRun = (*FileCrossRun)(nil)
