package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// podresultartifact4119_test.go pins Goobers#4119: a pod-executed stage
// surrendered its declared result file as OUTPUTS ONLY and never as an
// artifact, so every cross-stage artifact read in the tree was blind on the
// pod substrate.
//
// MEASURED on the live cluster, run 247675bec416d742f76db37ab2794cf3:
// gather-pr-context finished SUCCESS with a full v3 brief in its outputs, and
// gather-ci-failures then failed with "gather-pr-context produced no
// remediation brief artifact". Three of those in a row parked PR #3900 with
// goobers:needs-human.
//
// Both halves are pinned here, because either one alone still leaves the lane
// dead: the pod must RECORD the artifact, and the readers must recognise it
// under the name the pod records it with.

// planeJournal stands up a real livejournal.Writer over HTTP against a real
// on-disk run journal and points the pod environment at it, exactly as
// TestRecordStageArtifactsStampsOpTime does — the artifact has to cross the
// process boundary to prove anything (L-153).
func planeJournal(t *testing.T, runID, stage string) string {
	t.Helper()
	root := t.TempDir()
	runsDir := filepath.Join(root, "runs")
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageStarted, Stage: stage, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "goobers" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, err := writer.Emit(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "emit_failed", "message": err.Error()}})
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, runID)
	t.Setenv(dispatcher.EnvGaggle, "goobers")
	t.Setenv(dispatcher.EnvStage, stage)
	t.Setenv(dispatcher.EnvAttempt, "1")
	return filepath.Join(runsDir, runID)
}

// A pod stage's declared result file must reach the run journal as an
// artifact, with the pointer on the surrendered envelope — the two halves the
// local executor has always emitted together (recordResultArtifact plus
// refToPointer(ref, MediaTypeFor(resultFile))).
func TestPodStageSurrendersItsResultFileAsAnArtifact(t *testing.T) {
	const runID = "run-4119-result"
	const stage = "gather-pr-context"
	runDir := planeJournal(t, runID, stage)

	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	brief := `{"schema":"goobers.dev/remediation-brief/v3","selectedNumber":"3900"}`
	t.Setenv(dispatcher.EnvStageCommand, fmt.Sprintf(`["sh","-c",%q]`,
		fmt.Sprintf("printf '%%s' '%s' > brief.json", brief)))
	t.Setenv(dispatcher.InputEnvVar("resultFile"), "brief.json")

	var out, errOut bytes.Buffer
	got := runDeclaredStage(context.Background(), &out, &errOut)
	if got.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q (%+v), want success", got.Status, got.Error)
	}

	// Half one: the ENVELOPE declares it, or journalclient.ResolveStageArtifact
	// has no digest to bind the artifact to its stage with.
	var pointer *apiv1.ArtifactPointer
	for i := range got.Artifacts {
		if got.Artifacts[i].MediaType == "application/json" {
			pointer = &got.Artifacts[i]
		}
	}
	if pointer == nil {
		t.Fatalf("no application/json artifact pointer on the envelope; a self runner declares one for every declared result file, got %+v", got.Artifacts)
	}

	// Half two: the JOURNAL records it, under a name whose stage segment is
	// this stage and whose suffix is /result.
	rd, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var found *journal.Event
	for i := range events {
		ev := &events[i]
		if ev.Type == journal.EventArtifactRecorded && ev.Ref != nil &&
			stageArtifactName(runID, ev.Name) == stage+"/result" {
			found = ev
		}
	}
	if found == nil {
		var names []string
		for i := range events {
			if events[i].Type == journal.EventArtifactRecorded {
				names = append(names, events[i].Name)
			}
		}
		t.Fatalf("the run journal records no %s/result artifact; recorded %v", stage, names)
	}
	if found.Ref.Digest != pointer.Digest {
		t.Fatalf("journal digest %q != envelope pointer digest %q: the pointer is derived from the recorded bytes and the two must address the same blob",
			found.Ref.Digest, pointer.Digest)
	}
	data, err := rd.ArtifactBytes(*found.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != brief {
		t.Fatalf("recorded artifact = %q, want the declared result file verbatim %q", data, brief)
	}
	// The stream artifacts stay text/plain; only the result file is typed
	// from its own name.
	for _, a := range got.Artifacts {
		if a.Digest != pointer.Digest && a.MediaType != "text/plain" {
			t.Fatalf("stream artifact declared %q, want text/plain", a.MediaType)
		}
	}
}

// A stage that exits non-zero still surrenders whatever result file it wrote:
// failProviderStage's structured self-report IS that file, and a downstream
// reader (or an operator) that cannot see it is reading exit codes instead of
// evidence.
func TestPodStageSurrendersTheResultFileOfAFailingStage(t *testing.T) {
	const runID = "run-4119-failed"
	const stage = "rebase-pr"
	runDir := planeJournal(t, runID, stage)

	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	t.Setenv(dispatcher.EnvStageCommand, fmt.Sprintf(`["sh","-c",%q]`,
		`printf '%s' '{"errorCode":"provider_error","errorMessage":"nope"}' > r.json; exit 1`))
	t.Setenv(dispatcher.InputEnvVar("resultFile"), "r.json")

	var out, errOut bytes.Buffer
	if got := runDeclaredStage(context.Background(), &out, &errOut); got.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", got.Status)
	}

	rd, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if events[i].Type == journal.EventArtifactRecorded && stageArtifactName(runID, events[i].Name) == stage+"/result" {
			return
		}
	}
	t.Fatalf("a failing pod stage surrendered no %s/result artifact", stage)
}

// The brief reader must find the artifact under EITHER spelling. The local
// executor names it "<runID>:<stage>/result" and a pod names it
// "<stage>/result"; a reader that knows only one is substrate-dependent, which
// is the whole defect.
func TestRemediationBriefIsReadableUnderBothSubstrateSpellings(t *testing.T) {
	brief := remediationBriefFixture(false)
	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	for name, recordedAs := range map[string]string{
		"self runner": "%s:gather-pr-context/result",
		"pod":         "gather-pr-context/result",
	} {
		t.Run(name, func(t *testing.T) {
			const runID = "run-4119-read"
			root := initDemo(t)
			run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
				RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			recorded := recordedAs
			if strings.Contains(recorded, "%s") {
				recorded = fmt.Sprintf(recorded, runID)
			}
			if _, err := run.RecordArtifact(recorded, data); err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			got, err := readRemediationBriefArtifact(root, runID, "gather-pr-context")
			if err != nil {
				t.Fatalf("read brief recorded as %q: %v", recorded, err)
			}
			if got.SelectedNumber != brief.SelectedNumber {
				t.Fatalf("selectedNumber = %q, want %q", got.SelectedNumber, brief.SelectedNumber)
			}
		})
	}
}

// An artifact belonging to a DIFFERENT stage must stay invisible under either
// spelling: relaxing the run qualifier must not relax the stage attribution.
func TestBriefReaderStillRefusesAnotherStagesResult(t *testing.T) {
	const runID = "run-4119-other"
	root := initDemo(t)
	data, err := json.Marshal(remediationBriefFixture(false))
	if err != nil {
		t.Fatal(err)
	}
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "pr-remediation", Gaggle: "goobers",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordArtifact("update-behind-pr/result", data); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemediationBriefArtifact(root, runID, "gather-pr-context"); err == nil {
		t.Fatal("update-behind-pr's result was accepted as gather-pr-context's brief")
	}
}

func TestStageArtifactNameStripsOnlyTheRunQualifier(t *testing.T) {
	for _, tc := range []struct{ runID, recorded, wantName, wantStage string }{
		{"r1", "r1:gather-pr-context/result", "gather-pr-context/result", "gather-pr-context"},
		{"r1", "gather-pr-context/result", "gather-pr-context/result", "gather-pr-context"},
		{"r1", "context/gather-pr-context-attempt-1.json", "context/gather-pr-context-attempt-1.json", "context"},
		{"", "gather-pr-context/result", "gather-pr-context/result", "gather-pr-context"},
		{"r1", "finding.md", "finding.md", ""},
		// A run id that merely PREFIXES the name is not a qualifier.
		{"r1", "r1x:local-ci/stdout.log", "r1x:local-ci/stdout.log", "r1x:local-ci"},
	} {
		if got := stageArtifactName(tc.runID, tc.recorded); got != tc.wantName {
			t.Errorf("stageArtifactName(%q, %q) = %q, want %q", tc.runID, tc.recorded, got, tc.wantName)
		}
		if got := stageArtifactStage(tc.runID, tc.recorded); got != tc.wantStage {
			t.Errorf("stageArtifactStage(%q, %q) = %q, want %q", tc.runID, tc.recorded, got, tc.wantStage)
		}
	}
}

// No production reader may rebuild a stage-artifact name by concatenating the
// run id onto a stage name. That is the exact shape that made
// gather-ci-failures, gather-issue-context and open-pr-body blind on pods, and
// it reads as obviously correct every time someone writes it.
func TestNoStageArtifactReaderHardCodesTheRunQualifier(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// runID + ":" + stage, or the same thing spelled as a format string.
	concat := regexp.MustCompile(`(?i)run(ID)?\s*\+\s*":|%s:[a-z-]+/`)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			// The one legitimate site: the helper that STRIPS the qualifier.
			if name == "stagejournal.go" && strings.Contains(line, "strings.TrimPrefix(recorded, runID+\":\")") {
				continue
			}
			if concat.MatchString(line) {
				t.Errorf("%s:%d builds a stage-artifact name from the run id: %s\nuse stageArtifactName/stageArtifactStage — a pod does not record the qualifier (#4119)",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}
