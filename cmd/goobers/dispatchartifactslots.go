package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

// The dispatcher's privileged environment supplies the contract. The command
// receives only declared inputs and can request contained files in its manifest;
// it cannot supply the producing stage, visit, attempt, or published pointers.
func publishPodNamedResult(ctx context.Context, stderr io.Writer, scrubber journal.Scrubber, result *apiv1.ResultEnvelope) {
	encoded := os.Getenv(dispatcher.EnvArtifactPublication)
	if encoded == "" {
		return
	}
	pointers, err := publishPodNamedArtifacts(ctx, stderr, scrubber, encoded)
	if err == nil {
		result.Artifacts = append(pointers, result.Artifacts...)
		return
	}
	code, retryable := "artifact_publication_failed", true
	var publication *artifactset.PublicationError
	if errors.As(err, &publication) {
		code, retryable = publication.Code, false
	} else if errors.Is(err, artifactset.ErrInvalid) {
		code, retryable = "invalid_declared_artifact_set", false
	}
	result.Status, result.Summary = apiv1.ResultFailure, "named artifact publication failed"
	result.Error = journal.ErrorInfoFor(code, err, retryable)
}

func publishPodNamedArtifacts(ctx context.Context, stderr io.Writer, scrubber journal.Scrubber, encoded string) ([]apiv1.ArtifactPointer, error) {
	var contract apiv1.ArtifactPublication
	if err := json.Unmarshal([]byte(encoded), &contract); err != nil || contract.Stage != os.Getenv(dispatcher.EnvStage) {
		return nil, &artifactset.PublicationError{Code: artifactset.InvalidPublicationCode}
	}
	attempt, err := strconv.ParseInt(os.Getenv(dispatcher.EnvAttempt), 10, 32)
	if err != nil || attempt < 1 {
		return nil, &artifactset.PublicationError{Code: artifactset.InvalidPublicationCode}
	}
	manifest := os.Getenv(dispatcher.InputEnvVar("artifactManifestFile"))
	if manifest == "" {
		return nil, artifactset.MissingPublication(&contract)
	}
	prepared, err := artifactset.Prepare(ctx, ".", manifest, artifactset.NewSanitizer(scrubber))
	if err != nil {
		return nil, err
	}
	if err := prepared.Bind(&contract, int32(attempt)); err != nil {
		return nil, err
	}
	return prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := (podArtifactRecorder{stderr: stderr}).RecordPreparedArtifact(ctx, name, media, data)
		return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, MediaType: media, Size: ref.Size, Integrity: ref.Integrity}, err
	})
}
