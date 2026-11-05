package executor

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
)

type tlcChangedArtifactJournal struct {
	*tlcTestJournal
}

func (j *tlcChangedArtifactJournal) RecordArtifact(name string, _ []byte) (journal.Ref, error) {
	return j.tlcTestJournal.RecordArtifact(name, []byte("scrubbed or otherwise changed content"))
}

func TestTLCPublicationRejectsChangedBoundArtifactBytes(t *testing.T) {
	recorder := &tlcChangedArtifactJournal{&tlcTestJournal{fakeRecorder: newFakeRecorder()}}
	executor := &TLCExecutor{Shell: &ShellExecutor{Journal: recorder}}
	a := &tlcAttempt{subject: []byte("actual checked model"), manifest: TLCManifest{Property: "safe", Assumptions: []string{}}}
	_, err := executor.publishTLC(apiv1.InvocationEnvelope{TaskID: "check", Attempt: 1}, a,
		&tlcCapture{source: journal.Ref{Digest: tlcDigest(nil)}}, nil, creditgraph.VerificationProven, "")
	if err == nil {
		t.Fatal("recorded artifact content did not match the actual checked bytes")
	}
	for _, event := range recorder.events {
		if event.Type == journal.EventRunnerAnnotation {
			t.Fatal("changed bound input was promoted to verification evidence")
		}
	}
}
