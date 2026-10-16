package recovery

import (
	"net/http"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestTerminalSourceRunReadsLatestTerminalGeneration(t *testing.T) {
	identity := journal.RunIdentity{RunID: "source-run", Workflow: "implementation"}
	resumedAndEscalated := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventRunFinished, Status: string(journal.PhaseFailed)},
		{Seq: 3, Type: journal.EventRunResumed},
		{Seq: 4, Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
	}
	got, err := TerminalSourceRun(identity, resumedAndEscalated)
	if err != nil {
		t.Fatal(err)
	}
	if want := (SourceRun{Workflow: "implementation", Phase: "escalated", TerminalSeq: 4}); got != want {
		t.Fatalf("source run = %+v, want %+v", got, want)
	}
	if _, err := TerminalSourceRun(identity, resumedAndEscalated[:3]); err == nil {
		t.Fatal("resumed source run reported terminal evidence")
	}
}

func TestSourceRunHeadersRoundTripAndRejectPartialEvidence(t *testing.T) {
	source := SourceRun{Workflow: "implementation", Phase: "escalated", TerminalSeq: 12}
	header := http.Header{}
	source.SetHeaders(header)
	if got, err := SourceRunFromHeaders(header); err != nil || got != source {
		t.Fatalf("round trip = %+v %v", got, err)
	}
	if got, err := SourceRunFromHeaders(http.Header{}); err != nil || got.Known() {
		t.Fatalf("absent evidence = %+v %v, want unknown", got, err)
	}
	for name, mutate := range map[string]func(http.Header){
		"missing phase":    func(h http.Header) { h.Del(SourcePhaseHeader) },
		"missing workflow": func(h http.Header) { h.Del(SourceWorkflowHeader) },
		"running phase":    func(h http.Header) { h.Set(SourcePhaseHeader, string(journal.PhaseRunning)) },
		"zero sequence":    func(h http.Header) { h.Set(SourceTerminalSeqHeader, "0") },
		"bad sequence":     func(h http.Header) { h.Set(SourceTerminalSeqHeader, "-1") },
		"only sequence": func(h http.Header) {
			h.Del(SourceWorkflowHeader)
			h.Del(SourcePhaseHeader)
		},
	} {
		t.Run(name, func(t *testing.T) {
			malformed := header.Clone()
			mutate(malformed)
			if got, err := SourceRunFromHeaders(malformed); err == nil {
				t.Fatalf("partial evidence accepted as %+v", got)
			}
		})
	}
}
