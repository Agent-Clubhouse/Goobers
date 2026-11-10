package httpapi

import (
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// runContentPrivacy decides who may read a run's private content (#8425).
// Fluid UI request prompts reach a run's artifacts, transcripts, and the
// free text agents write into progress, gate rationale, and errors. None of
// those is scrubbed by string matching: a run whose content is private is
// withheld from every viewer other than the requester and admins.
type runContentPrivacy interface {
	CanReadRunContent(runID, viewer string, workflow func() (string, error)) (bool, error)
}

type runPrivacy struct {
	service  runContentPrivacy
	reader   readservice.Reader
	errorLog *log.Logger
}

func (p runPrivacy) enabled() bool { return p.service != nil }

// allowed reports whether request may read runID's private content. workflow
// names the run's workflow when the caller already has it; otherwise it is
// read only if the run has no Fluid UI binding. Any failure denies.
func (p runPrivacy) allowed(request *http.Request, runID, workflow string) bool {
	if p.service == nil {
		return true
	}
	if principal, ok := PrincipalFromRequest(request); ok && principal.HasRole(RoleAdmin) {
		return true
	}
	lookup := func() (string, error) {
		if workflow != "" {
			return workflow, nil
		}
		run, err := p.reader.GetRun(request.Context(), runID)
		return run.Workflow, err
	}
	ok, err := p.service.CanReadRunContent(runID, labsFluidUIUser(request), lookup)
	if err != nil {
		p.errorLog.Printf("run content privacy check for %s failed closed: %v", runID, err)
		return false
	}
	return ok
}

// private marks a response whose body depends on the viewer.
func (p runPrivacy) private(w http.ResponseWriter) {
	if p.service != nil {
		w.Header().Set("Cache-Control", "private, no-store")
	}
}

func writeRunContentPrivate(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "run_content_private", "this run's content is visible only to its requester and admins")
}

func withholdRunDetail(run readservice.RunDetail) readservice.RunDetail {
	run.AgentProgress = withholdAgentSummaries(run.AgentProgress)
	run.TerminalReason = ""
	run.Escalation = withholdTerminalCause(run.Escalation)
	run.TerminalCause = withholdTerminalCause(run.TerminalCause)
	return run
}

func withholdTerminalCause(cause *readservice.EscalationCause) *readservice.EscalationCause {
	if cause == nil {
		return nil
	}
	out := *cause
	out.TerminalReason = ""
	if cause.Record != nil {
		record := *cause.Record
		record.Message = ""
		out.Record = &record
	}
	return &out
}

func withholdAgentSummaries(summaries []readservice.AgentProgressSummary) []readservice.AgentProgressSummary {
	if summaries == nil {
		return nil
	}
	out := make([]readservice.AgentProgressSummary, len(summaries))
	for i, summary := range summaries {
		if summary.Current != nil {
			current := *summary.Current
			current.Summary = ""
			summary.Current = &current
		}
		if summary.Latest != nil {
			latest := withholdProgress(*summary.Latest)
			summary.Latest = &latest
		}
		history := make([]journal.AgentProgress, len(summary.History))
		for j, progress := range summary.History {
			history[j] = withholdProgress(progress)
		}
		summary.History = history
		summary.Children = withholdAgentSummaries(summary.Children)
		out[i] = summary
	}
	return out
}

// withholdProgress keeps a progress update's identity and kind and drops
// every field an agent writes in its own words.
func withholdProgress(progress journal.AgentProgress) journal.AgentProgress {
	return journal.AgentProgress{
		Schema: progress.Schema, AgentID: progress.AgentID, RunID: progress.RunID,
		Stage: progress.Stage, Attempt: progress.Attempt, Sequence: progress.Sequence,
		Kind: progress.Kind, Source: progress.Source, OccurredAt: progress.OccurredAt,
		UpdatedAt: progress.UpdatedAt, Fidelity: progress.Fidelity,
	}
}

func withholdEvents(list readservice.EventList) readservice.EventList {
	events := make([]readservice.RunEvent, len(list.Events))
	for i, event := range list.Events {
		if event.Progress != nil {
			progress := withholdProgress(*event.Progress)
			event.Progress = &progress
		}
		if event.Agent != nil {
			agent := *event.Agent
			agent.Objective = ""
			event.Agent = &agent
		}
		event.Rationale = ""
		event.InstructionAddendum = ""
		event.Reason = ""
		event.Raw = nil
		event.Error = withholdError(event.Error)
		events[i] = event
	}
	list.Events = events
	return list
}

func withholdAttempts(list readservice.AttemptList) readservice.AttemptList {
	attempts := make([]readservice.StageAttempt, len(list.Attempts))
	for i, attempt := range list.Attempts {
		attempt.Error = withholdError(attempt.Error)
		attempts[i] = attempt
	}
	list.Attempts = attempts
	return list
}

func withholdError(detail *journal.ErrorDetail) *journal.ErrorDetail {
	if detail == nil {
		return nil
	}
	out := journal.ErrorDetail{Code: detail.Code}
	for _, cause := range detail.Causes {
		out.Causes = append(out.Causes, journal.ErrorCause{Code: cause.Code, Class: cause.Class})
	}
	return &out
}

func withholdRunList(request *http.Request, privacy runPrivacy, list readservice.RunList) readservice.RunList {
	if !privacy.enabled() {
		return list
	}
	var runs []readservice.RunSummary
	for i, run := range list.Runs {
		if run.TerminalReason == "" || privacy.allowed(request, run.ID, run.Workflow) {
			continue
		}
		if runs == nil {
			runs = append([]readservice.RunSummary(nil), list.Runs...)
		}
		runs[i].TerminalReason = ""
	}
	if runs != nil {
		list.Runs = runs
	}
	return list
}
