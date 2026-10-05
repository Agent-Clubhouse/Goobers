package providers

import (
	"strings"
	"testing"
)

func TestAttributionRoundTripAndReplacement(t *testing.T) {
	nanoAIU := int64(12_500_000_000)
	premium := 1.5
	attribution := Attribution{
		Instance:   "MDB1",
		InstanceID: "e62c1c105fdc4273a72d199394b41cb0",
		Gaggle:     "dogfood",
		Workflow:   "implementation",
		Task:       "escalate",
		Goober:     "implementer",
		Run:        "224712dcde5c4deda9717a03a8c26770",
		Cost: &CostReceipt{
			JournalSequence:        42,
			CopilotPremiumRequests: &premium,
			NanoAIU:                &nanoAIU,
		},
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
		parsed.InstanceID != attribution.InstanceID ||
		parsed.Schema != 1 ||
		!parsed.Goobers ||
		parsed.Gaggle != attribution.Gaggle ||
		parsed.Workflow != attribution.Workflow ||
		parsed.Task != attribution.Task ||
		parsed.Goober != attribution.Goober ||
		parsed.Run != attribution.Run ||
		parsed.Action != "verdict" ||
		parsed.Cost == nil ||
		parsed.Cost.JournalSequence != 42 ||
		parsed.Cost.NanoAIU == nil || *parsed.Cost.NanoAIU != nanoAIU ||
		parsed.Cost.CopilotPremiumRequests == nil || *parsed.Cost.CopilotPremiumRequests != premium {
		t.Fatalf("parsed attribution = %+v", parsed)
	}

	if !strings.Contains(first, "Posted by **Goobers**") {
		t.Fatalf("visible attribution missing from %q", first)
	}
	if !strings.Contains(first, "| version `dev`") {
		t.Fatalf("visible attribution version missing from %q", first)
	}
	if !strings.Contains(first, "| Cost: 13 AIC") {
		t.Fatalf("visible attribution cost missing from %q", first)
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

func TestStripAttributionRecoversTheWrittenBody(t *testing.T) {
	nanoAIU := int64(2_500_000_000)
	attribution := Attribution{
		Instance: "example-instance", Gaggle: "example-gaggle", Workflow: "decomposition",
		Task: "publish-batch", Goober: "deterministic", Run: "run-strip-1",
		Cost: &CostReceipt{JournalSequence: 1, NanoAIU: &nanoAIU},
	}
	written := "Child body.\n\n<!-- goobers-action:v1 key=YXBp -->\n<!-- goobers-action-digest:v1 sha256:00 -->\n"
	stamped, err := StampAttribution(written, attribution, "issue-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := ParseAttribution(stamped); err != nil || !found {
		t.Fatalf("StampAttribution body has no attribution: found=%v err=%v body=%q", found, err, stamped)
	}
	if got, want := StripAttribution(stamped), strings.TrimSpace(written); got != want {
		t.Fatalf("StripAttribution(stamped) = %q, want %q", got, want)
	}
	if got := StripAttribution("  " + written); got != strings.TrimSpace(written) {
		t.Fatalf("StripAttribution(unattributed) = %q, want trimmed input", got)
	}
	restamped, err := StampAttribution(stamped, attribution, "issue-create")
	if err != nil {
		t.Fatal(err)
	}
	if restamped != stamped {
		t.Fatalf("re-stamp changed body:\n%q\nwant\n%q", restamped, stamped)
	}
	unchanged, err := StampAttribution(written, Attribution{}, "issue-create")
	if err != nil || unchanged != written {
		t.Fatalf("StampAttribution with zero attribution = %q, %v; want input unchanged", unchanged, err)
	}
}

// TestStripAttributionRemovesTheOperationMarker: a keyed work-item update
// stores its comment as text + operation marker + attribution footer (#2657).
// Both are the provider's stamp, so a reader comparing against the intended
// text must not see either; caller-authored markers stay.
func TestStripAttributionRemovesTheOperationMarker(t *testing.T) {
	written := "Implementation complete: https://example.test/pr/1 is open for merge-review.\n<!-- goobers-action:v1 key=YXBp -->"
	keyed := written + "\n\n" + OperationCommentMarker("issue-close-out/run-1/7/in-review")
	if got := StripAttribution(keyed); got != strings.TrimSpace(written) {
		t.Fatalf("StripAttribution(unattributed) = %q, want %q", got, strings.TrimSpace(written))
	}
	stamped, err := StampAttribution(keyed, Attribution{
		Gaggle: "example-gaggle", Workflow: "implementation", Task: "issue-close-out", Goober: "deterministic", Run: "run-1",
	}, "state-change")
	if err != nil {
		t.Fatal(err)
	}
	if got := StripAttribution(stamped); got != strings.TrimSpace(written) {
		t.Fatalf("StripAttribution(stamped) = %q, want %q", got, strings.TrimSpace(written))
	}
}

func TestStripAttributionKeepsTextAddedAfterTheFooterOnItsOwnLine(t *testing.T) {
	stamped, err := StampAttribution("Body.\n<!-- goobers-action-digest:v1 sha256:00 -->", Attribution{
		Gaggle: "example-gaggle", Workflow: "decomposition", Task: "publish-batch", Goober: "deterministic", Run: "run-strip-2",
	}, "issue-create")
	if err != nil {
		t.Fatal(err)
	}
	got := StripAttribution(stamped + "\nEdited later.")
	want := "Body.\n<!-- goobers-action-digest:v1 sha256:00 -->\nEdited later."
	if got != want {
		t.Fatalf("StripAttribution = %q, want %q", got, want)
	}
}
