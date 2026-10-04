package main

import (
	"context"
	"errors"
	"io"

	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/tracefollow"
)

type traceTranscriptOptions struct {
	show, follow, terminal, jsonOutput bool
	afterSeq                           uint64
}

func maybeTraceTranscripts(ctx context.Context, reads readservice.OfflineRuns, runID, stage string, options traceTranscriptOptions, newFollowContext func() (context.Context, func()), stdout, stderr io.Writer) (bool, int) {
	if !options.follow || !options.show {
		return maybePrintTraceTranscripts(ctx, reads, runID, stage, options.show, stdout, stderr)
	}
	followCtx, stop := newFollowContext()
	defer stop()
	if err := tracefollow.FollowTranscripts(followCtx, reads, runID, stage, options.afterSeq, options.terminal, options.jsonOutput, traceFollowPollInterval, stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return true, traceInterruptedExitCode
		}
		pf(stderr, "error: follow transcripts: %v\n", err)
		return true, 2
	}
	return true, 0
}
