package journal

import (
	"encoding/json"
	"errors"
)

// SessionWriterStarted binds a native writer to the committed stage occurrence.
const SessionWriterStarted = "session.writer.started"

// SessionWriterJoined acknowledges all writers stopped and response evidence stored.
const SessionWriterJoined = "session.writer.joined"

// SessionWriterEvidence reads only host annotations. A stage's terminal status
// never closes an unjoined native writer. The optional response is model text,
// not a completion or authorization assertion.
func SessionWriterEvidence(events []Event, id RunIdentity) (*Ref, bool, error) {
	if id.Session == nil || id.ValidateSessionLineage() != nil {
		return nil, false, errors.New("journal: session writer identity missing")
	}
	pending := map[uint64]Event{}
	seen := map[uint64]bool{}
	starts := map[uint64]Event{}
	var response *Ref
	for _, e := range events {
		if e.Type == EventStageStarted {
			starts[e.Seq] = e
		}
		kind, _ := e.Runner["kind"].(string)
		if e.Type != EventRunnerAnnotation || (kind != SessionWriterStarted && kind != SessionWriterJoined) {
			continue
		}
		seq, err := sessionWriterSequence(e, id)
		if err != nil {
			return nil, false, err
		}
		start, ok := starts[seq]
		if !ok || start.Stage != e.Stage || start.Attempt != e.Attempt || start.Branch != e.Branch || start.Seq >= e.Seq {
			return nil, false, errors.New("journal: session writer stage binding invalid")
		}
		if kind == SessionWriterStarted {
			if seen[seq] || len(pending) != 0 {
				return nil, false, errors.New("journal: session writer scope overlaps")
			}
			seen[seq] = true
			pending[seq] = e
			continue
		}
		if _, ok = pending[seq]; !ok || len(e.Artifacts) > 1 {
			return nil, false, errors.New("journal: session writer join has no owner")
		}
		delete(pending, seq)
		if len(e.Artifacts) == 1 {
			ref := e.Artifacts[0]
			response = &ref
		}
	}
	return response, len(pending) == 0, nil
}

func sessionWriterSequence(e Event, id RunIdentity) (uint64, error) {
	raw, err := json.Marshal(e.Runner)
	if err != nil {
		return 0, err
	}
	var data struct {
		Kind        string `json:"kind"`
		Sequence    uint64 `json:"stageSequence"`
		InputDigest string `json:"inputDigest"`
	}
	if json.Unmarshal(raw, &data) != nil || len(e.Runner) != 3 || data.Sequence == 0 || data.InputDigest != id.Session.InputDigest {
		return 0, errors.New("journal: session writer metadata invalid")
	}
	return data.Sequence, nil
}
