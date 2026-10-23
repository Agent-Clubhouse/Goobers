package childengine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

type recordingSource struct {
	asked      []string
	generation childworkflow.Generation
	err        error
}

func (s *recordingSource) Acquire(_ context.Context, generation string) (childworkflow.Generation, error) {
	s.asked = append(s.asked, generation)
	return s.generation, s.err
}

type appliedConfig struct {
	candidate cloudconfig.Candidate
	ok        bool
}

func (a appliedConfig) Applied() (cloudconfig.Candidate, bool) { return a.candidate, a.ok }

func running() appliedConfig {
	return appliedConfig{candidate: cloudconfig.Candidate{Digest: "sha256:applied", Config: &instance.Config{}, Set: &instance.ConfigSet{}}, ok: true}
}

func envelope() childworkflow.ChildStartEnvelope {
	return childworkflow.ChildStartEnvelope{Gaggle: "web", ParentRunID: "parent-run", ParentStage: "plan", ParentWorkflow: "parent",
		ConfigGeneration: "sha256:pinned-generation", ConfigDigest: "sha256:admission-context", ParentWorkflowDigest: "sha256:wf", ParentGooberDigest: "sha256:goober"}
}

func TestPinnedResolverLoadsTheEnvelopesGenerationNeverTheApplied(t *testing.T) {
	source := &recordingSource{err: objstore.ErrGenerationNotFound}
	resolve, err := PinnedResolver("instance-a", source, running())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), envelope()); !errors.Is(err, objstore.ErrGenerationNotFound) {
		t.Fatalf("resolve = %v", err)
	}
	if len(source.asked) != 1 || source.asked[0] != "sha256:pinned-generation" {
		t.Fatalf("resolver acquired %v, want only the envelope's ConfigGeneration", source.asked)
	}
}

func TestPinnedResolverWithoutAnAppliedGenerationIsUnavailable(t *testing.T) {
	source := &recordingSource{}
	resolve, err := PinnedResolver("instance-a", source, appliedConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(t.Context(), envelope()); !errors.Is(err, engine.ErrChildCustodyUnavailable) {
		t.Fatalf("resolve before any apply = %v, want unavailable", err)
	}
	if len(source.asked) != 0 {
		t.Fatal("resolver read a generation with no applied configuration")
	}
}

func TestPinnedResolverRefusesAnUnloadedGeneration(t *testing.T) {
	released := 0
	for name, generation := range map[string]childworkflow.Generation{
		"no release": {Directory: "/tmp/generation", Set: &instance.ConfigSet{}},
		"no set":     {Directory: "/tmp/generation", Release: func() error { released++; return nil }},
		"no tree":    {Set: &instance.ConfigSet{}, Release: func() error { released++; return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			resolve, err := PinnedResolver("instance-a", &recordingSource{generation: generation}, running())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolve(t.Context(), envelope()); err == nil {
				t.Fatal("resolved from an unloaded generation")
			}
		})
	}
	if released != 2 {
		t.Fatalf("released %d acquired generations, want 2 (every one with a release)", released)
	}
}

func TestPinnedResolverRequiresItsDependencies(t *testing.T) {
	for name, build := range map[string]func() error{
		"instance": func() error { _, err := PinnedResolver("", &recordingSource{}, running()); return err },
		"source":   func() error { _, err := PinnedResolver("instance-a", nil, running()); return err },
		"applied":  func() error { _, err := PinnedResolver("instance-a", &recordingSource{}, nil); return err },
	} {
		if build() == nil {
			t.Errorf("resolver built without its %s", name)
		}
	}
}

func TestClassifyRefusesGenerationAndDriftFailures(t *testing.T) {
	for _, err := range []error{objstore.ErrGenerationNotFound, objstore.ErrGenerationInvalid, childworkflow.ErrFrozenChildDrift, childworkflow.ErrAuthorityUnavailable} {
		if got := classify(fmt.Errorf("childengine: freeze accepted child: %w", err)); !errors.Is(got, engine.ErrChildCustodyRefused) {
			t.Errorf("classify(%v) = %v, want refused", err, got)
		}
	}
}
