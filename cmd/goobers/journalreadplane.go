package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/localscheduler"
)

// journalreadplane.go is the daemon side of decision 005 R1 / finding 002 C4's
// cross-run journal plane. The same-run half needs no service at all — it is
// the existing readservice routes with handler-level run containment added
// (internal/httpapi/router.go, podRunContained).
//
// Every method here answers a purpose-built question from the instance's own
// run directories through journalclient.FileCrossRun — the identical code the
// same-host CLI path runs — so "the daemon answered it" and "the stage read it
// off disk" cannot drift into two different answers. What the daemon adds is
// the containment the filesystem cannot express:
//
//   - the asking run must belong to the gaggle it names (runJournalGaggleOK);
//   - a cross-run phase lookup must target a run in THAT gaggle, so a pod
//     cannot enumerate phases across the instance;
//   - the stranded-work question is answered for the items the LEDGER says the
//     asking run holds, never for items the request named.

// claimLockOperationAPIUnpushedWork labels the ledger read the unpushed-work
// route makes, alongside writeplanes.go's claims-plane labels.
const claimLockOperationAPIUnpushedWork = "api.journal.unpushed-work"

// daemonRunJournalService serves the cross-run journal plane.
type daemonRunJournalService struct {
	layout instance.Layout
	log    *journal.InstanceLog
}

func newDaemonRunJournalService(layout instance.Layout, log *journal.InstanceLog) *daemonRunJournalService {
	return &daemonRunJournalService{layout: layout, log: log}
}

// runJournalGaggleOK reports whether runID's journal lives under gaggle's runs
// directory on this instance — the same run.yaml lookup daemonClaimService's
// namespace containment makes, and the same fail-closed treatment of an
// unsafe gaggle segment.
func (s *daemonRunJournalService) runJournalGaggleOK(gaggle, runID string) bool {
	if !apiv1.ValidRunID(runID) || !plainPathElement(gaggle) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID, "run.yaml"))
	return err == nil
}

func gaggleMismatch(what string) error {
	return httpapi.NewInterventionError(http.StatusForbidden, "gaggle_mismatch",
		"the asking run does not belong to the named gaggle; "+what+" is refused", nil)
}

// crossRun is the shared reader, scoped per request by the caller.
func (s *daemonRunJournalService) crossRun() *journalclient.FileCrossRun {
	return journalclient.NewFileCrossRun(s.layout)
}

// RunPhase answers the phase of one prior run in the asking run's gaggle.
//
// Two containment checks, both required: the asking run must be in the gaggle
// it names, and so must the target. Without the second, a pod could learn the
// terminal phase of any run on the instance by naming its own gaggle and some
// other gaggle's run id.
func (s *daemonRunJournalService) RunPhase(ctx context.Context, request journalclient.RunPhaseRequest) (journalclient.RunPhaseResponse, error) {
	if !apiv1.ValidRunID(request.RunID) || !apiv1.ValidRunID(request.TargetRunID) {
		return journalclient.RunPhaseResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"runId and targetRunId are required and must be valid run ids", nil)
	}
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		return journalclient.RunPhaseResponse{}, gaggleMismatch("a cross-run phase lookup")
	}
	if !s.runJournalGaggleOK(request.Gaggle, request.TargetRunID) {
		// Deliberately the same refusal a foreign gaggle gets, and deliberately
		// NOT a 404: "no such run" and "that run is not yours to ask about" must
		// be indistinguishable, or the route becomes a run-id oracle.
		return journalclient.RunPhaseResponse{}, gaggleMismatch("a cross-run phase lookup")
	}
	phase, err := s.crossRun().RunPhase(ctx, request.TargetRunID)
	if err != nil {
		if errors.Is(err, journalclient.ErrRunNotFound) {
			return journalclient.RunPhaseResponse{}, httpapi.NewInterventionError(http.StatusNotFound, "not_found",
				"the named run has no readable journal on this instance", nil)
		}
		return journalclient.RunPhaseResponse{}, err
	}
	return journalclient.RunPhaseResponse{RunID: request.TargetRunID, Phase: string(phase)}, nil
}

// ConflictTouches answers the gaggle's base-sync conflict history: run ids and
// the file paths those runs conflicted over, nothing else.
func (s *daemonRunJournalService) ConflictTouches(ctx context.Context, request journalclient.ConflictTouchRequest) (journalclient.ConflictTouchResponse, error) {
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		return journalclient.ConflictTouchResponse{}, gaggleMismatch("a conflict-history read")
	}
	touches, err := s.crossRun().ConflictTouches(ctx, journalclient.ConflictTouchRequest{
		RunID:  request.RunID,
		Gaggle: request.Gaggle,
		Since:  request.Since,
	})
	if err != nil {
		return journalclient.ConflictTouchResponse{}, err
	}
	return journalclient.ConflictTouchResponse{Touches: touches}, nil
}

// UnpushedWork answers the stranded-diff question for the items the asking run
// actually holds.
//
// The item set comes from the ledger, never from the request: a pod that could
// name items would be able to read any other run's stranded diff by naming its
// item. The httpapi handler already blanks the request's list; deriving here
// as well means no other caller of this service can reintroduce the hole.
func (s *daemonRunJournalService) UnpushedWork(ctx context.Context, request journalclient.UnpushedWorkRequest) (journalclient.UnpushedWorkResponse, error) {
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		return journalclient.UnpushedWorkResponse{}, gaggleMismatch("a prior-unpushed-work read")
	}
	itemIDs, err := s.claimedItemIDs(request.Gaggle, request.RunID)
	if err != nil {
		return journalclient.UnpushedWorkResponse{}, err
	}
	if len(itemIDs) == 0 {
		return journalclient.UnpushedWorkResponse{}, nil
	}
	work, err := s.crossRun().UnpushedWork(ctx, journalclient.UnpushedWorkRequest{
		RunID:              request.RunID,
		Gaggle:             request.Gaggle,
		Since:              request.Since,
		ItemIDs:            itemIDs,
		MaxInlineDiffBytes: request.MaxInlineDiffBytes,
	})
	if err != nil {
		return journalclient.UnpushedWorkResponse{}, err
	}
	return journalclient.UnpushedWorkResponse{Work: work}, nil
}

// EscalationCandidates answers the gaggle's outstanding decomposition
// escalation candidates (#4342): the same
// decomposition.FindEscalationCandidates scan select-source ran directly off
// disk before this route existed, run here over the SAME FileCrossRun this
// service backs every other cross-run question with — so a pod and a
// self-runner select-source can never see a different candidate set.
func (s *daemonRunJournalService) EscalationCandidates(ctx context.Context, request journalclient.EscalationCandidatesRequest) (journalclient.EscalationCandidatesResponse, error) {
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		return journalclient.EscalationCandidatesResponse{}, gaggleMismatch("a decomposition escalation-candidates read")
	}
	candidates, err := s.crossRun().EscalationCandidates(ctx, journalclient.EscalationCandidatesRequest{
		RunID:  request.RunID,
		Gaggle: request.Gaggle,
	})
	if err != nil {
		return journalclient.EscalationCandidatesResponse{}, err
	}
	return journalclient.EscalationCandidatesResponse{Candidates: candidates}, nil
}

// BranchOwnership answers whether req.TargetRunID's journal actually owns
// req.Branch, in the asking run's gaggle.
//
// Two containment checks, both required, mirroring RunPhase: the asking run
// must be in the gaggle it names, and so must the target — without the
// second, a pod could ask about any run on the instance by naming its own
// gaggle and someone else's run id, learning that run's identity and phase
// through a plausible-looking branch name it need not actually reference.
func (s *daemonRunJournalService) BranchOwnership(ctx context.Context, request journalclient.BranchOwnershipRequest) (journalclient.BranchOwnershipResponse, error) {
	if !apiv1.ValidRunID(request.RunID) || !apiv1.ValidRunID(request.TargetRunID) {
		return journalclient.BranchOwnershipResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"runId and targetRunId are required and must be valid run ids", nil)
	}
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		return journalclient.BranchOwnershipResponse{}, gaggleMismatch("a branch-ownership lookup")
	}
	if !s.runJournalGaggleOK(request.Gaggle, request.TargetRunID) {
		// Deliberately the same refusal a foreign gaggle gets (see RunPhase).
		return journalclient.BranchOwnershipResponse{}, gaggleMismatch("a branch-ownership lookup")
	}
	return s.crossRun().BranchOwnership(ctx, journalclient.BranchOwnershipRequest{
		RunID: request.RunID, Gaggle: request.Gaggle, TargetRunID: request.TargetRunID,
		Workflow: request.Workflow, Branch: request.Branch,
	})
}

// claimedItemIDs lists the items runID currently holds, live or expired —
// ForRunAll's contract, the same set the same-host caller reads through
// claimedItemIDsForRun. Held claims only: released history is deliberately NOT
// included, because a released claim is a run that finished with that item and
// has no continuing entitlement to its stranded work.
func (s *daemonRunJournalService) claimedItemIDs(gaggle, runID string) ([]string, error) {
	var ids []string
	lockPath := filepath.Join(s.layout.SchedulerDir(), claimLockFileName)
	err := withClaimLockForRun(lockPath, claimLockOperationAPIUnpushedWork, gaggle, runID, func() error {
		opts := []localscheduler.LedgerOption{}
		if s.log != nil {
			opts = append(opts, localscheduler.WithInstanceLog(s.log))
		}
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName), opts...)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, entry := range ledger.ForRunAll(runID) {
			if entry.ItemID != "" {
				ids = append(ids, entry.ItemID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

var _ httpapi.RunJournalService = (*daemonRunJournalService)(nil)
