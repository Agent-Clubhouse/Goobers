package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type copilotTranscriptCheckpoints struct {
	worker transcriptCheckpoints
	root   *os.File
}

func startCopilotTranscriptCheckpoints(req *RunRequest, path string, env []string) (copilotTranscriptCheckpoints, error) {
	if req.TranscriptCheckpoint == nil || path == "" {
		return copilotTranscriptCheckpoints{}, nil
	}
	rootPath := req.Workspace
	relative, err := filepath.Rel(rootPath, path)
	if err != nil || !filepath.IsLocal(relative) {
		home, ok := copilotConfigHome(env)
		if !ok {
			return copilotTranscriptCheckpoints{}, errors.New("native transcript checkpoint has no configured root")
		}
		withinHome, homeErr := filepath.Rel(home, path)
		if homeErr != nil || !filepath.IsLocal(withinHome) {
			return copilotTranscriptCheckpoints{}, errors.New("native transcript checkpoint escapes configured home")
		}
		// The harness may create its config directory after launch. Pin its
		// existing parent; the config directory itself is opened no-follow.
		rootPath = filepath.Dir(home)
		relative, err = filepath.Rel(rootPath, path)
		if err != nil || !filepath.IsLocal(relative) {
			return copilotTranscriptCheckpoints{}, errors.New("native transcript checkpoint escapes configured root")
		}
	}
	root, err := os.Open(rootPath)
	if err != nil {
		return copilotTranscriptCheckpoints{}, fmt.Errorf("pin native transcript checkpoint root: %w", err)
	}
	// Native logs can grow without any stdout. Their independent timer and
	// the process timer share one serialized sink, including recovery turns.
	sink := req.TranscriptCheckpoint
	var mu sync.Mutex
	req.TranscriptCheckpoint = func(delta InvocationTranscriptDelta) error {
		mu.Lock()
		defer mu.Unlock()
		return sink(delta)
	}
	state := fileTranscriptCheckpoint{root: root, path: relative, limit: req.MaxTranscriptBytes,
		sink: func(delta TranscriptDelta) error {
			return req.TranscriptCheckpoint(InvocationTranscriptDelta{Source: "copilot-session", TranscriptDelta: delta})
		},
	}
	return copilotTranscriptCheckpoints{
		root:   root,
		worker: startTranscriptCheckpointWorker(req.TranscriptCheckpointInterval, state.capture),
	}, nil
}

func (c copilotTranscriptCheckpoints) finish(runErr error) error {
	if c.root == nil {
		return nil
	}
	err := c.worker.finish(transcriptEndReason(errors.Is(runErr, ErrTimeout), errors.Is(runErr, ErrCanceled)))
	return errors.Join(err, c.root.Close())
}
