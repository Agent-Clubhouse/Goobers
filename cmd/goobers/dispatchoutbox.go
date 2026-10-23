package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

const dispatchOutboxUploadTimeout = 60 * time.Second

func stageMetricsAndCaptureOutbox(ctx context.Context, exitCode int, eligible bool) map[string]float64 {
	if capture, ok := ctx.Value(declaredStageOutboxCaptureKey{}).(*declaredStageOutboxCapture); ok {
		capture.eligible = eligible
	}
	return map[string]float64{"exitCode": float64(exitCode)}
}

func podStageScrubberAndCaptureOutbox(ctx context.Context, creds []dispatcher.MintedCredential, scheme string) journal.Scrubber {
	scrubber := podStageScrubber(creds, scheme)
	capturePodOutboxScrubber(ctx, scrubber)
	return scrubber
}

func capturePodOutboxScrubber(ctx context.Context, scrubber journal.Scrubber) {
	if capture, ok := ctx.Value(declaredStageOutboxCaptureKey{}).(*declaredStageOutboxCapture); ok {
		capture.scrubber = scrubber
	}
}

func publishPodOutbox(ctx context.Context, scrubber journal.Scrubber) ([]dispatcher.SurrenderedOutboxFile, error) {
	raw := os.Getenv(dispatcher.EnvStageOutbox)
	if raw == "" {
		return nil, nil
	}
	var declared []string
	if err := json.Unmarshal([]byte(raw), &declared); err != nil {
		return nil, fmt.Errorf("decode %s: %w", dispatcher.EnvStageOutbox, err)
	}
	files, err := runner.CollectOutboxFiles(".", declared)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	if scrubber == nil {
		_, scrubber = journal.DefaultScrubber()
	}
	blobs := podBlobClient()
	if blobs == nil {
		return nil, fmt.Errorf("declared outbox requires %s", dispatcher.EnvBlobEndpoint)
	}
	uploadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dispatchOutboxUploadTimeout)
	defer cancel()
	manifest := make([]dispatcher.SurrenderedOutboxFile, 0, len(files))
	for _, file := range files {
		data := file.Data
		data = scrubber.Scrub(data)
		digest := journal.Digest(data)
		if err := blobs.Put(uploadCtx, digest, data); err != nil {
			return nil, fmt.Errorf("upload outbox file %q: %w", file.RelPath, err)
		}
		manifest = append(manifest, dispatcher.SurrenderedOutboxFile{
			RelPath: file.RelPath,
			Digest:  digest,
			Size:    int64(len(data)),
		})
	}
	return manifest, nil
}
