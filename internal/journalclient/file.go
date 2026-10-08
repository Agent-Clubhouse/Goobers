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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decomposition"
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

// ArtifactBytesBounded implements Reader.
func (f *File) ArtifactBytesBounded(ref journal.Ref, maxBytes int64) ([]byte, error) {
	return f.reader.ArtifactBytesBounded(ref, maxBytes)
}

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

// FileCrossRun answers the cross-run questions from the instance's own
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

// journalClockSlack absorbs skew between an event's recorded time and the
// filesystem's modification time of the file it was appended to.
const journalClockSlack = time.Minute

// journalQuiescentBefore reports that a run's journal was last appended to
// before since, so none of its events can fall inside the window. It costs one
// stat and is what lets a cross-run scan over a large retained history skip
// out-of-window runs without opening (and, for OpenRead, schema-checking) their
// journals. Conservative: a zero since or any stat failure never prunes.
func journalQuiescentBefore(runDir string, since time.Time) bool {
	if since.IsZero() {
		return false
	}
	info, err := os.Stat(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		return false
	}
	return info.ModTime().Before(since.Add(-journalClockSlack))
}

// RunLister is the slice of the daemon's live, read-model-backed run reader
// the cross-run scans narrow their candidate set with.
type RunLister interface {
	ListRuns(ctx context.Context, options readservice.RunListOptions) (readservice.RunList, error)
}

// runCandidate is one run directory a cross-run scan will inspect.
type runCandidate struct {
	dir  string
	name string
}

// runListPageLimit is the read model's maximum page size.
const runListPageLimit = 200

// activeRunCandidates names the run directories that can hold an event at or
// after since, using the read model (an indexed query on the gaggle's
// last-activity axis) instead of listing the runs directory.
//
// workflows restricts the listing to the runs of those workflows, one indexed
// listing per workflow. The caller derives the set from the route's own
// producers (WorkflowCanRecordBaseSyncConflict, WorkflowCanStrandUnpushedWork):
// a run of any other workflow cannot hold an event the scan reads, so skipping
// it cannot change the answer. An empty set is a legitimate answer (no workflow
// can contribute) and lists nothing.
//
// It is a superset of what the directory scan would open after its own
// journalQuiescentBefore prune AND could contribute from: a run with an event
// timestamped at or after since has last activity at or after since, and the
// window is widened by the same journalClockSlack the file prune uses.
// Candidates come back in ascending run-id order, the order os.ReadDir yields
// across the merged per-workflow listings, so tie-breaks (newest diff wins,
// first seen on a tie) are identical. Requires a gaggle and a since: an
// unbounded window would be every run.
func (f *FileCrossRun) activeRunCandidates(ctx context.Context, reads RunLister, gaggle string, since time.Time, workflows []string) ([]runCandidate, error) {
	if reads == nil {
		return nil, errors.New("journalclient: a live read model is required to narrow the run set")
	}
	if gaggle == "" || since.IsZero() {
		return nil, errors.New("journalclient: a gaggle and a since are required to narrow the run set")
	}
	runDirs, err := f.scoped(gaggle).RunDirs()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, workflow := range workflows {
		if workflow == "" {
			continue // an empty Workflow would list every workflow's runs
		}
		cursor := ""
		for {
			page, err := reads.ListRuns(ctx, readservice.RunListOptions{
				Gaggle:          gaggle,
				Workflow:        workflow,
				Since:           since.Add(-journalClockSlack),
				OrderByActivity: true,
				ShowNoWork:      true,
				Limit:           runListPageLimit,
				Cursor:          cursor,
			})
			if err != nil {
				return nil, fmt.Errorf("journalclient: list active %s runs: %w", workflow, err)
			}
			for _, run := range page.Runs {
				if _, dup := seen[run.ID]; !dup {
					seen[run.ID] = struct{}{}
					ids = append(ids, run.ID)
				}
			}
			if page.NextCursor == "" || len(page.Runs) == 0 {
				break
			}
			cursor = page.NextCursor
		}
	}
	sort.Strings(ids)
	candidates := make([]runCandidate, 0, len(ids))
	for _, id := range ids {
		for _, runsDir := range runDirs {
			dir := filepath.Join(runsDir, id)
			if info, err := os.Lstat(dir); err == nil && info.IsDir() {
				candidates = append(candidates, runCandidate{dir: dir, name: id})
				break
			}
		}
	}
	return candidates, nil
}

// WorkflowCanRecordBaseSyncConflict reports whether a run of this workflow can
// journal a "<stage>/base-sync-conflict.json" artifact, the only thing
// ConflictTouches reads. Both producers (the local runner's dispatchTask and
// the engine's RunDeterministic) record it only when a task's base
// synchronization fails, and that happens only for a task declaring
// run.syncBase.
func WorkflowCanRecordBaseSyncConflict(spec apiv1.WorkflowSpec) bool {
	for i := range spec.Tasks {
		if run := spec.Tasks[i].Run; run != nil && run.SyncBase {
			return true
		}
	}
	return false
}

// WorkflowCanStrandUnpushedWork reports whether a run of this workflow can
// journal a "<stage>/unpushed-diff.json" artifact, the only thing UnpushedWork
// reads. Both producers (the local runner's recordUnpushedDiff and the engine's
// captureUnpushedDiff) record it after an AGENTIC task attempt whose workspace
// is the writable run-branch worktree (an unset workspace is that worktree).
// Deterministic tasks, gates, and scratch or read-only workspaces never do.
func WorkflowCanStrandUnpushedWork(spec apiv1.WorkflowSpec) bool {
	for i := range spec.Tasks {
		task := spec.Tasks[i]
		if task.Type != apiv1.TaskAgentic {
			continue
		}
		if mode := task.EffectiveWorkspace(); mode == "" || mode.IsWritableRepo() {
			return true
		}
	}
	return false
}

// ConflictTouches implements CrossRun over the gaggle's run directories. Runs
// whose journal went quiet before req.Since are pruned by a stat before any
// journal is opened. It lists every run directory, so it is the same-host and
// offline path; the daemon uses ConflictTouchesFromReads.
func (f *FileCrossRun) ConflictTouches(ctx context.Context, req ConflictTouchRequest) ([]ConflictTouch, error) {
	layout := f.scoped(req.Gaggle)
	runDirs, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}
	var candidates []runCandidate
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
			if entry.IsDir() {
				candidates = append(candidates, runCandidate{dir: filepath.Join(runsDir, entry.Name()), name: entry.Name()})
			}
		}
	}
	return f.conflictTouchesFrom(ctx, candidates, req)
}

// ConflictTouchesFromReads answers ConflictTouches without listing the run
// directories: the live read model names the runs whose journal saw activity
// since req.Since, and only those journals are opened. Results are identical to
// ConflictTouches (see activeRunCandidates for why the candidate set is a
// superset of every run that can contribute). workflows are the workflows whose
// runs can record a conflict artifact (WorkflowCanRecordBaseSyncConflict).
func (f *FileCrossRun) ConflictTouchesFromReads(ctx context.Context, reads RunLister, workflows []string, req ConflictTouchRequest) ([]ConflictTouch, error) {
	candidates, err := f.activeRunCandidates(ctx, reads, req.Gaggle, req.Since, workflows)
	if err != nil {
		return nil, err
	}
	return f.conflictTouchesFrom(ctx, candidates, req)
}

func (f *FileCrossRun) conflictTouchesFrom(ctx context.Context, candidates []runCandidate, req ConflictTouchRequest) ([]ConflictTouch, error) {
	byRun := make(map[string]map[string]struct{})
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if journalQuiescentBefore(candidate.dir, req.Since) {
			continue
		}
		reader, err := journal.OpenRead(candidate.dir)
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
				return nil, fmt.Errorf("decode conflict artifact for run %s: %w", candidate.name, err)
			}
			if artifact.Code != ConflictArtifactCode || len(artifact.ConflictingFiles) == 0 {
				continue
			}
			files := byRun[candidate.name]
			if files == nil {
				files = make(map[string]struct{})
				byRun[candidate.name] = files
			}
			for _, path := range artifact.ConflictingFiles {
				if path != "" {
					files[path] = struct{}{}
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

// EscalationCandidates implements CrossRun by running
// decomposition.FindEscalationCandidates directly over the gaggle's own
// readservice.OfflineRuns projection — the SAME function select-source called
// locally before #4342, so this backend and the HTTP backend (which runs the
// identical function daemon-side) can never drift from each other or from
// pre-#4342 local behavior.
func (f *FileCrossRun) EscalationCandidates(ctx context.Context, req EscalationCandidatesRequest) ([]EscalationCandidate, error) {
	offline, err := readservice.NewOfflineRuns(f.scoped(req.Gaggle))
	if err != nil {
		return nil, err
	}
	return EscalationCandidatesFromReads(ctx, offline, "")
}

// EscalationCandidatesFromReads runs the shared selection over any run reader.
// The daemon passes its live read-model-backed service with the request's
// gaggle (so the list is scoped by the indexed gaggle column); FileCrossRun
// passes the layout-scoped offline reader with no gaggle. The qualification
// rules and ordering are decomposition.FindEscalationCandidates's in both.
func EscalationCandidatesFromReads(ctx context.Context, reads decomposition.EscalationReads, gaggle string) ([]EscalationCandidate, error) {
	if gaggle != "" {
		reads = gaggleScopedReads{EscalationReads: reads, gaggle: gaggle}
	}
	found, err := decomposition.FindEscalationCandidates(ctx, reads)
	if err != nil {
		return nil, err
	}
	candidates := make([]EscalationCandidate, 0, len(found))
	for _, c := range found {
		candidates = append(candidates, EscalationCandidate{
			SourceRunID:    c.SourceRunID,
			SourceWorkflow: c.SourceWorkflow,
			SourceStage:    c.SourceStage,
			ErrorCode:      c.ErrorCode,
			ErrorMessage:   c.ErrorMessage,
			StartedAt:      c.StartedAt,
			ParentProvider: c.ParentProvider,
			ParentID:       c.ParentID,
		})
	}
	return candidates, nil
}

// gaggleScopedReads pins every ListRuns to one gaggle.
type gaggleScopedReads struct {
	decomposition.EscalationReads
	gaggle string
}

func (g gaggleScopedReads) ListRuns(ctx context.Context, options readservice.RunListOptions) (readservice.RunList, error) {
	options.Gaggle = g.gaggle
	return g.EscalationReads.ListRuns(ctx, options)
}

// BranchOwnership implements CrossRun by opening the target run's own
// journal and checking it actually recorded owning the branch — the same
// checks reconcile-branches's own inspectBranchOwner made directly before
// #4344, moved here so the file and HTTP backends answer identically.
func (f *FileCrossRun) BranchOwnership(ctx context.Context, req BranchOwnershipRequest) (BranchOwnershipResponse, error) {
	if err := ctx.Err(); err != nil {
		return BranchOwnershipResponse{}, err
	}
	ambiguous := func(detail string) (BranchOwnershipResponse, error) {
		return BranchOwnershipResponse{Reason: "ambiguous-ownership", Detail: detail}, nil
	}
	unreadable := func(detail string) (BranchOwnershipResponse, error) {
		return BranchOwnershipResponse{Reason: "run-journal-unreadable", Detail: detail}, nil
	}
	layout := f.scoped(req.Gaggle)
	dir, err := layout.FindRunDir(req.TargetRunID)
	if err != nil {
		return ambiguous(fmt.Sprintf("open owning run %s: %v", req.TargetRunID, err))
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		return ambiguous(fmt.Sprintf("open owning run %s: %v", req.TargetRunID, err))
	}
	id, err := reader.Identity()
	if err != nil {
		return ambiguous(fmt.Sprintf("read owning run %s identity: %v", req.TargetRunID, err))
	}
	if id.RunID != req.TargetRunID || id.Workflow != req.Workflow || id.StartedAt.IsZero() {
		return ambiguous(fmt.Sprintf("branch %q does not match owning run identity", req.Branch))
	}
	events, err := reader.Events()
	if err != nil {
		return unreadable(fmt.Sprintf("read owning run %s events: %v", req.TargetRunID, err))
	}
	owned := false
	var terminalAt time.Time
	for _, event := range events {
		if event.Type == journal.EventRefTouched && event.ExternalRef != nil &&
			event.ExternalRef.Provider == "github" && event.ExternalRef.Kind == "branch" && event.ExternalRef.ID == req.Branch {
			owned = true
		}
		if event.Type == journal.EventRunFinished {
			terminalAt = event.Time
		}
	}
	if !owned {
		return ambiguous(fmt.Sprintf("owning run %s has no journaled reference to branch %q", req.TargetRunID, req.Branch))
	}
	phase, err := reader.Phase()
	if err != nil {
		return unreadable(fmt.Sprintf("read owning run %s phase: %v", req.TargetRunID, err))
	}
	if terminalBranchOwnershipPhase(phase) && terminalAt.IsZero() {
		return unreadable(fmt.Sprintf("owning run %s has no timestamped terminal event", req.TargetRunID))
	}
	return BranchOwnershipResponse{Owner: &BranchOwnership{
		Workflow: req.Workflow, RunID: req.TargetRunID, StartedAt: id.StartedAt, TerminalAt: terminalAt, Phase: string(phase),
	}}, nil
}

// terminalBranchOwnershipPhase mirrors reconcile-branches's own
// terminalBranchRunPhase — duplicated rather than imported to avoid a
// cmd/goobers dependency from this package.
func terminalBranchOwnershipPhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
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
// It lists every run directory, so it is the same-host and offline path; the
// daemon uses UnpushedWorkFromReads.
func (f *FileCrossRun) UnpushedWork(ctx context.Context, req UnpushedWorkRequest) (*UnpushedWork, error) {
	if len(req.ItemIDs) == 0 {
		return nil, nil
	}
	layout := f.scoped(req.Gaggle)
	runDirs, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}
	var candidates []runCandidate
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
			if entry.IsDir() {
				candidates = append(candidates, runCandidate{dir: filepath.Join(runsDir, entry.Name()), name: entry.Name()})
			}
		}
	}
	work := f.unpushedWorkFrom(ctx, candidates, req)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return work, nil
}

// UnpushedWorkFromReads answers UnpushedWork without listing the run
// directories: the live read model names the runs with journal activity since
// req.Since, and only those journals are opened. Results are identical to
// UnpushedWork. workflows are the workflows whose runs can strand a diff
// (WorkflowCanStrandUnpushedWork).
func (f *FileCrossRun) UnpushedWorkFromReads(ctx context.Context, reads RunLister, workflows []string, req UnpushedWorkRequest) (*UnpushedWork, error) {
	if len(req.ItemIDs) == 0 {
		return nil, nil
	}
	candidates, err := f.activeRunCandidates(ctx, reads, req.Gaggle, req.Since, workflows)
	if err != nil {
		return nil, err
	}
	work := f.unpushedWorkFrom(ctx, candidates, req)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return work, nil
}

func (f *FileCrossRun) unpushedWorkFrom(ctx context.Context, candidates []runCandidate, req UnpushedWorkRequest) *UnpushedWork {
	limit := req.MaxInlineDiffBytes
	if limit <= 0 {
		limit = DefaultMaxInlineDiffBytes
	}
	var best *UnpushedWork
	for _, c := range candidates {
		if ctx.Err() != nil {
			return nil
		}
		if c.name == req.RunID || journalQuiescentBefore(c.dir, req.Since) {
			continue
		}
		candidate := f.unpushedWorkFromRun(c.dir, req.ItemIDs, req.Since, limit)
		if candidate == nil {
			continue
		}
		if best == nil || candidate.RecordedAt.After(best.RecordedAt) {
			best = candidate
		}
	}
	return best
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
		case journal.EventRefTouched, journal.EventRunnerMutationRecovered:
			if event.IsReferenceTouch() &&
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
