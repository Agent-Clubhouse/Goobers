package workflowcluster

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
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
)

// Default scan bounds. They keep a behavior scan's memory proportional to
// MaxRuns × (MaxEventBytes + MaxSpanBytes) regardless of instance history.
const (
	DefaultBehaviorMaxRuns       = 2000
	DefaultBehaviorMaxEventBytes = 16 << 20
	DefaultBehaviorMaxSpanBytes  = 8 << 20
	// maxSpanRecordBytes bounds one span record; a larger record marks the run
	// oversized rather than silently undercounting its usage.
	maxSpanRecordBytes = 1 << 20
)

// BehaviorScanOptions bounds a read-only journal scan.
type BehaviorScanOptions struct {
	// Since excludes runs that started before it. Zero scans all retained runs.
	Since time.Time
	// MaxRuns caps the runs read, newest journals first. Default 2000.
	MaxRuns int
	// MaxEventBytes skips runs whose event log is larger. Default 16 MiB.
	MaxEventBytes int64
	// MaxSpanBytes skips runs whose span records total more. Default 8 MiB.
	MaxSpanBytes int64
}

func (o BehaviorScanOptions) withDefaults() BehaviorScanOptions {
	if o.MaxRuns <= 0 {
		o.MaxRuns = DefaultBehaviorMaxRuns
	}
	if o.MaxEventBytes <= 0 {
		o.MaxEventBytes = DefaultBehaviorMaxEventBytes
	}
	if o.MaxSpanBytes <= 0 {
		o.MaxSpanBytes = DefaultBehaviorMaxSpanBytes
	}
	return o
}

// BehaviorScan is the result of scanning an instance's run journals. Every
// candidate run is either described or counted under exactly one exclusion.
type BehaviorScan struct {
	Descriptors []BehaviorDescriptor `json:"descriptors,omitempty"`
	// Candidates is the number of run journals found.
	Candidates int `json:"candidates"`
	// OverLimit runs were not read because MaxRuns was reached.
	OverLimit int `json:"overLimit"`
	// Oversized runs exceeded MaxEventBytes or MaxSpanBytes.
	Oversized int `json:"oversized"`
	// BeforeSince runs started before the requested window.
	BeforeSince int `json:"beforeSince"`
	// NonTerminal runs have not finished.
	NonTerminal int `json:"nonTerminal"`
	// UndefinedProvenance runs have no defined EffectiveVersion.
	UndefinedProvenance int `json:"undefinedProvenance"`
	// Unreadable runs could not be opened read-only (for example a journal
	// that needs migration) or failed to parse.
	Unreadable int `json:"unreadable"`
}

// ScanBehavior reads the run journals under the instance root and extracts
// behavior descriptors. It never writes or migrates a journal.
func ScanBehavior(root string, opts BehaviorScanOptions) (BehaviorScan, error) {
	opts = opts.withDefaults()
	candidates, err := listRunCandidates(context.Background(), root)
	if err != nil {
		return BehaviorScan{}, err
	}

	scan := BehaviorScan{Descriptors: []BehaviorDescriptor{}, Candidates: len(candidates)}
	observations := map[childObservationKey]ChildBehaviorObservation{}
	for i, c := range candidates {
		if i >= opts.MaxRuns {
			scan.OverLimit = len(candidates) - i
			break
		}
		// A run's last activity bounds its start, so older journals are
		// outside the window without being read.
		if !opts.Since.IsZero() && !c.modified.IsZero() && c.modified.Before(opts.Since) {
			scan.BeforeSince++
			continue
		}
		scan.add(c.dir, opts, observations)
	}
	for i := range scan.Descriptors {
		if scan.Descriptors[i].Lineage == nil {
			continue
		}
		key := childObservationKey{parentRunID: scan.Descriptors[i].Lineage.ParentRunID, childRunID: scan.Descriptors[i].RunID}
		observation, ok := observations[key]
		if !ok {
			continue
		}
		childObservationFeatures(scan.Descriptors[i].Numeric, journal.RunIdentity{Child: &journal.ChildLineage{}}, &observation)
	}
	sort.Slice(scan.Descriptors, func(i, j int) bool { return scan.Descriptors[i].RunID < scan.Descriptors[j].RunID })
	return scan, nil
}

type runCandidate struct {
	dir      string
	modified time.Time
}

// listRunCandidates lists recorded run journals newest activity first, so a
// run cap keeps the most recent history.
func listRunCandidates(ctx context.Context, root string) ([]runCandidate, error) {
	roots, err := instance.NewLayout(root).RunDirsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list run journals: %w", err)
	}
	var candidates []runCandidate
	for _, runsDir := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			return nil, fmt.Errorf("read runs directory: %w", err)
		}
		for _, entry := range entries {
			dir := filepath.Join(runsDir, entry.Name())
			if !entry.IsDir() || !journal.Recorded(dir) {
				continue
			}
			info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
			if err != nil {
				candidates = append(candidates, runCandidate{dir: dir})
				continue
			}
			candidates = append(candidates, runCandidate{dir: dir, modified: info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].modified.After(candidates[j].modified)
		}
		return candidates[i].dir < candidates[j].dir
	})
	return candidates, nil
}

func (s *BehaviorScan) add(dir string, opts BehaviorScanOptions, observations map[childObservationKey]ChildBehaviorObservation) {
	in, err := readBehaviorInput(dir, opts)
	switch {
	case errors.Is(err, errBehaviorOversized):
		s.Oversized++
		return
	case err != nil:
		s.Unreadable++
		return
	}
	for key, observation := range readChildObservations(in.Identity.RunID, in.Events, in.artifacts) {
		observations[key] = mergeChildObservation(observations[key], observation)
	}
	d, err := ExtractBehavior(in)
	switch {
	case errors.Is(err, ErrRunNotTerminal):
		s.NonTerminal++
	case errors.Is(err, ErrUndefinedProvenance):
		s.UndefinedProvenance++
	case err != nil:
		s.Unreadable++
	case !opts.Since.IsZero() && d.StartedAt.Before(opts.Since):
		s.BeforeSince++
	default:
		s.Descriptors = append(s.Descriptors, d)
	}
}

var errBehaviorOversized = errors.New("workflowcluster: run journal exceeds scan bounds")

func readBehaviorInput(dir string, opts BehaviorScanOptions) (BehaviorInput, error) {
	if info, err := os.Stat(filepath.Join(dir, "events.jsonl")); err == nil && info.Size() > opts.MaxEventBytes {
		return BehaviorInput{}, errBehaviorOversized
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return BehaviorInput{}, err
	}
	identity, err := reader.Identity()
	if err != nil {
		return BehaviorInput{}, err
	}
	events, err := reader.Events()
	if err != nil {
		return BehaviorInput{}, err
	}
	in := BehaviorInput{Identity: identity, Events: events}
	in.TaskCohort = pinnedTaskCohort(reader, identity)
	var total int64
	for _, e := range events {
		if e.Type != journal.EventSpanRecorded || e.Ref == nil || e.DataSchema != telemetry.SpanSchema {
			continue
		}
		if e.Ref.Size > maxSpanRecordBytes {
			return BehaviorInput{}, errBehaviorOversized
		}
		data, err := reader.SpanBytes(*e.Ref)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return BehaviorInput{}, err
		}
		// Ref.Size is informational, so the actual length is what is charged.
		if int64(len(data)) > maxSpanRecordBytes {
			return BehaviorInput{}, errBehaviorOversized
		}
		total += int64(len(data))
		if total > opts.MaxSpanBytes {
			return BehaviorInput{}, errBehaviorOversized
		}
		var span telemetry.SpanRecord
		if json.Unmarshal(data, &span) == nil {
			in.Spans = append(in.Spans, span)
		}
	}
	in.artifacts = readBehaviorArtifacts(reader, events)
	return in, nil
}

type behaviorArtifact struct {
	data []byte
}

func pinnedTaskCohort(reader *journal.Reader, identity journal.RunIdentity) string {
	for _, input := range identity.Inputs {
		if input.Name != journal.PinnedWorkflowDefinitionInputName {
			continue
		}
		data, err := reader.ArtifactBytesBounded(input.Ref, 4<<20)
		if err != nil {
			return ""
		}
		var definition workflow.Definition
		if json.Unmarshal(data, &definition) != nil {
			return ""
		}
		return Cohort(apiv1.Workflow{Spec: definition.Spec})
	}
	return ""
}

func readBehaviorArtifacts(reader *journal.Reader, events []journal.Event) []behaviorArtifact {
	var artifacts []behaviorArtifact
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil ||
			!strings.HasPrefix(event.Name, "child-") || !strings.HasSuffix(event.Name, "-completion.json") {
			continue
		}
		data, err := reader.ArtifactBytesBounded(*event.Ref, journal.MaxChildWaitBytes)
		if err == nil {
			artifacts = append(artifacts, behaviorArtifact{data: data})
		}
	}
	return artifacts
}

type childObservationKey struct {
	parentRunID string
	childRunID  string
}

func readChildObservations(parentRunID string, events []journal.Event, artifacts []behaviorArtifact) map[childObservationKey]ChildBehaviorObservation {
	type activeWait struct {
		childRunID string
		requestID  string
		action     string
		started    time.Time
	}
	active := map[int]activeWait{}
	requests := map[string]journal.ChildHandoffRequest{}
	out := map[childObservationKey]ChildBehaviorObservation{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation {
			continue
		}
		switch event.Runner["kind"] {
		case journal.ChildWaitKind:
			request, ok := childWaitRequest(event)
			if ok && request.ParentRunID == parentRunID {
				active[event.Branch] = activeWait{request.ChildRunID, request.RequestID, request.Action, event.Time}
				requests[request.RequestID] = request
				key := childObservationKey{parentRunID: parentRunID, childRunID: request.ChildRunID}
				observation := out[key]
				if request.Action != "wait" {
					observation.Disposition = request.Action
				}
				out[key] = observation
			}
		case journal.ChildContinuedKind:
			wait, ok := active[event.Branch]
			if !ok || event.Runner["requestId"] != wait.requestID || event.Time.Before(wait.started) {
				continue
			}
			key := childObservationKey{parentRunID: parentRunID, childRunID: wait.childRunID}
			observation := out[key]
			observation.ParentWait += event.Time.Sub(wait.started)
			if wait.action != "wait" {
				observation.Disposition = wait.action
			}
			out[key] = observation
			delete(active, event.Branch)
		}
	}
	for _, artifact := range artifacts {
		var completion struct {
			Request struct {
				ParentRunID string `json:"parentRunId"`
				RequestID   string `json:"requestId"`
				ChildRunID  string `json:"childRunId"`
				Action      string `json:"action"`
			} `json:"request"`
			Completion struct {
				Publications []struct {
					Action string `json:"action"`
					State  string `json:"state"`
				} `json:"publications"`
			} `json:"completion"`
		}
		if json.Unmarshal(artifact.data, &completion) != nil {
			continue
		}
		request, ok := requests[completion.Request.RequestID]
		if !ok || completion.Request.ParentRunID != parentRunID ||
			completion.Request.ChildRunID != request.ChildRunID || completion.Request.Action != request.Action {
			continue
		}
		key := childObservationKey{parentRunID: parentRunID, childRunID: completion.Request.ChildRunID}
		observation := out[key]
		if completion.Request.Action != "wait" {
			observation.Disposition = completion.Request.Action
		}
		if observation.PublicationState == nil {
			observation.PublicationState = map[string]string{}
		}
		observation.CompletionSeen = true
		for _, publication := range completion.Completion.Publications {
			if publication.Action == "branch" || publication.Action == "pr" {
				observation.PublicationState[publication.Action] = publication.State
			}
		}
		out[key] = observation
	}
	return out
}

func mergeChildObservation(a, b ChildBehaviorObservation) ChildBehaviorObservation {
	a.ParentWait += b.ParentWait
	if b.Disposition != "" {
		a.Disposition = b.Disposition
	}
	a.CompletionSeen = a.CompletionSeen || b.CompletionSeen
	if a.PublicationState == nil {
		a.PublicationState = map[string]string{}
	}
	for action, state := range b.PublicationState {
		a.PublicationState[action] = state
	}
	return a
}
