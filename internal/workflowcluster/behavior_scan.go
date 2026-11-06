package workflowcluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
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
		scan.add(c.dir, opts)
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

func (s *BehaviorScan) add(dir string, opts BehaviorScanOptions) {
	in, err := readBehaviorInput(dir, opts)
	switch {
	case errors.Is(err, errBehaviorOversized):
		s.Oversized++
		return
	case err != nil:
		s.Unreadable++
		return
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
	return in, nil
}
