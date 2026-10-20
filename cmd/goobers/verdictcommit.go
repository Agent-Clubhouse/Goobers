package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/findingledger"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// verdictPublication captures the predecessor BEFORE the provider's live-head
// check. Same-head publications can fold concurrently. A replacement of another
// head must still see this exact predecessor: a delayed old-head review cannot
// overwrite a newer head's record after its live-head check.
type verdictPublication struct {
	store    stateclient.Store
	key      string
	observed stateclient.Value
	root     string
	repo     providers.RepositoryRef
	number   int
}

func beginVerdictPublication(root string, repo providers.RepositoryRef, number int) (verdictPublication, error) {
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		return verdictPublication{}, fmt.Errorf("open remediation-verdict state: %w", err)
	}
	publication := verdictPublication{store: store, key: verdictRecordKey(repo, number), root: root, repo: repo, number: number}
	publication.observed, err = store.Get(stateContext(), verdictStateKey(publication.key))
	if err == nil {
		_, _, err = decodeVerdictRecord(publication.observed, publication.key)
	}
	if err != nil {
		return verdictPublication{}, fmt.Errorf("capture remediation-verdict predecessor: %w", err)
	}
	return publication, nil
}

// commit returns only the successfully committed fold. finalize is pure and
// may run repeatedly after a CAS conflict; provider and journal writes belong
// after commit returns. The original candidate remains unchanged on every try.
func (p verdictPublication) commit(candidate apiv1.Verdict, legacy []apiv1.Verdict, finalize func(apiv1.Verdict) (apiv1.Verdict, error), stdout io.Writer) (apiv1.Verdict, error) {
	var committed apiv1.Verdict
	err := withVerdictPublicationCustody(stateContext(), p.root, p.repo, func(ctx context.Context) error {
		var commitErr error
		committed, commitErr = p.commitHeld(ctx, candidate, legacy, finalize, stdout)
		return commitErr
	})
	if err != nil {
		return apiv1.Verdict{}, err
	}
	return committed, nil
}

func (p verdictPublication) commitHeld(ctx context.Context, candidate apiv1.Verdict, legacy []apiv1.Verdict, finalize func(apiv1.Verdict) (apiv1.Verdict, error), stdout io.Writer) (apiv1.Verdict, error) {
	var committed apiv1.Verdict
	var result findingledger.FoldResult
	err := p.store.Update(ctx, verdictStateKey(p.key), verdictStateLockOperation, func(value stateclient.Value) ([]byte, bool, error) {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		current, exists, err := decodeVerdictRecord(value, p.key)
		if err != nil {
			return nil, false, err
		}
		if current.HeadSHA != candidate.HeadSHA && value.ETag != p.observed.ETag {
			return nil, false, fmt.Errorf("verdict predecessor changed to head %s generation %d; re-check the live PR before publishing head %s", current.HeadSHA, current.LedgerGeneration, candidate.HeadSHA)
		}
		prior := append([]apiv1.Verdict(nil), legacy...)
		if exists {
			prior = append(prior, current)
		}
		generation := current.LedgerGeneration
		for _, record := range prior {
			generation = max(generation, record.LedgerGeneration)
		}
		if generation == math.MaxUint64 {
			return nil, false, fmt.Errorf("verdict generation exhausted")
		}
		next, folded := findingledger.Commit(prior, candidate, generation+1, prFindingPolicy(p.number))
		if len(folded.Standing) > 0 {
			note := fmt.Sprintf("Standing from an earlier review of this head, never resolved or rejected: %s.", strings.Join(folded.Standing, ", "))
			if !strings.Contains(next.Rationale, note) {
				next.Rationale = strings.TrimSpace(next.Rationale + "\n\n" + note)
			}
		}
		if finalize != nil {
			next, err = finalize(next)
			if err != nil {
				return nil, false, err
			}
		}
		if err := validateVerdictForPublish(next); err != nil {
			return nil, false, err
		}
		data, err := encodeKeyedStateRecord(p.key, next, verdictRecordSpec)
		if err != nil {
			return nil, false, err
		}
		committed, result = next, folded
		return data, true, nil
	})
	if err != nil {
		return apiv1.Verdict{}, fmt.Errorf("persist remediation-verdict state for PR #%d: %w", p.number, err)
	}
	p.record(committed, result, stdout)
	return committed, nil
}

func (p verdictPublication) record(committed apiv1.Verdict, result findingledger.FoldResult, stdout io.Writer) {
	if len(result.Standing) > 0 {
		action := "carry-findings"
		if result.Downgraded {
			action = "downgrade-pass"
			pf(stdout, "PR #%d: review left %d earlier finding(s) unanswered (%s); committed %s\n", p.number, len(result.Standing), strings.Join(result.Standing, ", "), committed.Decision)
		}
		recordMergeSafetyBlock(p.root, "apply-verdict", action, p.number, committed.HeadSHA, committed.BaseSHA, result.Standing)
	}
	if annotations, err := openStageAnnotator(instance.NewLayout(p.root)); err == nil {
		_ = annotations.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{
			"annotation": "remediation-verdict", "key": p.key, "decision": string(committed.Decision), "generation": committed.LedgerGeneration,
		}})
		_ = annotations.Close()
	}
}

// finalizeFindingHistory computes routing from the committed finding set,
// including any findings discovered on a CAS retry.
func finalizeFindingHistory(prior findingSetHistory, verdict apiv1.Verdict) (apiv1.Verdict, findingSetHistory, error) {
	history, digest, revisited, err := advanceFindingSetHistory(prior, verdict.Findings)
	if err != nil {
		return verdict, history, err
	}
	if revisited && verdict.Decision == apiv1.VerdictNeedsChanges && verdictLabel(verdict.Decision, verdict.Findings) == needsRemediationLabel {
		verdict.Decision = apiv1.VerdictEscalate
		verdict.ReasonCode = apiv1.VerdictReasonFindingOscillation
		verdict.Elected = false
		reason := fmt.Sprintf("Finding-set oscillation detected: `%s` matches an earlier merge-review state. Remediation returned to a prior unresolved finding set, so this PR is escalated instead of spending the remaining repass budget.", digest)
		verdict.Rationale = preserveReviewerRationale(reason, verdict.Rationale)
	}
	return verdict, history, nil
}
