package eventing

import (
	"strings"
	"testing"
)

func TestPublicationAuthorInputsCannotSupplyAuthority(t *testing.T) {
	valid := map[string]any{"kind": KindPublishEvent, "type": "build.finished", "occurrenceKey": "release", "data": `{"status":"done"}`}
	p, err := ParsePublication(valid)
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Envelope("one", "run-a", "visit-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Envelope("one", "run-a", "visit-a")
	if err != nil || a.Digest != b.Digest {
		t.Fatal("retry changed payload")
	}
	for _, field := range []string{"id", "source", "gaggle", "rootId", "actor", "runId", "stage", "branch"} {
		copy := map[string]any{}
		for k, v := range valid {
			copy[k] = v
		}
		copy[field] = "attacker"
		if _, err := ParsePublication(copy); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
	for _, change := range []func(*Publication){func(p *Publication) { p.OccurrenceKey = "second" }, func(p *Publication) { p.Type = "other" }} {
		changed := p
		change(&changed)
		next, err := changed.Envelope("one", "run-a", "visit-a")
		if err != nil || next.Digest == a.Digest {
			t.Fatal(next, err)
		}
	}
	valid["data"] = strings.Repeat("x", MaxEnvelopeBytes)
	if _, err = ParsePublication(valid); err == nil {
		t.Fatal("accepted overlarge data")
	}
}
