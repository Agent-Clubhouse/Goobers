package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

var _ harness.PreparedArtifactRecorder = podArtifactRecorder{}

const preparedArtifactPublishBudget = time.Minute

// RecordPreparedArtifact publishes the exact sanitized bytes, then awaits the
// journal's verified adoption from the blob store. A successful reference means
// both durability steps completed, including for an empty member. Diagnostic
// streams continue through the independent best-effort RecordArtifact path.
func (r podArtifactRecorder) RecordPreparedArtifact(ctx context.Context, name, mediaType string, data []byte) (journal.Ref, error) {
	id, err := preparedArtifactIdentity()
	if err != nil {
		return journal.Ref{}, err
	}
	blobs := podBlobClient()
	if blobs == nil {
		return journal.Ref{}, errors.New("prepared artifact: blob plane is not configured")
	}
	if len(data) > artifactset.MaxPayloadBytes {
		return journal.Ref{}, errors.New("prepared artifact exceeds payload limit")
	}
	ref, err := journal.ArtifactRef(data)
	if err != nil {
		return journal.Ref{}, err
	}
	ref.MediaType = mediaType
	ctx, cancel := context.WithTimeout(ctx, preparedArtifactPublishBudget)
	defer cancel()
	if err := blobs.Put(ctx, ref.Digest, data); err != nil {
		return journal.Ref{}, fmt.Errorf("prepared artifact %q: publish blob: %w", name, err)
	}
	// Adoption includes a bounded blob read, scrubbing, and durable disk writes.
	// It shares the publication budget instead of the short diagnostic budget.
	emitter := &livejournal.HTTPEmitter{
		BaseURL: id.daemonAPI, Token: id.token,
		Client: &http.Client{Timeout: preparedArtifactPublishBudget}, RetryDeadline: preparedArtifactPublishBudget,
	}
	_, err = emitter.Emit(ctx, livejournal.EmitRequest{
		RunID: id.runID, Gaggle: id.gaggle,
		Ops: []livejournal.Op{{
			Kind: livejournal.OpArtifact,
			// Content distinguishes a member named artifact-set.json from the
			// normalized index; physical identity distinguishes later pods.
			Key:  podJournalKey(id.podAttempt, id.stage+"/prepared/"+name+"/"+ref.Digest),
			Time: time.Now().UTC(),
			Artifact: &livejournal.ArtifactOp{
				Stage: id.stage, Attempt: id.attempt,
				Class: journal.AttemptClass(os.Getenv(dispatcher.EnvAttemptClass)),
				Name:  id.stage + "/" + name, Ref: &ref,
			},
		}},
	})
	if err != nil {
		return journal.Ref{}, fmt.Errorf("prepared artifact %q: adopt into journal: %w", name, err)
	}
	return ref, nil
}

func preparedArtifactIdentity() (podStageIdentity, error) {
	id, ok := podStageIdentityFromEnv()
	if !ok || id.gaggle == "" {
		return podStageIdentity{}, errors.New("prepared artifact: journal identity is not configured")
	}
	if _, err := podSurrenderAttempt(); err != nil {
		return podStageIdentity{}, err
	}
	logical, err := strconv.Atoi(os.Getenv(dispatcher.EnvAttempt))
	if err != nil || logical < 1 {
		return podStageIdentity{}, errors.New("prepared artifact: invalid logical attempt")
	}
	id.attempt = logical
	return id, nil
}
