package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/workerhost"
)

// Automated resolves checks against one immutable gaggle snapshot, as the other
// worker execution seams do. Artifact checks read verified blobs directly; they
// never provision a workspace or populate the stage's writable cache.
func (w *workerSeams) Automated() invoke.Automated { return workerAutomated{seams: w} }

type workerAutomated struct{ seams *workerSeams }

func (a workerAutomated) Evaluate(ctx context.Context, conf apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	g, err := a.seams.forGaggle(env.Gaggle)
	if err != nil {
		return "", err
	}
	if g.cfg.Automated == nil {
		return "", errors.New("worker: automated evaluator is not configured")
	}
	automated, ok := g.cfg.Automated.(*gate.AutomatedEvaluator)
	if !ok {
		return g.cfg.Automated.Evaluate(ctx, conf, env)
	}
	bound := *automated
	bound.OpenArtifacts = func(ctx context.Context, runID, gaggle string) (gate.ArtifactReader, error) {
		if runID != env.RunID || gaggle != env.Gaggle || !apiv1.ValidRunID(runID) {
			return nil, fmt.Errorf("%w: invalid artifact run identity", artifactset.ErrInvalid)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a.seams.store != nil {
			store, ok := a.seams.store.(blobstore.BoundedReader)
			if !ok {
				return nil, errors.New("worker: artifact store does not support bounded reads")
			}
			return artifactset.NewStoreReader(store, env.ContextPointers)
		}
		reader, err := artifactset.OpenJournal(workerhost.StagingArtifactsDir(g.runsDir, runID))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: run has no staged artifact journal", artifactset.ErrInvalid)
		}
		return reader, err
	}
	return bound.Evaluate(ctx, conf, env)
}
