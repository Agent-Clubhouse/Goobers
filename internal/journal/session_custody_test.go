package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func sessionWriterEvents(id RunIdentity) []Event {
	return []Event{{Seq: 1, Type: EventStageStarted, Stage: "respond", Attempt: 1}, {Seq: 2, Type: EventRunnerAnnotation, Stage: "respond", Attempt: 1, Runner: map[string]any{"kind": SessionWriterStarted, "stageSequence": uint64(1), "inputDigest": id.Session.InputDigest}}, {Seq: 3, Type: EventRunnerAnnotation, Stage: "respond", Attempt: 1, Runner: map[string]any{"kind": SessionWriterJoined, "stageSequence": uint64(1), "inputDigest": id.Session.InputDigest}, Artifacts: []Ref{{Path: "artifacts/answer", Digest: Digest([]byte("answer")), Size: 6}}}}
}
func TestSessionWriterEvidenceRequiresExactHostPair(t *testing.T) {
	id := sessionIdentity()
	events := sessionWriterEvents(id)
	ref, joined, err := SessionWriterEvidence(events, id)
	if err != nil || !joined || ref == nil {
		t.Fatal(ref, joined, err)
	}
	ref, joined, err = SessionWriterEvidence(append(events[:2], Event{Seq: 3, Type: EventRunFinished, Status: "failed"}), id)
	if err != nil || joined || ref != nil {
		t.Fatal("terminal event claimed termination", ref, joined, err)
	}
	for _, kind := range []string{"stage", "attempt", "branch", "digest", "unmatched", "overlap", "duplicate", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			e := sessionWriterEvents(id)
			switch kind {
			case "stage":
				e[2].Stage = "other"
			case "attempt":
				e[2].Attempt = 2
			case "branch":
				e[2].Branch = 1
			case "digest":
				e[2].Runner["inputDigest"] = Digest([]byte("foreign"))
			case "unmatched":
				e = append(e[:1], e[2])
			case "overlap":
				e = append(e[:2], e[1])
				e[2].Seq = 3
			case "duplicate":
				e = append(e, e[2])
				e[3].Seq = 4
			case "metadata":
				e[2].Runner["unexpected"] = true
			}
			if _, _, err := SessionWriterEvidence(e, id); err == nil {
				t.Fatal("accepted bad writer proof")
			}
		})
	}
}
func TestEventsBoundedRefusesTruncationAndPreservesTornTail(t *testing.T) {
	run, err := Create(t.TempDir(), sessionIdentity(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Append(Event{Type: EventRunnerAnnotation, Runner: map[string]any{"kind": "test"}}); err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err := OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	expected, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(run.Dir(), fileEvents)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"torn":`); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := rd.EventsBounded(1<<20, 100)
	if err != nil || len(got) != len(expected) {
		t.Fatal(got, err)
	}
	if _, err = rd.EventsBounded(10, 100); err == nil {
		t.Fatal("bytes silently truncated")
	}
	if _, err = rd.EventsBounded(1<<20, len(expected)-1); err == nil {
		t.Fatal("records silently truncated")
	}
}
