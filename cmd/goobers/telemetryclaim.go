package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/creditgraph"
)

const telemetryClaimHelp = "Usage: goobers telemetry claim --kind=assignment|reliance|credit|blame (--stage=NAME [--attempt=N] | --agent=ID | --artifact=sha256:HEX) [--statement=TEXT] [--evidence=sha256:HEX|seq:N]... [--confidence=0..1]\n\n" +
	"Append one Backprop claim to the current stage attempt's claim log\n" +
	"($GOOBERS_TELEMETRY_DIR/backprop-claims.jsonl). Run it from inside an\n" +
	"agentic stage: the runner journals the attempt's claims when the stage\n" +
	"exits and attributes them to the writing stage attempt, never to text in\n" +
	"the claim. assignment says the target owns the work the statement\n" +
	"describes; reliance says this stage consumed the target's output; credit\n" +
	"and blame say the target helped or hurt the outcome and need --evidence.\n" +
	"Evidence names a recorded artifact digest or an earlier journal seq.\n" +
	"Statements are sanitized and capped at 1024 bytes; an attempt may record\n" +
	"at most 32 claims. Backprop lists a claim as verified only when its writer,\n" +
	"target, and evidence match records journaled before it; claims never\n" +
	"change credit scores or stage outcomes.\n" +
	"Exit codes: 0 = recorded, 1 = write error, 2 = usage error.\n"

func runTelemetryClaim(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("telemetry claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("kind", "", "claim kind: assignment, reliance, credit, or blame (required)")
	stage := fs.String("stage", "", "target stage name")
	attempt := fs.Int("attempt", 0, "target stage attempt (default latest recorded)")
	agent := fs.String("agent", "", "target nested agent ID")
	artifact := fs.String("artifact", "", "target recorded artifact digest (sha256:<hex>)")
	statement := fs.String("statement", "", "what the claim asserts (required for assignment)")
	confidence := fs.Float64("confidence", 1, "the writer's confidence between 0 and 1")
	var evidence stringListFlag
	fs.Var(&evidence, "evidence", "supporting artifact digest (sha256:<hex>) or journal seq (seq:N); repeatable")
	fs.Usage = helpUsage(stderr, "telemetry claim")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*kind) == "" {
		fs.Usage()
		return 2
	}
	claim := creditgraph.Claim{
		Kind:       creditgraph.ClaimKind(*kind),
		Target:     creditgraph.ClaimTarget{Stage: *stage, Attempt: *attempt, Agent: *agent, Artifact: *artifact},
		Statement:  *statement,
		Confidence: *confidence,
	}
	for _, value := range evidence {
		link, ok := parseClaimEvidence(value)
		if !ok {
			pf(stderr, "error: --evidence %q must be sha256:<hex> or seq:<n>\n", value)
			return 2
		}
		claim.Evidence = append(claim.Evidence, link)
	}
	if (claim.Kind == creditgraph.ClaimCredit || claim.Kind == creditgraph.ClaimBlame) && len(claim.Evidence) == 0 {
		pf(stderr, "error: credit and blame claims need at least one --evidence link\n")
		return 2
	}
	if _, err := creditgraph.NormalizeClaim(claim); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	recorded, err := creditgraph.AppendClaim(os.Getenv("GOOBERS_TELEMETRY_DIR"), claim)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	line, _ := json.Marshal(recorded)
	pf(stdout, "%s\n", line)
	return 0
}

func parseClaimEvidence(value string) (creditgraph.ClaimEvidence, bool) {
	if raw, ok := strings.CutPrefix(value, "seq:"); ok {
		seq, err := strconv.ParseUint(raw, 10, 64)
		return creditgraph.ClaimEvidence{Seq: seq}, err == nil && seq > 0
	}
	if strings.HasPrefix(value, "sha256:") {
		return creditgraph.ClaimEvidence{Artifact: value}, true
	}
	return creditgraph.ClaimEvidence{}, false
}
