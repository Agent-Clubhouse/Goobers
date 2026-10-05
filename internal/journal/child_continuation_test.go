package journal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/validate"
)

func TestChildContinuationRequiresExactAdmittedEpochAndPreservesSource(t *testing.T) {
	root := t.TempDir()
	id := childTestIdentity()
	source, err := Create(root, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	rd, err := OpenReadOnly(filepath.Join(root, id.RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotJournalTree(t, filepath.Join(root, id.RunID))
	lineage := *id.Child
	lineage.ExecutionEpoch = 1
	lineage.PriorResultRef = Digest([]byte("sealed original result"))
	lineage.RestartDigest = Digest([]byte("admitted human restart"))
	request := ContinuationRequest{RunID: "caf7651916cd43dd8448eb211c80319c", SourceRunID: id.RunID, ExpectedTerminalSeq: events[len(events)-1].Seq, Operator: "issuer:human", Target: "implement", ChildContinuation: &lineage}
	for _, mode := range []string{"unadmitted", "source", "epoch", "missing result"} {
		t.Run(mode, func(t *testing.T) {
			bad := request
			changed := lineage
			bad.ChildContinuation = &changed
			switch mode {
			case "unadmitted":
				bad.ChildContinuation = nil
			case "source":
				changed.SourceDigest = Digest([]byte("other"))
			case "epoch":
				changed.ExecutionEpoch = 2
			case "missing result":
				changed.PriorResultRef = ""
			}
			if run, err := CreateContinuation(root, bad); err == nil {
				_ = run.Close()
				t.Fatal("invalid continuation published")
			}
			if _, err := os.Stat(filepath.Join(root, request.RunID)); !os.IsNotExist(err) {
				t.Fatal("invalid continuation left custody", err)
			}
		})
	}
	next, err := CreateContinuation(root, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
	after := snapshotJournalTree(t, filepath.Join(root, id.RunID))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("source journal mutated")
	}
	verifyChildEpochSchema(t, root, request, lineage)
}
func verifyChildEpochSchema(t *testing.T, root string, request ContinuationRequest, lineage ChildLineage) {
	t.Helper()
	rd, err := OpenReadOnly(filepath.Join(root, request.RunID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rd.Identity()
	if err != nil || got.Child == nil || *got.Child != lineage || got.ContinuedFromRunID != request.SourceRunID {
		t.Fatal(got, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, request.RunID, fileRunYAML))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yaml.YAMLToJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	v, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err = v.ValidateJSON("journal-run.schema.json", doc); err != nil {
		t.Fatal(err)
	}
}
