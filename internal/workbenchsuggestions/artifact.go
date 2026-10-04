// Package workbenchsuggestions binds untrusted relationship candidates to actual
// retained journal evidence. Callers independently hold current gaggle/source
// authorization; this package confers no visibility, credentials or write grants.
package workbenchsuggestions

import (
	"context"
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workbench"
)

const (
	// MaxJournalBytes bounds committed journal proof allocation.
	MaxJournalBytes = 16 << 20
	// MaxJournalEvents bounds proof traversal.
	MaxJournalEvents = 32768
	// MaxArtifactWindow bounds explicit artifact inventory, independently of source content.
	MaxArtifactWindow = 32
)

// ErrArtifact refuses missing, ambiguous, changed or unbounded journal evidence.
var ErrArtifact = errors.New("workbench suggestions: retained artifact evidence is unavailable")

// Selection picks an actual artifact event, never caller-supplied provenance.
type Selection = workbench.SuggestionSelection

// Artifact identifies committed stage evidence. Name is inert display text.
// Sequence is the artifact.recorded event; StageSequence proves its attempt.
type Artifact = workbench.SuggestionArtifact

// Inventory contains one explicit bounded window, with no artifact body reads.
type Inventory = workbench.SuggestionInventory

// Loaded is host provenance plus parsed candidates. It is not source truth and
// must never be fed to the source graph or accepted without fresh source checks.
type Loaded struct {
	RunID            string
	ConfigGeneration string
	Artifact         Artifact
	Origin           workbench.SuggestionOrigin
	Suggestions      []workbench.BoundSuggestion
}

// List inventories stage artifacts only after the caller authorizes this gaggle.
// It does not presume a JSON-looking file is a valid suggestion artifact.
func List(ctx context.Context, reader *journal.Reader, gaggle, runID string, after uint64) (Inventory, error) {
	_, events, err := proof(ctx, reader, gaggle, runID)
	if err != nil {
		return Inventory{}, err
	}
	result := Inventory{RunID: runID, Artifacts: []Artifact{}}
	starts := map[stageKey]uint64{}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		recordStart(starts, event)
		if event.Seq <= after || event.Type != journal.EventArtifactRecorded {
			continue
		}
		artifact, err := descriptor(event, starts[eventStageKey(event)])
		if err != nil {
			continue
		}
		if len(result.Artifacts) == MaxArtifactWindow {
			result.Partial = true
			result.NextSequence = result.Artifacts[len(result.Artifacts)-1].Sequence
			break
		}
		result.Artifacts = append(result.Artifacts, artifact)
	}
	if err := ctx.Err(); err != nil {
		return Inventory{}, err
	}
	return result, nil
}

// Load verifies the exact committed event and content address. The immutable run
// supplies run/config identity and stage.started supplies the emitting occurrence.
func Load(ctx context.Context, reader *journal.Reader, set workbench.SourceSet, selected Selection) (Loaded, error) {
	identity, events, err := proof(ctx, reader, set.Scope.GaggleID, selected.RunID)
	if err != nil {
		return Loaded{}, err
	}
	if selected.Sequence == 0 {
		return Loaded{}, ErrArtifact
	}
	var event journal.Event
	starts := map[stageKey]uint64{}
	for _, candidate := range events {
		if err := ctx.Err(); err != nil {
			return Loaded{}, err
		}
		recordStart(starts, candidate)
		if candidate.Seq == selected.Sequence {
			event = candidate
			break
		}
	}
	artifact, err := descriptor(event, starts[eventStageKey(event)])
	if err != nil {
		return Loaded{}, err
	}
	data, err := reader.ArtifactBytesBounded(*event.Ref, workbench.MaxSuggestionBytes)
	if err != nil || int64(len(data)) != artifact.Bytes {
		return Loaded{}, ErrArtifact
	}
	origin := workbench.SuggestionOrigin{RunID: identity.RunID, StageID: event.Stage, Attempt: event.Attempt, ArtifactPath: event.Ref.Path, ArtifactDigest: artifact.Digest}
	suggestions, err := workbench.BindSuggestions(data, set, origin)
	if err != nil {
		return Loaded{}, err
	}
	if err := ctx.Err(); err != nil {
		return Loaded{}, err
	}
	return Loaded{RunID: identity.RunID, ConfigGeneration: identity.ConfigGeneration, Artifact: artifact, Origin: origin, Suggestions: suggestions}, nil
}

func proof(ctx context.Context, reader *journal.Reader, gaggle, runID string) (journal.RunIdentity, []journal.Event, error) {
	if ctx.Err() != nil {
		return journal.RunIdentity{}, nil, ctx.Err()
	}
	if reader == nil || gaggle == "" || !apiv1.ValidRunID(runID) {
		return journal.RunIdentity{}, nil, ErrArtifact
	}
	identity, err := reader.Identity()
	if err != nil || identity.RunID != runID || identity.Gaggle != gaggle || !identity.KnownSchema() {
		return journal.RunIdentity{}, nil, ErrArtifact
	}
	events, err := reader.EventsBounded(MaxJournalBytes, MaxJournalEvents)
	if err != nil {
		return journal.RunIdentity{}, nil, ErrArtifact
	}
	return identity, events, nil
}

func descriptor(event journal.Event, started uint64) (Artifact, error) {
	if event.Type != journal.EventArtifactRecorded || event.Ref == nil || event.Stage == "" || len(event.Stage) > 128 || event.Attempt < 1 || event.Branch < 0 || event.Ref.Size < 1 || event.Ref.Size > workbench.MaxSuggestionBytes {
		return Artifact{}, ErrArtifact
	}
	path, err := journal.ArtifactPath(event.Ref.Digest)
	if err != nil || path != event.Ref.Path {
		return Artifact{}, ErrArtifact
	}
	if started == 0 {
		return Artifact{}, ErrArtifact
	}
	name := event.Name
	if len(name) > 256 {
		name = "Artifact name exceeds display bound"
	}
	return Artifact{Sequence: event.Seq, StageSequence: started, StageID: event.Stage, Attempt: event.Attempt, Branch: event.Branch, Name: name, Digest: strings.TrimPrefix(event.Ref.Digest, "sha256:"), Bytes: event.Ref.Size}, nil
}

type stageKey struct {
	stage           string
	attempt, branch int
	class           journal.AttemptClass
}

func eventStageKey(event journal.Event) stageKey {
	return stageKey{event.Stage, event.Attempt, event.Branch, event.AttemptClass}
}
func recordStart(starts map[stageKey]uint64, event journal.Event) {
	if event.Type != journal.EventStageStarted {
		return
	}
	key := eventStageKey(event)
	if _, exists := starts[key]; exists {
		starts[key] = 0
	} else {
		starts[key] = event.Seq
	}
}
