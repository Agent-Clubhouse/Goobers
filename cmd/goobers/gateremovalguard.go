package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/tutorguard"
	"github.com/goobers/goobers/providers"
)

// gateRemovalGuardHelp documents `goobers gate-removal-guard`, TUT-A3's
// enforcement point (#1215, docs/design/tutor-redesign.md §5 item 2): the
// Tutor may never remove or loosen the exact gate whose noise (gate-never-
// fails / gate-repass-churn) produced this run's finding, without the
// analyst having cited independent proof the gate is dead.
const gateRemovalGuardHelp = "Usage: goobers gate-removal-guard [path]\n\n" +
	"Block a tutor run whose drafted change removes or loosens the specific\n" +
	"gate its own finding flagged as noisy, unless the finding cites\n" +
	"independent proof the gate is dead. Runs after draft-change, before\n" +
	"validate-config, reading the analyze stage's finding.md from the run\n" +
	"journal and diffing every changed workflow YAML file against base.\n" +
	"A finding unrelated to gate noise, or a diff that doesn't touch the\n" +
	"named gate, is a no-op pass-through.\n" +
	"Exit codes: 0 = clear (or nothing to check), 1 = blocked, 2 = usage/IO error.\n"

func runGateRemovalGuard(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gate-removal-guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gate-removal-guard")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	base := providerInput("base", providerBaseBranch())
	resultFile := providerInput("resultFile", "gate-edit.json")

	meta, findErr := findingMetaFromJournal(root, runID)
	if findErr != nil {
		pf(stderr, "error: read finding.md from journal: %v\n", findErr)
		return 1
	}
	if !meta.IsGateNoise() || meta.Subject == "" {
		if err := writeGateEditResult(resultFile, string(tutorguard.GateEditNone), ""); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pln(stdout, "gate-removal-guard: finding is not gate-noise — nothing to check")
		return 0
	}

	changed, err := changedFilesVsBase(base)
	if err != nil {
		pf(stderr, "error: gate-removal guard: compute changed files vs %q: %v\n", base, err)
		return 1
	}

	kind := tutorguard.GateEditNone
	for _, path := range changed {
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			continue
		}
		oldYAML, oldErr := gitShowOrEmpty(base, path)
		if oldErr != nil {
			pf(stderr, "error: gate-removal guard: read %s at %s: %v\n", path, base, oldErr)
			return 1
		}
		newYAML, readErr := os.ReadFile(filepath.Join(".", path))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				newYAML = nil
			} else {
				pf(stderr, "error: gate-removal guard: read %s: %v\n", path, readErr)
				return 1
			}
		}
		fileKind, classifyErr := tutorguard.ClassifyGateEdit(oldYAML, newYAML, meta.Subject)
		if classifyErr != nil {
			// A workflow file this run touched fails to parse as YAML — fail
			// closed rather than silently skipping a file that might hide the
			// exact edit this guard exists to catch.
			pf(stderr, "error: gate-removal guard: classify %s: %v\n", path, classifyErr)
			return 1
		}
		if worseGateEdit(fileKind, kind) {
			kind = fileKind
		}
	}

	if kind.RequiresIndependentProof() && !meta.HasIndependentProof() {
		pf(stderr,
			"error: gate-removal guard: this run %s gate %q, the exact gate whose noise (%s) produced this run's finding, with no independent proof it is dead (TUT-A3, #1215) — blocking\n",
			kind, meta.Subject, meta.Kind)
		return 1
	}

	if err := writeGateEditResult(resultFile, string(kind), meta.Subject); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "gate-removal-guard: %s\n", kind)
	return 0
}

// worseGateEdit orders classifications by how much scrutiny they demand, so
// scanning several changed workflow files keeps the most severe one found —
// a run that removes the gate in one file and merely tunes it in another
// must still be classified "removed".
func worseGateEdit(candidate, current tutorguard.GateEditKind) bool {
	rank := map[tutorguard.GateEditKind]int{
		tutorguard.GateEditNone:     0,
		tutorguard.GateEditTuning:   1,
		tutorguard.GateEditLoosened: 2,
		tutorguard.GateEditRemoved:  3,
	}
	return rank[candidate] > rank[current]
}

// findingMetaFromJournal recovers the analyst's finding.md front matter from
// the run journal: the analyze stage's artifact, whatever its declared
// artifact name (the analyst goober writes exactly one such artifact per
// run.md, per instructions.md). A finding.md with no front matter, or no
// analyze-stage artifact at all (a workflow that doesn't have one, or an
// unreadable journal), is not itself an error — it just means there's
// nothing for this guard to check, and the caller treats it as such.
func findingMetaFromJournal(root, runID string) (tutorguard.FindingMeta, error) {
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		// "This host has no journal for the run" stays the benign case it has
		// always been. A journal that exists but could not be READ — a refused
		// or failed run-scoped plane read (#3880) — is an error, because
		// "unreadable" must never be indistinguishable from "no gate edit".
		if errors.Is(err, journalclient.ErrRunNotFound) {
			return tutorguard.FindingMeta{}, nil
		}
		return tutorguard.FindingMeta{}, fmt.Errorf("open run journal: %w", err)
	}
	events, err := rd.Events()
	if err != nil {
		return tutorguard.FindingMeta{}, fmt.Errorf("read run journal: %w", err)
	}

	var artifacts []journalArtifact
	var analyzeArtifacts []journal.Ref
	for i := range events {
		ev := &events[i]
		if ev.Type == journal.EventArtifactRecorded && ev.Ref != nil {
			artifacts = append(artifacts, journalArtifact{name: ev.Name, ref: *ev.Ref})
		}
		if ev.Type == journal.EventStageFinished && ev.Stage == "analyze" {
			analyzeArtifacts = ev.Artifacts
		}
	}
	if len(analyzeArtifacts) == 0 {
		return tutorguard.FindingMeta{}, nil
	}

	var findingData []byte
	if ref, ok := stageArtifactByName(artifacts, analyzeArtifacts, runID, "finding.md"); ok {
		findingData, err = rd.ArtifactBytes(ref)
	} else {
		// Fall back to the analyze stage's single declared artifact, whatever
		// its recorded name — the analyst writes exactly one.
		if len(analyzeArtifacts) == 1 {
			findingData, err = rd.ArtifactBytes(analyzeArtifacts[0])
		}
	}
	if err != nil || len(findingData) == 0 {
		return tutorguard.FindingMeta{}, nil
	}

	meta, parseErr := tutorguard.ParseFindingMarkdown(findingData)
	if parseErr != nil {
		return tutorguard.FindingMeta{}, nil
	}
	return meta, nil
}

// gitShowOrEmpty returns path's content at ref, or nil if the path did not
// exist at that revision — a run that adds a brand-new workflow file must
// never read as removing a gate that never existed before this run.
func gitShowOrEmpty(ref, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", ref+":"+path)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// writeGateEditResult writes gate-removal-guard's declared result file: the
// classification this run's diff earned against the flagged gate (or "none"),
// and which gate it checked. A later stage (open-pr) reads this back out of
// the journal to route the PR for review accordingly.
func writeGateEditResult(resultFile, gateEdit, subject string) error {
	out := map[string]string{"gateEdit": gateEdit}
	if subject != "" {
		out["subject"] = subject
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal gate-edit result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}

// tutorGateRemovalLabel/tutorGateTuningLabel route a tutor-authored PR for
// review commensurate with what it actually did to the gate its own finding
// flagged (TUT-A3, #1215): a removal/loosening (only ever possible when the
// analyst recorded independent proof — gate-removal-guard blocks it
// otherwise) is a materially riskier change than an ordinary tuning edit and
// gets the stricter label.
const (
	tutorGateRemovalLabel = "tutor:gate-removal"
	tutorGateTuningLabel  = "tutor:gate-tuning"
)

// gateEditClassificationFromJournal recovers the gate-removal-guard stage's
// classification of this run's diff from the journal — the same
// scalar-outputs-merged-from-resultFile pattern claimedIssueFromJournal uses,
// letting open-pr (several stages later) reach back to gate-removal-guard's
// verdict without a fragile multi-hop InputsFrom chain. Returns ("", "") when
// no such stage ran (a non-tutor workflow) or its outputs are unreadable.
func gateEditClassificationFromJournal(root, runID string) (kind, subject string, err error) {
	rd, err := stageRunJournal(root, runID)
	if err != nil {
		// As in findingMetaFromJournal: absent journal is "no classification",
		// unreadable journal is an error the caller reports rather than
		// silently routing the PR to the lighter review path (#3880).
		if errors.Is(err, journalclient.ErrRunNotFound) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("open run journal: %w", err)
	}
	events, err := rd.Events()
	if err != nil {
		return "", "", fmt.Errorf("read run journal: %w", err)
	}
	for _, ev := range events {
		if ev.Type != journal.EventStageFinished || ev.Stage != "gate-removal-guard" || ev.Outputs == nil {
			continue
		}
		gotKind, ok := ev.Outputs["gateEdit"].(string)
		if !ok {
			continue
		}
		gotSubject, _ := ev.Outputs["subject"].(string)
		return gotKind, gotSubject, nil
	}
	return "", "", nil
}

// labelGateEdit applies the review-routing label matching kind, clearing the
// other one first so a repass that reclassifies (e.g. removal on attempt 1,
// tuning after the analyst revises) never leaves both labels on the PR.
func labelGateEdit(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prNumber int, kind, subject string) error {
	var add, remove []string
	switch kind {
	case string(tutorguard.GateEditRemoved), string(tutorguard.GateEditLoosened):
		add = []string{tutorGateRemovalLabel}
		remove = []string{tutorGateTuningLabel}
	case string(tutorguard.GateEditTuning):
		add = []string{tutorGateTuningLabel}
		remove = []string{tutorGateRemovalLabel}
	default:
		return nil
	}
	comment := fmt.Sprintf(
		"🛡️ **Gate-edit review routing** (TUT-A3, #1215): this run's diff was classified **%s** against gate `%s` — the exact gate whose noise produced this run's finding. %s",
		kind, subject, gateEditReviewNote(kind))
	_, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo, ID: fmt.Sprintf("%d", prNumber), AddLabels: add, RemoveLabels: remove, Comment: comment,
	})
	return err
}

func gateEditReviewNote(kind string) string {
	if kind == string(tutorguard.GateEditTuning) {
		return "This is an ordinary tuning edit; standard review applies."
	}
	return "This removes or loosens the gate's enforcement — gate-removal-guard only allowed it because the analyst cited independent proof the gate is dead; verify that proof before approving."
}
