package gate

import (
	"context"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
)

// ArtifactCheckFunc is separate from the pure scalar CheckFunc registry. It
// receives upstream pointers and a read-only current-run resolver, never an
// invocation envelope, workspace, credentials, or general filesystem access.
type ArtifactCheckFunc func(context.Context, map[string]interface{}, map[string]string, []apiv1.ContextPointer, artifactset.Reader) (string, error)

// ArtifactReader is owned by one evaluation and closed before it returns.
type ArtifactReader interface {
	artifactset.Reader
	Close() error
}

// OpenArtifactReader is bound by runner wiring to authoritative journal storage.
// Run and gaggle identity come from the runner-authored invocation, not inputs.
type OpenArtifactReader func(context.Context, string, string) (ArtifactReader, error)

func (e *AutomatedEvaluator) evaluateArtifacts(ctx context.Context, check ArtifactCheckFunc, conf apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (outcome string, retErr error) {
	if e.OpenArtifacts == nil {
		return "", errors.New("gate: artifact reader is not configured")
	}
	reader, err := e.OpenArtifacts(ctx, env.RunID, env.Gaggle)
	if err != nil {
		return artifactOutcome("", err)
	}
	if reader == nil {
		return "", errors.New("gate: artifact reader factory returned nil")
	}
	defer func() {
		if err := reader.Close(); err != nil {
			outcome = ""
			retErr = errors.Join(retErr, fmt.Errorf("gate: close artifact reader: %w", err))
		}
	}()
	outcome, err = check(ctx, env.Inputs, conf.Params, cloneArtifactContexts(env.ContextPointers), reader)
	return artifactOutcome(outcome, err)
}

func artifactOutcome(outcome string, err error) (string, error) {
	if errors.Is(err, artifactset.ErrInvalid) {
		return OutcomeFail, nil
	}
	if err != nil {
		return "", err
	}
	return outcome, nil
}

func cloneArtifactContexts(pointers []apiv1.ContextPointer) []apiv1.ContextPointer {
	cloned := append([]apiv1.ContextPointer(nil), pointers...)
	for i := range cloned {
		if cloned[i].Artifact != nil {
			value := *cloned[i].Artifact
			cloned[i].Artifact = &value
		}
		if cloned[i].External != nil {
			value := *cloned[i].External
			cloned[i].External = &value
		}
	}
	return cloned
}
