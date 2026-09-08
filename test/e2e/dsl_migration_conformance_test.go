package e2e

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/workflow"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// This exercises migration through the real local runner, journal, agent
// executor, git worktrees/commits, and deterministic subprocess. Only the agent
// provider is scripted, using the existing walking-skeleton harness. The repo
// consumer reads the producer's committed file, including its latest repass.
// This is representative single-runner evidence, not the distributed S1–S9 exit.
func TestConformanceDSL30MigrationPreservesLocalRuntime(t *testing.T) {
	before, after := migratedSkeletonMachines(t)
	for _, scenario := range []struct {
		name          string
		retry, repass bool
	}{
		{name: "success and declared repo handoff"},
		{name: "policy retry", retry: true},
		{name: "review repass", repass: true},
		{name: "policy retry followed by review repass", retry: true, repass: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			original := runMigratedSkeleton(t, before, scenario.retry, scenario.repass)
			migrated := runMigratedSkeleton(t, after, scenario.retry, scenario.repass)
			expected := expectedMigratedJournal(t, original, migrated, before.Digest(), after.Digest())
			if !slices.Equal(expected, migrated.events) {
				for i := range min(len(expected), len(migrated.events)) {
					if expected[i] != migrated.events[i] {
						t.Fatalf("normative event %d changed during migration:\nexpected: %+v\n3.0: %+v", i, expected[i], migrated.events[i])
					}
				}
				t.Fatalf("normative event count changed during migration: 2.0=%d, 3.0=%d", len(expected), len(migrated.events))
			}
		})
	}
}

func migratedSkeletonMachines(t *testing.T) (*workflow.Machine, *workflow.Machine) {
	t.Helper()
	doc := apiv1.Workflow{
		TypeMeta:   metav1.TypeMeta{APIVersion: "goobers.dev/v1alpha1", Kind: "Workflow"},
		ObjectMeta: metav1.ObjectMeta{Name: "walking-skeleton"},
		DSLVersion: "2.0", Spec: skeletonMachine(t).Def.Spec,
	}
	doc.Spec.Tasks[1].Run.Env = map[string]string{e2eCommandHelperMode: "read-file", e2eCommandHelperPath: "impl.txt"}
	compile := func(doc apiv1.Workflow) *workflow.Machine {
		machine, err := workflow.Compile(workflow.Definition{Name: doc.Name, Version: 1, DSLVersion: doc.DSLVersion, Spec: doc.Spec}, workflow.WithPreviewFeatures(true))
		if err != nil {
			t.Fatal(err)
		}
		return machine
	}
	before := compile(doc)
	source, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	result, err := dslmigrate.Migrate(source, "3.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(result.After), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.DSLVersion != "3.0" || !slices.Equal([]string(doc.Spec.Tasks[1].RepoFrom), []string{"implement"}) {
		t.Fatalf("migration did not declare the expected repo handoff: %s", result.After)
	}
	after := compile(doc)
	if before.Digest() == after.Digest() {
		t.Fatal("a version migration must change the pinned workflow identity")
	}
	return before, after
}

type migrationJournal struct {
	events   []journal.NormativeEvent
	episodes map[string][]byte
}

func runMigratedSkeleton(t *testing.T, machine *workflow.Machine, retry, repass bool) migrationJournal {
	t.Helper()
	r, runsDir := newSkeletonRunner(t, func(call int) interface{} {
		if retry && call == 1 {
			return dispatchFailure{err: fmt.Errorf("transient dispatch failure")}
		}
		return resultPayload(apiv1.ResultSuccess, "implemented")
	}, func(call int) interface{} {
		if repass && call == 1 {
			return verdictPayload(apiv1.VerdictNeedsChanges, "add a test for the new branch")
		}
		return verdictPayload(apiv1.VerdictPass, "looks good")
	})
	const runID = "11111111111111111111111111111111"
	result, err := r.Start(context.Background(), skeletonStartInput(runID, machine))
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("DSL %s Start: %+v, %v", machine.Def.DSLVersion, result, err)
	}
	reader, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.WorkflowDigest != machine.Digest() {
		t.Fatalf("journal pinned %s instead of %s", identity.WorkflowDigest, machine.Digest())
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	checkMigratedSkeletonObservations(t, reader, events, retry, repass)
	episodes := map[string][]byte{}
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || !strings.HasPrefix(event.Name, "learning/episode-") || event.Ref == nil {
			continue
		}
		payload, err := reader.ArtifactBytes(*event.Ref)
		if err != nil {
			t.Fatal(err)
		}
		episodes[event.Name] = payload
	}
	if repass && len(episodes) != 1 {
		t.Fatalf("review repass produced %d learning episodes, want one", len(episodes))
	}
	return migrationJournal{events: journal.ConformanceView(events), episodes: episodes}
}

// Learning episodes deliberately pin the workflow digest, so a migrated
// definition cannot produce a byte-identical learning artifact. Prove the
// exact expected content change rather than discard the artifact from the
// normative view: its bytes must differ solely by the workflow digest and
// its derived effective-version identity (the harness pins no goober digest).
// All other event fields, artifact content, and ordering remain comparable.
func expectedMigratedJournal(t *testing.T, original, migrated migrationJournal, oldDigest, newDigest string) []journal.NormativeEvent {
	t.Helper()
	if len(original.episodes) != len(migrated.episodes) {
		t.Fatal("migration changed learning episode count")
	}
	expected := slices.Clone(original.events)
	for i := range expected {
		before, isEpisode := original.episodes[expected[i].Name]
		if !isEpisode {
			continue
		}
		after, found := migrated.episodes[expected[i].Name]
		oldEffective := learning.EffectiveVersion(oldDigest, "")
		newEffective := learning.EffectiveVersion(newDigest, "")
		expectedPayload := bytes.Replace(before, []byte(oldDigest), []byte(newDigest), 1)
		expectedPayload = bytes.Replace(expectedPayload, []byte(oldEffective), []byte(newEffective), 1)
		if !found || bytes.Count(before, []byte(oldDigest)) != 1 || bytes.Count(before, []byte(oldEffective)) != 1 || !bytes.Equal(expectedPayload, after) {
			t.Fatalf("learning episode %s changed beyond its pinned workflow identity:\n2.0: %s\n3.0: %s", expected[i].Name, before, after)
		}
		if expected[i].RefDigest != journal.Digest(before) {
			t.Fatal("original learning artifact digest does not match its content")
		}
		expected[i].RefDigest = journal.Digest(after)
	}
	return expected
}

func checkMigratedSkeletonObservations(t *testing.T, reader *journal.Reader, events []journal.Event, retry, repass bool) {
	t.Helper()
	producerCalls := 1
	if retry {
		producerCalls++
	}
	if repass {
		producerCalls++
	}
	var sawConsumer, sawRetry, sawRepass bool
	for _, event := range events {
		if event.Stage == "implement" && event.AttemptClass == journal.AttemptPolicy {
			sawRetry = true
		}
		if event.Verdict == string(apiv1.VerdictNeedsChanges) {
			sawRepass = true
		}
		if event.Type == journal.EventArtifactRecorded && strings.HasSuffix(event.Name, ":local-ci/stdout.log") && event.Ref != nil {
			payload, err := reader.ArtifactBytes(*event.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if string(payload) != fmt.Sprintf("coder change %d\n", producerCalls) {
				t.Fatalf("repo consumer read %q, want latest producer call %d", payload, producerCalls)
			}
			sawConsumer = true
		}
	}
	if !sawConsumer || sawRetry != retry || sawRepass != repass {
		t.Fatalf("scenario was not exercised: consumer=%v retry=%v repass=%v", sawConsumer, sawRetry, sawRepass)
	}
}
