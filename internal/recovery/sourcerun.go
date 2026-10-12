package recovery

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/goobers/goobers/internal/journal"
)

// Response headers carrying the source run's terminal journal evidence beside
// a claims-plane archive download. A daemon that predates them sends none, and
// the receiver then reports no source-run evidence rather than inventing it.
const (
	SourceWorkflowHeader    = "X-Goobers-Recovery-Source-Workflow"
	SourcePhaseHeader       = "X-Goobers-Recovery-Source-Phase"
	SourceTerminalSeqHeader = "X-Goobers-Recovery-Source-Terminal-Seq"
)

var sourceWorkflowName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// SourceRun is the terminal journal evidence for the run whose retained
// implementation a recovery stage adopted or skipped, read from that run's own
// journal while it was idle. TerminalSeq is the sequence of the run's latest
// run.finished event, the generation `goobers run continue --terminal-seq`
// binds to; it is omitted, never guessed, when the journal has none.
type SourceRun struct {
	// Workflow is the workflow definition the source run executed.
	Workflow string `json:"sourceWorkflow,omitempty"`
	// Phase is the source run's terminal phase (completed, failed, aborted, or
	// escalated).
	Phase string `json:"sourceRunPhase,omitempty"`
	// TerminalSeq is the sequence of the source run's latest run.finished event.
	TerminalSeq uint64 `json:"sourceTerminalSeq,omitempty"`
}

// ReadTerminalSourceRun reads an idle source run's terminal evidence in one
// journal pass. The caller must hold the run idle and have verified identity.
func ReadTerminalSourceRun(reader *journal.Reader, identity journal.RunIdentity) (SourceRun, error) {
	events, err := reader.Events()
	if err != nil {
		return SourceRun{}, err
	}
	return TerminalSourceRun(identity, events)
}

// TerminalSourceRun reconstructs a source run's terminal evidence from one read
// of its events, so phase and sequence describe the same journal generation. It
// refuses a source run that is not terminal.
func TerminalSourceRun(identity journal.RunIdentity, events []journal.Event) (SourceRun, error) {
	phase := journal.PhaseFromEvents(events)
	if !terminalSourcePhase(phase) {
		return SourceRun{}, fmt.Errorf("selected recovery run is no longer terminal")
	}
	source := SourceRun{Workflow: identity.Workflow, Phase: string(phase)}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventRunFinished {
			source.TerminalSeq = events[i].Seq
			break
		}
	}
	if err := source.validate(); err != nil {
		return SourceRun{}, err
	}
	return source, nil
}

// Known reports whether any source-run evidence is present.
func (s SourceRun) Known() bool { return s != SourceRun{} }

// SourceRunReporter is a delivery stream that can carry SourceRun evidence
// beside the archive. Delivery services report through ReportingSourceRun;
// streams without a side channel do not implement it.
type SourceRunReporter interface {
	ReportSourceRun(SourceRun)
}

// ReportingSourceRun returns out wrapped so source is reported immediately
// before the first archive byte, and never for a delivery refused before its
// archive verified. A stream that is not a SourceRunReporter is returned as is.
func ReportingSourceRun(out io.Writer, source SourceRun) io.Writer {
	reporter, ok := out.(SourceRunReporter)
	if !ok {
		return out
	}
	return &sourceRunReportingWriter{out: out, reporter: reporter, source: source}
}

type sourceRunReportingWriter struct {
	out      io.Writer
	reporter SourceRunReporter
	source   SourceRun
	reported bool
}

func (w *sourceRunReportingWriter) Write(data []byte) (int, error) {
	if !w.reported && len(data) != 0 {
		w.reporter.ReportSourceRun(w.source)
		w.reported = true
	}
	return w.out.Write(data)
}

// SetHeaders publishes s on a download response before any archive byte is
// written, replacing any earlier evidence. Unknown evidence sets no headers.
func (s SourceRun) SetHeaders(header http.Header) {
	ClearSourceRunHeaders(header)
	if !s.Known() {
		return
	}
	header.Set(SourceWorkflowHeader, s.Workflow)
	header.Set(SourcePhaseHeader, s.Phase)
	if s.TerminalSeq != 0 {
		header.Set(SourceTerminalSeqHeader, strconv.FormatUint(s.TerminalSeq, 10))
	}
}

// ClearSourceRunHeaders removes source-run evidence from a response that will
// not deliver an archive.
func ClearSourceRunHeaders(header http.Header) {
	header.Del(SourceWorkflowHeader)
	header.Del(SourcePhaseHeader)
	header.Del(SourceTerminalSeqHeader)
}

// SourceRunFromHeaders reads evidence set by SetHeaders. No headers is the
// unknown zero value; any malformed or partial evidence is an error, so a
// download never reports evidence it did not receive intact.
func SourceRunFromHeaders(header http.Header) (SourceRun, error) {
	workflow, phase, seq := header.Get(SourceWorkflowHeader), header.Get(SourcePhaseHeader), header.Get(SourceTerminalSeqHeader)
	if workflow == "" && phase == "" && seq == "" {
		return SourceRun{}, nil
	}
	source := SourceRun{Workflow: workflow, Phase: phase}
	if seq != "" {
		parsed, err := strconv.ParseUint(seq, 10, 64)
		if err != nil || parsed == 0 {
			return SourceRun{}, fmt.Errorf("invalid recovery source run evidence")
		}
		source.TerminalSeq = parsed
	}
	if err := source.validate(); err != nil {
		return SourceRun{}, err
	}
	return source, nil
}

func (s SourceRun) validate() error {
	if !sourceWorkflowName.MatchString(s.Workflow) || !terminalSourcePhase(journal.RunPhase(s.Phase)) {
		return fmt.Errorf("invalid recovery source run evidence")
	}
	return nil
}

func terminalSourcePhase(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}
