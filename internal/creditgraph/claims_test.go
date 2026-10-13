package creditgraph

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/journal"
)

var (
	planDigest = "sha256:" + strings.Repeat("a", 64)
	implDigest = "sha256:" + strings.Repeat("b", 64)
)

func claimBatch(seq uint64, stage string, attempt int, writerAgent string, claims ...Claim) journal.Event {
	return journal.Event{
		Seq: seq, Type: journal.EventRunnerAnnotation, Stage: stage, Attempt: attempt,
		Runner: roundTripRunner(ClaimsAnnotationPayload(claims, 0, writerAgent)),
	}
}

// roundTripRunner gives a payload the shape the journal replays: JSON-decoded
// maps and slices rather than the typed values the harness wrote.
func roundTripRunner(payload map[string]any) map[string]any {
	data, _ := json.Marshal(payload)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	return decoded
}

// claimRunEvents is plan → implement, where implement fails. The claim batch
// is appended by the caller between implement's start and finish.
func claimRunEvents(batch ...journal.Event) []journal.Event {
	events := []journal.Event{
		{Seq: 1, Type: journal.EventRunStarted},
		{Seq: 2, Type: journal.EventStageStarted, Stage: "plan", Attempt: 1},
		{Seq: 3, Type: journal.EventArtifactRecorded, Stage: "plan", Attempt: 1, Name: "plan", Ref: &journal.Ref{Digest: planDigest}},
		{Seq: 4, Type: journal.EventStageFinished, Stage: "plan", Attempt: 1, Status: "success"},
		{Seq: 5, Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
		{Seq: 6, Type: journal.EventArtifactRecorded, Stage: "implement", Attempt: 1, Name: "diff", Ref: &journal.Ref{Digest: implDigest}},
	}
	events = append(events, batch...)
	return append(events,
		journal.Event{Seq: 90, Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: "failure"},
		journal.Event{Seq: 91, Type: journal.EventRunFinished, Status: "failed"},
	)
}

func buildClaims(t *testing.T, events []journal.Event) *Graph {
	t.Helper()
	graph, err := Build(Input{RunID: "run-1", Workflow: "implementation", Events: events})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return graph
}

func TestNormalizeClaimSanitizesAndBoundsUntrustedText(t *testing.T) {
	claim, err := NormalizeClaim(Claim{
		Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}, Confidence: 0.5,
		Statement: "used\u202e the\x00 plan\u200b\n\n  file   list\xff",
	})
	if err != nil {
		t.Fatalf("NormalizeClaim: %v", err)
	}
	if claim.Statement != "used the plan file list" {
		t.Fatalf("statement = %q", claim.Statement)
	}
	for name, bad := range map[string]Claim{
		"unknown kind":     {Kind: "praise", Target: ClaimTarget{Stage: "plan"}},
		"two targets":      {Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan", Agent: "a"}},
		"no target":        {Kind: ClaimReliance},
		"attempt on agent": {Kind: ClaimReliance, Target: ClaimTarget{Agent: "a", Attempt: 2}},
		"bad digest":       {Kind: ClaimReliance, Target: ClaimTarget{Artifact: "sha256:nope"}},
		"spaced stage":     {Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan\u202e x"}},
		"oversized":        {Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}, Statement: strings.Repeat("x", MaxClaimStatementBytes+1)},
		"confidence":       {Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}, Confidence: 1.5},
		"bare assignment":  {Kind: ClaimAssignment, Target: ClaimTarget{Stage: "plan"}},
		"both evidence":    {Kind: ClaimBlame, Target: ClaimTarget{Stage: "plan"}, Evidence: []ClaimEvidence{{Artifact: planDigest, Seq: 3}}},
		"evidence digest":  {Kind: ClaimBlame, Target: ClaimTarget{Stage: "plan"}, Evidence: []ClaimEvidence{{Artifact: "md5:x"}}},
	} {
		if _, err := NormalizeClaim(bad); err == nil {
			t.Errorf("%s: NormalizeClaim accepted %+v", name, bad)
		}
	}
}

func TestAppendClaimBoundsTheAttemptAndReadDropsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	claim := Claim{Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}, Confidence: 1}
	for range MaxClaimsPerAttempt {
		if _, err := AppendClaim(dir, claim); err != nil {
			t.Fatalf("AppendClaim: %v", err)
		}
	}
	if _, err := AppendClaim(dir, claim); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("AppendClaim over the bound err = %v", err)
	}
	if _, err := AppendClaim("", claim); err == nil {
		t.Fatal("AppendClaim without a telemetry dir succeeded")
	}
	file, err := os.OpenFile(filepath.Join(dir, ClaimsFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("not json\n{\"kind\":\"blame\",\"target\":{\"stage\":\"plan\"},\"confidence\":1,\"extra\":1}\n")
	_ = file.Close()
	claims, dropped := ReadClaimFile(dir)
	if len(claims) != MaxClaimsPerAttempt || dropped != 2 {
		t.Fatalf("ReadClaimFile = %d claims, %d dropped; want %d and 2", len(claims), dropped, MaxClaimsPerAttempt)
	}
	if claims, dropped := ReadClaimFile(t.TempDir()); claims != nil || dropped != 0 {
		t.Fatalf("missing file = %v, %d", claims, dropped)
	}
}

func TestReadClaimFileRefusesASymlinkedLog(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(target, []byte(`{"kind":"reliance","target":{"stage":"plan"},"confidence":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ClaimsFileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if claims, dropped := ReadClaimFile(dir); len(claims) != 0 || dropped != 1 {
		t.Fatalf("ReadClaimFile(symlink) = %v, %d", claims, dropped)
	}
	if _, err := AppendClaim(dir, Claim{Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}}); err == nil {
		t.Fatal("AppendClaim wrote through a symlink")
	}
}

func TestClaimLineMatchesItsSchema(t *testing.T) {
	data, err := schemas.FS.ReadFile(schemas.BackpropClaim)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	uri := schemas.BaseURI + schemas.BackpropClaim
	if err := compiler.AddResource(uri, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(uri)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, claim := range []Claim{
		{Kind: ClaimAssignment, Target: ClaimTarget{Stage: "plan", Attempt: 2}, Statement: "owns the file list", Confidence: 0.4},
		{Kind: ClaimBlame, Target: ClaimTarget{Artifact: planDigest}, Evidence: []ClaimEvidence{{Artifact: implDigest}, {Seq: 4}}},
	} {
		if _, err := AppendClaim(dir, claim); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ClaimsFileName))
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var document any
		if err := json.Unmarshal([]byte(line), &document); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(document); err != nil {
			t.Errorf("line %s: %v", line, err)
		}
	}
	var invalid any
	_ = json.Unmarshal([]byte(`{"kind":"blame","target":{"stage":"plan","agent":"a"},"confidence":1}`), &invalid)
	if schema.Validate(invalid) == nil {
		t.Error("schema accepted a two-target claim")
	}
}

// TestBuildVerifiesClaimsOnlyAgainstEarlierProvenance is the verification
// matrix: a claim is verified only when its writer, target, and evidence were
// all journaled before it, and verification never adds structure.
func TestBuildVerifiesClaimsOnlyAgainstEarlierProvenance(t *testing.T) {
	baseline := buildClaims(t, claimRunEvents())
	cases := []struct {
		name   string
		claim  Claim
		reason string
	}{
		{name: "reliance on upstream artifact", claim: Claim{Kind: ClaimReliance, Target: ClaimTarget{Artifact: planDigest}, Confidence: 0.9}},
		{name: "blame upstream stage with seq evidence", claim: Claim{Kind: ClaimBlame, Target: ClaimTarget{Stage: "plan"}, Evidence: []ClaimEvidence{{Seq: 3}}, Confidence: 0.6}},
		{name: "self assignment", claim: Claim{Kind: ClaimAssignment, Target: ClaimTarget{Stage: "implement"}, Statement: "owns the diff", Confidence: 1}},
		{name: "target recorded after claim", claim: Claim{Kind: ClaimReliance, Target: ClaimTarget{Stage: "review"}}, reason: "target stage attempt recorded no stage.started"},
		{name: "self credit", claim: Claim{Kind: ClaimCredit, Target: ClaimTarget{Stage: "implement"}, Evidence: []ClaimEvidence{{Seq: 6}}}, reason: "writer's own work"},
		{name: "self artifact", claim: Claim{Kind: ClaimReliance, Target: ClaimTarget{Artifact: implDigest}}, reason: "writer's own work"},
		{name: "blame without evidence", claim: Claim{Kind: ClaimBlame, Target: ClaimTarget{Stage: "plan"}}, reason: "need at least one evidence link"},
		{name: "evidence from the future", claim: Claim{Kind: ClaimBlame, Target: ClaimTarget{Stage: "plan"}, Evidence: []ClaimEvidence{{Seq: 90}}}, reason: "evidence seq 90"},
		{name: "unrecorded evidence artifact", claim: Claim{Kind: ClaimCredit, Target: ClaimTarget{Stage: "plan"}, Evidence: []ClaimEvidence{{Artifact: "sha256:" + strings.Repeat("c", 64)}}}, reason: "was not recorded before the claim"},
		{name: "unknown agent", claim: Claim{Kind: ClaimReliance, Target: ClaimTarget{Agent: "ghost"}}, reason: "target agent recorded no lifecycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := buildClaims(t, claimRunEvents(claimBatch(10, "implement", 1, "", tc.claim)))
			if len(graph.Nodes) != len(baseline.Nodes) || len(graph.Edges) != len(baseline.Edges) {
				t.Fatalf("claims changed structure: %d/%d nodes, %d/%d edges",
					len(graph.Nodes), len(baseline.Nodes), len(graph.Edges), len(baseline.Edges))
			}
			if tc.reason == "" {
				if len(graph.Claims) != 1 || len(graph.UnverifiedClaims) != 0 {
					t.Fatalf("want verified, got claims=%+v unverified=%+v", graph.Claims, graph.UnverifiedClaims)
				}
				record := graph.Claims[0]
				if record.Status != ClaimVerified || record.WriterNodeID != "stage:implement#1" || record.TargetNodeID == "" || record.ID != "claim:10:0" {
					t.Fatalf("verified record = %+v", record)
				}
				return
			}
			if len(graph.Claims) != 0 || len(graph.UnverifiedClaims) != 1 {
				t.Fatalf("want unverified, got claims=%+v unverified=%+v", graph.Claims, graph.UnverifiedClaims)
			}
			record := graph.UnverifiedClaims[0]
			if record.Status != ClaimUnverified || record.TargetNodeID != "" || record.WriterNodeID != "" ||
				!strings.Contains(strings.Join(record.Reasons, "; "), tc.reason) {
				t.Fatalf("unverified record = %+v, want reason %q", record, tc.reason)
			}
		})
	}
}

func TestBuildRejectsClaimsFromAnUnrecordedWriter(t *testing.T) {
	claim := Claim{Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}}
	graph := buildClaims(t, claimRunEvents(
		claimBatch(10, "ghost", 1, "", claim),
		claimBatch(11, "implement", 1, "ghost-agent", claim),
	))
	if len(graph.Claims) != 0 || len(graph.UnverifiedClaims) != 2 {
		t.Fatalf("claims=%+v unverified=%+v", graph.Claims, graph.UnverifiedClaims)
	}
	if !strings.Contains(graph.UnverifiedClaims[0].Reasons[0], "writing stage attempt") ||
		!strings.Contains(graph.UnverifiedClaims[1].Reasons[0], "writer agent") {
		t.Fatalf("reasons = %v / %v", graph.UnverifiedClaims[0].Reasons, graph.UnverifiedClaims[1].Reasons)
	}
}

func TestBuildBoundsClaimsPerRun(t *testing.T) {
	claim := Claim{Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}}
	var batches []journal.Event
	claims := make([]Claim, MaxClaimsPerAttempt)
	for index := range claims {
		claims[index] = claim
	}
	for seq := uint64(10); len(batches)*MaxClaimsPerAttempt <= MaxClaimsPerRun; seq++ {
		batches = append(batches, claimBatch(seq, "implement", 1, "", claims...))
	}
	batches[0].Runner[ClaimsKeyDropped] = float64(3)
	graph := buildClaims(t, claimRunEvents(batches...))
	if len(graph.Claims) != MaxClaimsPerRun {
		t.Fatalf("verified = %d, want %d", len(graph.Claims), MaxClaimsPerRun)
	}
	if want := len(batches)*MaxClaimsPerAttempt - MaxClaimsPerRun + 3; graph.DroppedClaims != want {
		t.Fatalf("dropped = %d, want %d", graph.DroppedClaims, want)
	}
}

func TestBuildRevalidatesJournaledClaims(t *testing.T) {
	batch := claimBatch(10, "implement", 1, "", Claim{Kind: ClaimReliance, Target: ClaimTarget{Stage: "plan"}})
	batch.Runner[ClaimsKeyClaims] = append(batch.Runner[ClaimsKeyClaims].([]any),
		map[string]any{"kind": "reliance", "target": map[string]any{"stage": "plan"}, "statement": "x\u202ey", "confidence": 1},
		map[string]any{"kind": "credit", "target": map[string]any{"stage": "plan", "agent": "a"}, "confidence": 1})
	graph := buildClaims(t, claimRunEvents(batch))
	if len(graph.Claims) != 2 || graph.DroppedClaims != 1 || graph.Claims[1].Statement != "xy" {
		t.Fatalf("claims=%+v dropped=%d", graph.Claims, graph.DroppedClaims)
	}
}

func TestAttributeCitesVerifiedBlameClaimsWithoutChangingScores(t *testing.T) {
	reviewStart := journal.Event{Seq: 7, Type: journal.EventStageStarted, Stage: "review", Attempt: 1}
	base := Attribute(buildClaims(t, claimRunEvents(reviewStart)))
	verified := Claim{Kind: ClaimBlame, Target: ClaimTarget{Stage: "implement", Attempt: 1}, Evidence: []ClaimEvidence{{Seq: 6}}, Confidence: 0.7}
	unverified := Claim{Kind: ClaimBlame, Target: ClaimTarget{Stage: "implement"}, Confidence: 0.9}
	attribution := Attribute(buildClaims(t, claimRunEvents(reviewStart, claimBatch(10, "review", 1, "", verified, unverified))))
	if len(attribution.Claims) != 1 || len(attribution.UnverifiedClaims) != 1 {
		t.Fatalf("claims=%+v unverified=%+v", attribution.Claims, attribution.UnverifiedClaims)
	}
	cited := 0
	for _, cause := range attribution.Causes {
		for _, evidence := range cause.Evidence {
			if strings.HasPrefix(evidence, ClaimEvidencePrefix) {
				cited++
				if !strings.Contains(evidence, "claim:10:0") || strings.Contains(evidence, "claim:10:1") {
					t.Fatalf("cited evidence = %q", evidence)
				}
			}
		}
	}
	if cited == 0 {
		t.Fatalf("no cause cites the verified blame claim: %+v", attribution.Causes)
	}
	baseContribution := contributionOf(t, base, "stage:implement#1")
	contribution := contributionOf(t, attribution, "stage:implement#1")
	if baseContribution.Share != contribution.Share || baseContribution.Confidence != contribution.Confidence {
		t.Fatalf("claims changed credit: %+v vs %+v", contribution, baseContribution)
	}
}
