package livejournal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// MaxArtifactRefBytes bounds digest-adopted artifacts, including artifact sets.
// Inline artifact operations retain their existing contract.
const MaxArtifactRefBytes int64 = 16 << 20

// ArtifactSource must enforce the limit before fetching or allocating a body.
// Adoption never falls back to an unbounded Get operation.
type ArtifactSource interface {
	GetBounded(context.Context, string, int64) ([]byte, error)
}

// WithArtifactSource enables strict artifact adoption by content digest. Unlike
// optional span evidence, missing artifact bytes fail the emit for safe retry.
func WithArtifactSource(source ArtifactSource) Option {
	return func(w *Writer) { w.artifacts = source }
}

func (w *Writer) applyArtifact(ctx context.Context, run *liveRun, op Op) (bool, error) {
	a := op.Artifact
	if a == nil {
		return false, errors.New("artifact op carries no payload")
	}
	integrity := a.Integrity
	if integrity == "" && a.Ref != nil {
		integrity = a.Ref.Integrity
	}
	if integrity == "" {
		integrity = apiv1.IntegrityDerived
	}
	meta := map[string]any{EmitKeyRunnerField: op.Key}
	ref, err := w.recordArtifact(ctx, run, a, integrity, meta)
	if err != nil {
		return false, err
	}
	run.keys[op.Key] = run.jr.Seq()
	run.artifactRefs[a.Name] = ref
	return true, nil
}

func (w *Writer) recordArtifact(ctx context.Context, run *liveRun, a *ArtifactOp, integrity apiv1.Integrity, meta map[string]any) (journal.Ref, error) {
	if a.Ref != nil {
		data, err := w.fetchArtifact(ctx, a, integrity)
		if err != nil {
			return journal.Ref{}, err
		}
		return run.jr.RecordExpectedArtifactAnnotated(a.Stage, a.Attempt, a.Class, a.Name, data, *a.Ref, integrity, meta)
	}
	if a.Stage != "" {
		return run.jr.RecordStageArtifactAnnotated(a.Stage, a.Attempt, a.Class, a.Name, a.Data, integrity, meta)
	}
	return run.jr.RecordArtifactAnnotated(a.Name, a.Data, integrity, meta)
}

func (w *Writer) fetchArtifact(ctx context.Context, a *ArtifactOp, integrity apiv1.Integrity) ([]byte, error) {
	if a.Data != nil {
		return nil, errors.New("artifact op cannot carry both inline data and a ref")
	}
	ref := a.Ref
	path, err := journal.ArtifactPath(ref.Digest)
	if err != nil || path != ref.Path || strings.ToLower(ref.Digest) != ref.Digest {
		return nil, errors.New("artifact op has an invalid content-addressed ref")
	}
	if ref.Size < 0 || ref.Size > MaxArtifactRefBytes {
		return nil, fmt.Errorf("artifact ref size %d is outside 0..%d", ref.Size, MaxArtifactRefBytes)
	}
	if !integrity.Valid() || (ref.Integrity != "" && ref.Integrity != integrity) {
		return nil, errors.New("artifact ref has invalid or conflicting integrity")
	}
	if w.artifacts == nil {
		return nil, errors.New("no bounded artifact source configured")
	}
	data, err := w.artifacts.GetBounded(ctx, ref.Digest, max(ref.Size, 1))
	if err != nil {
		return nil, fmt.Errorf("fetch artifact %q: %w", a.Name, err)
	}
	if int64(len(data)) != ref.Size || journal.Digest(data) != ref.Digest {
		return nil, fmt.Errorf("fetched artifact %q does not match its ref", a.Name)
	}
	return data, nil
}
