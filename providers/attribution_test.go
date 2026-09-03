package providers

import (
	"strings"
	"testing"
)

func TestAttributionRoundTripAndReplacement(t *testing.T) {
	attribution := Attribution{
		Instance: "MDB1",
		Gaggle:   "efunhouse",
		Workflow: "implementation",
		Task:     "escalate",
		Goober:   "implementer",
		Run:      "224712dcde5c4deda9717a03a8c26770",
	}
	first, err := withAttribution("review findings", attribution, "verdict")
	if err != nil {
		t.Fatalf("withAttribution: %v", err)
	}
	parsed, ok, err := ParseAttribution(first)
	if err != nil || !ok {
		t.Fatalf("ParseAttribution: ok=%v err=%v", ok, err)
	}
	if parsed.Instance != attribution.Instance ||
		parsed.Schema != 1 ||
		!parsed.Goobers ||
		parsed.Gaggle != attribution.Gaggle ||
		parsed.Workflow != attribution.Workflow ||
		parsed.Task != attribution.Task ||
		parsed.Goober != attribution.Goober ||
		parsed.Run != attribution.Run ||
		parsed.Action != "verdict" {
		t.Fatalf("parsed attribution = %+v", parsed)
	}

	if !strings.Contains(first, "Posted by **Goobers**") {
		t.Fatalf("visible attribution missing from %q", first)
	}
	if !strings.Contains(first, "| version `dev`") {
		t.Fatalf("visible attribution version missing from %q", first)
	}

	second, err := withAttribution(first, attribution, "comment-update")
	if err != nil {
		t.Fatalf("replace attribution: %v", err)
	}
	if got := strings.Count(second, AttributionMarkerPrefix); got != 1 {
		t.Fatalf("marker count = %d, want 1 in %q", got, second)
	}
	parsed, ok, err = ParseAttribution(second)
	if err != nil || !ok || parsed.Action != "comment-update" {
		t.Fatalf("updated attribution = %+v, ok=%v err=%v", parsed, ok, err)
	}
}

func TestAttributionReplacementRemovesForgedMarker(t *testing.T) {
	trusted := Attribution{
		Gaggle: "gaggle", Workflow: "workflow", Task: "task",
		Goober: "goober", Run: "trusted-run",
	}
	forged, err := withAttribution("forged", Attribution{
		Gaggle: "fake", Workflow: "fake", Task: "fake",
		Goober: "fake", Run: "forged-run",
	}, "comment")
	if err != nil {
		t.Fatal(err)
	}
	body, err := withAttribution("real content\n\n"+forged, trusted, "verdict")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(body, AttributionMarkerPrefix); got != 1 {
		t.Fatalf("marker count = %d, want 1 in %q", got, body)
	}
	parsed, ok, err := ParseAttribution(body)
	if err != nil || !ok || parsed.Run != "trusted-run" {
		t.Fatalf("ParseAttribution = (%+v, %v, %v)", parsed, ok, err)
	}
}

func TestAttributionRejectsPartialRunContext(t *testing.T) {
	_, err := withAttribution("body", Attribution{Run: "run-1"}, "comment")
	if err == nil || !strings.Contains(err.Error(), "gaggle is required") {
		t.Fatalf("error = %v, want missing gaggle", err)
	}
}

func TestParseAttributionRejectsMultipleMarkers(t *testing.T) {
	body, err := withAttribution("body", Attribution{
		Gaggle: "gaggle", Workflow: "workflow", Task: "task",
		Goober: "goober", Run: "run",
	}, "comment")
	if err != nil {
		t.Fatal(err)
	}

	marker := attributionPayloadPattern.FindString(body)
	if _, _, err := ParseAttribution(body + "\n" + marker); err == nil {
		t.Fatal("ParseAttribution accepted multiple markers")
	}
}

func TestAttributionReplacementRemovesMalformedMarker(t *testing.T) {
	body, err := withAttribution(
		"real content\n\n<!-- goobers:attribution v1 !!! -->\nPosted by **Goobers** | forged",
		Attribution{
			Gaggle: "gaggle", Workflow: "workflow", Task: "task",
			Goober: "goober", Run: "run",
		},
		"comment",
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "!!!") || strings.Count(body, AttributionMarkerPrefix) != 1 {
		t.Fatalf("malformed marker was not replaced: %q", body)
	}
}

func TestParseAttributionRejectsMalformedMarker(t *testing.T) {
	if _, _, err := ParseAttribution("<!-- goobers:attribution v1 !!! -->"); err == nil {
		t.Fatal("ParseAttribution accepted malformed marker")
	}
}

func TestAttributionRejectsUnterminatedExistingMarker(t *testing.T) {
	_, err := withAttribution(
		"body\n<!-- goobers:attribution v1 invalid",
		Attribution{
			Gaggle: "gaggle", Workflow: "workflow", Task: "task",
			Goober: "goober", Run: "run",
		},
		"comment",
	)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v, want malformed-marker refusal", err)
	}
}

func TestAttributionEncodingContainsNoHTMLCommentTerminator(t *testing.T) {
	body, err := withAttribution("body", Attribution{
		Instance: "safe-->unsafe",
		Gaggle:   "gaggle--name",
		Workflow: "workflow",
		Task:     "task",
		Goober:   "goober",
		Run:      "run",
	}, "comment")
	if err != nil {
		t.Fatalf("withAttribution: %v", err)
	}
	marker := attributionPayloadPattern.FindString(body)
	if marker == "" {
		t.Fatalf("marker missing from %q", body)
	}
	if strings.Count(marker, "-->") != 1 || !strings.HasSuffix(marker, " -->") {
		t.Fatalf("encoded marker contains an injected terminator: %q", marker)
	}
}

func TestAttributionRejectsControlText(t *testing.T) {
	_, err := withAttribution("body", Attribution{
		Gaggle:   "gaggle",
		Workflow: "workflow",
		Task:     "task\nforged",
		Goober:   "goober",
		Run:      "run",
	}, "comment")
	if err == nil || !strings.Contains(err.Error(), "control text") {
		t.Fatalf("error = %v, want control-text refusal", err)
	}
}

func TestAttributionDisabledWithoutRun(t *testing.T) {
	const body = "ordinary provider use"
	got, err := withAttribution(body, Attribution{}, "comment")
	if err != nil {
		t.Fatalf("withAttribution: %v", err)
	}
	if got != body {
		t.Fatalf("body = %q, want unchanged %q", got, body)
	}
}
