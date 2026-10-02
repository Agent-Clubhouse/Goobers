package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journalclient"
)

// journalreadplane.go is decision 005 ruling R1, option 1 (finding 002's
// JOURNAL READ / CARRY-FORWARD section, plan step C4): the read half of the
// journal boundary for stage pods.
//
// It has two parts, and they are deliberately different shapes.
//
//  1. SAME-RUN. A pod principal may GET its OWN run's three read routes —
//     RunEventsPath, StageAttemptsPath, RunArtifactPath — and no others. The
//     authorizer admits the shape (runReadPlanePath); the handlers enforce
//     which run (podRunContained), exactly as the journal-emit and surrender
//     planes enforce theirs. What it serves is the daemon's existing scrubbed
//     projection, not the raw journal: journal-relative paths are omitted,
//     non-scalar stage outputs are dropped, and artifact bytes are the
//     already-redacted blobs the portal reads. The widening this represents
//     is bounded and stated: a compromised stage of run X learns what earlier
//     stages of run X produced. It learns nothing about any other run, and
//     transcripts (RunTranscriptPath) stay human-only — they are not on the
//     converted readers' input list, and the conservative reading of the
//     ruling admits only what the ruling enumerated.
//
//  2. CROSS-RUN. Purpose-built, gaggle-scoped questions, each answered by the
//     daemon from data the daemon derives. A pod never receives another run's
//     journal, event log, or artifact bytes through these: it receives a
//     phase string, a list of (run id, file names), one stranded diff for an
//     item the DAEMON confirmed the asking run holds, or one branch's owning
//     run identity and terminal/ref facts (BranchOwnership, #4344). Option 2
//     (carry verdicts forward by pointer) cannot serve any of these at all,
//     which is why R1 chose option 1 for them.

// RunJournalService is the daemon-side cross-run journal plane. The shipped
// implementation lives in cmd/goobers; the wire types are journalclient's own
// so the stage-side client and this transport cannot drift.
type RunJournalService interface {
	// RunPhase answers the terminal phase of req.TargetRunID.
	RunPhase(ctx context.Context, req journalclient.RunPhaseRequest) (journalclient.RunPhaseResponse, error)
	// ConflictTouches answers the gaggle's base-sync conflict history.
	ConflictTouches(ctx context.Context, req journalclient.ConflictTouchRequest) (journalclient.ConflictTouchResponse, error)
	// UnpushedWork answers the stranded-diff question for the asking run's
	// OWN claimed items, which the implementation derives rather than trusts.
	UnpushedWork(ctx context.Context, req journalclient.UnpushedWorkRequest) (journalclient.UnpushedWorkResponse, error)
	// EscalationCandidates answers the gaggle's outstanding decomposition
	// escalation candidates (#4342).
	EscalationCandidates(ctx context.Context, req journalclient.EscalationCandidatesRequest) (journalclient.EscalationCandidatesResponse, error)
	// BranchOwnership answers whether req.TargetRunID's journal actually
	// owns req.Branch (#4344).
	BranchOwnership(ctx context.Context, req journalclient.BranchOwnershipRequest) (journalclient.BranchOwnershipResponse, error)
}

// OperatorMessageService submits operator messages after service-level authorization.
type OperatorMessageService interface {
	SubmitOperatorMessage(context.Context, OperatorMessageSubmissionRequest) (OperatorMessageSubmissionResponse, error)
}

// WithRunJournalService enables the cross-run journal-plane routes.
func WithRunJournalService(service RunJournalService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API run journal service is required")
		}
		config.runJournal = service
		return nil
	}
}

// WithOperatorMessageService enables run-scoped operator-message submission routes.
func WithOperatorMessageService(service OperatorMessageService) HandlerOption {
	return func(config *handlerConfig) error {
		if service == nil {
			return errors.New("http API operator message service is required")
		}
		config.operatorMessages = service
		return nil
	}
}

// podRunContained enforces the same-run boundary on a route that names a run
// in its path. It reports whether the request may proceed, writing the
// refusal itself when it may not.
//
// Human principals are unaffected: they reached the handler through the role
// ladder and are not confined to any one run. A pod principal is confined to
// the run its token names — the identical rule the journal-emit, surrender,
// and credential planes apply, restated here because this is the read side.
func podRunContained(w http.ResponseWriter, request *http.Request, run, action string) bool {
	principal, ok := PrincipalFromRequest(request)
	if !ok || !IsPodPrincipal(principal) {
		return true
	}
	if !apiv1.ValidRunID(run) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "run id is not a safe path segment")
		return false
	}
	if principal.Subject != podPrincipalSubject(run) {
		writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only read its own run's "+action)
		return false
	}
	return true
}

func registerRunJournalPlaneRoutes(router *Router, config handlerConfig, errorLog *log.Logger) {
	service := config.runJournal
	router.Handle(apicontract.RouteJournalMergeAuthority, journalMergeAuthorityHandler(service, errorLog))
	router.Handle(apicontract.RouteJournalRunPhase, journalRunPhaseHandler(service, errorLog))
	router.Handle(apicontract.RouteJournalConflictTouches, journalConflictTouchesHandler(service, errorLog))
	router.Handle(apicontract.RouteJournalUnpushedWork, journalUnpushedWorkHandler(service, errorLog))
	router.Handle(apicontract.RouteJournalEscalationCandidates, journalEscalationCandidatesHandler(service, errorLog))
	router.Handle(apicontract.RouteJournalBranchOwnership, journalBranchOwnershipHandler(service, errorLog))
	router.Handle(apicontract.RouteOperatorMessageSubmit, operatorMessageSubmitHandler(config.operatorMessages, errorLog))
}

// journalPlaneUnavailable reports (and answers) whether service is nil — the
// same guard every cross-run route handler needs before touching it.
func journalPlaneUnavailable(service RunJournalService, w http.ResponseWriter) bool {
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "journal_unavailable", "the cross-run journal plane is not available from this server")
		return true
	}
	return false
}

type journalRoute[T any, R any] struct {
	operation    string
	action       string
	sharedFields func(*T) (runID, gaggle string)
	validate     func(http.ResponseWriter, *T) bool
	prepare      func(*T)
	call         func(context.Context, T) (R, error)
	normalize    func(*R)
}

func journalJSONHandler[T any, R any](service RunJournalService, errorLog *log.Logger, route journalRoute[T, R]) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if journalPlaneUnavailable(service, w) {
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input T
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		runID, gaggle := route.sharedFields(&input)
		if !validCrossRunRequest(w, runID, gaggle) {
			return
		}
		if route.validate != nil && !route.validate(w, &input) {
			return
		}
		if !podBodyRunContained(w, request, runID, route.action) {
			return
		}
		if route.prepare != nil {
			route.prepare(&input)
		}
		response, err := route.call(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, route.operation, err)
			return
		}
		if route.normalize != nil {
			route.normalize(&response)
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func journalRunPhaseHandler(service RunJournalService, errorLog *log.Logger) http.HandlerFunc {
	return journalJSONHandler(service, errorLog, journalRoute[journalclient.RunPhaseRequest, journalclient.RunPhaseResponse]{
		operation: "read run phase",
		action:    "read another run's phase",
		sharedFields: func(input *journalclient.RunPhaseRequest) (string, string) {
			return input.RunID, input.Gaggle
		},
		validate: func(w http.ResponseWriter, input *journalclient.RunPhaseRequest) bool {
			if !apiv1.ValidRunID(strings.TrimSpace(input.TargetRunID)) {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "targetRunId is required and must be a valid run id")
				return false
			}
			return true
		},
		call: func(ctx context.Context, input journalclient.RunPhaseRequest) (journalclient.RunPhaseResponse, error) {
			return service.RunPhase(ctx, input)
		},
	})
}

func journalConflictTouchesHandler(service RunJournalService, errorLog *log.Logger) http.HandlerFunc {
	return journalJSONHandler(service, errorLog, journalRoute[journalclient.ConflictTouchRequest, journalclient.ConflictTouchResponse]{
		operation: "read conflict touches",
		action:    "read its gaggle's conflict history",
		sharedFields: func(input *journalclient.ConflictTouchRequest) (string, string) {
			return input.RunID, input.Gaggle
		},
		validate: func(w http.ResponseWriter, input *journalclient.ConflictTouchRequest) bool {
			if input.Since.IsZero() {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "since is required; an unbounded conflict-history walk is refused")
				return false
			}
			return true
		},
		call: func(ctx context.Context, input journalclient.ConflictTouchRequest) (journalclient.ConflictTouchResponse, error) {
			return service.ConflictTouches(ctx, input)
		},
		normalize: func(response *journalclient.ConflictTouchResponse) {
			if response.Touches == nil {
				response.Touches = []journalclient.ConflictTouch{}
			}
		},
	})
}

func journalUnpushedWorkHandler(service RunJournalService, errorLog *log.Logger) http.HandlerFunc {
	return journalJSONHandler(service, errorLog, journalRoute[journalclient.UnpushedWorkRequest, journalclient.UnpushedWorkResponse]{
		operation: "read prior unpushed work",
		action:    "read prior unpushed work",
		sharedFields: func(input *journalclient.UnpushedWorkRequest) (string, string) {
			return input.RunID, input.Gaggle
		},
		validate: func(w http.ResponseWriter, input *journalclient.UnpushedWorkRequest) bool {
			if input.Since.IsZero() {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "since is required; an unbounded unpushed-work walk is refused")
				return false
			}
			if input.MaxInlineDiffBytes < 0 || input.MaxInlineDiffBytes > MaxUnpushedWorkInlineDiffBytes {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "maxInlineDiffBytes is out of range")
				return false
			}
			return true
		},
		prepare: func(input *journalclient.UnpushedWorkRequest) {
			input.ItemIDs = nil
		},
		call: func(ctx context.Context, input journalclient.UnpushedWorkRequest) (journalclient.UnpushedWorkResponse, error) {
			return service.UnpushedWork(ctx, input)
		},
	})
}

func journalEscalationCandidatesHandler(service RunJournalService, errorLog *log.Logger) http.HandlerFunc {
	return journalJSONHandler(service, errorLog, journalRoute[journalclient.EscalationCandidatesRequest, journalclient.EscalationCandidatesResponse]{
		operation: "read decomposition escalation candidates",
		action:    "read its gaggle's decomposition escalation candidates",
		sharedFields: func(input *journalclient.EscalationCandidatesRequest) (string, string) {
			return input.RunID, input.Gaggle
		},
		call: func(ctx context.Context, input journalclient.EscalationCandidatesRequest) (journalclient.EscalationCandidatesResponse, error) {
			return service.EscalationCandidates(ctx, input)
		},
		normalize: func(response *journalclient.EscalationCandidatesResponse) {
			if response.Candidates == nil {
				response.Candidates = []journalclient.EscalationCandidate{}
			}
		},
	})
}

func journalBranchOwnershipHandler(service RunJournalService, errorLog *log.Logger) http.HandlerFunc {
	return journalJSONHandler(service, errorLog, journalRoute[journalclient.BranchOwnershipRequest, journalclient.BranchOwnershipResponse]{
		operation: "read branch ownership",
		action:    "read another run's branch ownership",
		sharedFields: func(input *journalclient.BranchOwnershipRequest) (string, string) {
			return input.RunID, input.Gaggle
		},
		validate: func(w http.ResponseWriter, input *journalclient.BranchOwnershipRequest) bool {
			if !apiv1.ValidRunID(strings.TrimSpace(input.TargetRunID)) {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "targetRunId is required and must be a valid run id")
				return false
			}
			if strings.TrimSpace(input.Workflow) == "" || strings.TrimSpace(input.Branch) == "" {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "workflow and branch are required")
				return false
			}
			return true
		},
		call: func(ctx context.Context, input journalclient.BranchOwnershipRequest) (journalclient.BranchOwnershipResponse, error) {
			return service.BranchOwnership(ctx, input)
		},
	})
}

// MaxUnpushedWorkInlineDiffBytes caps how much of a stranded diff one answer
// may carry inline. The full diff always remains addressable by the digest the
// answer names, so the cap costs discoverability nothing.
const MaxUnpushedWorkInlineDiffBytes = 1 << 20

// validCrossRunRequest applies the two validations every cross-run route
// shares: an asking run, and a gaggle to scope the answer to. A cross-run read
// with no gaggle is refused rather than widened to the whole instance — the
// unscoped walk is precisely what decision 005 R1 declined to expose.
func validCrossRunRequest(w http.ResponseWriter, runID, gaggle string) bool {
	if !apiv1.ValidRunID(strings.TrimSpace(runID)) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "runId is required and must be a valid run id")
		return false
	}
	if strings.TrimSpace(gaggle) == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "gaggle is required; cross-run journal reads are gaggle-scoped")
		return false
	}
	return true
}

// podBodyRunContained is podRunContained for a route that names its run in
// the request body rather than the path.
func podBodyRunContained(w http.ResponseWriter, request *http.Request, runID, action string) bool {
	principal, ok := PrincipalFromRequest(request)
	if !ok || !IsPodPrincipal(principal) {
		return true
	}
	if principal.Subject != podPrincipalSubject(runID) {
		writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only ask as its own run: "+action)
		return false
	}
	return true
}
