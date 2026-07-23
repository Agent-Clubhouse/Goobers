package rollup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture run ids are valid 32-hex-char OTel trace ids (W3C tracecontext
// examples), matching the real "run = trace" convention (internal/telemetry.NewRunID).
const (
	fixtureRunID  = "4bf92f3577b34da6a3ce929d0e0e4736"
	fixtureRunID2 = "5bf92f3577b34da6a3ce929d0e0e4737"
)

// writeFixtureRun hand-writes run.yaml + events.jsonl for a run that: starts,
// runs a build stage to completion, passes a gate, touches an external GitHub
// issue (with a runner.operation annotation), then fails a deploy stage on a
// provider rate-limit error, and finishes failed.
//
// The content is hand-written raw text — deliberately NOT constructed via this
// package's own mirror types — so a field-name mismatch between the mirror in
// mirror.go and the real internal/journal on-disk schema (pinned from PR #56
// at authoring time: event.go, identity.go, ref.go, README.md) is caught by
// these tests, not silently canceled out by encoding and decoding with the
// same (possibly wrong) shape.
func writeFixtureRun(t *testing.T, runsDir, runID string, startedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}

	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: implement
workflowVersion: 3
workflowDigest: sha256:deadbeefcafef00d
gooberDigest: sha256:resolvedgoobers
gaggle: web
trigger:
  kind: item
  ref: issue-42
startedAt: %s
`, runID, startedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}

	t0 := startedAt
	ts := func(offsetSeconds int) string {
		return t0.Add(time.Duration(offsetSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, ts(0)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"build","attempt":1,"attemptClass":"policy"}`, ts(1)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"stage.finished","stage":"build","attempt":1,"status":"success"}`, ts(3)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"gate.evaluated","gate":"review","verdict":"approve","target":"deploy"}`, ts(4)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":5,"branch":0,"time":%q,"type":"ref.touched","externalRef":{"provider":"github","kind":"issue","id":"42","url":"https://github.com/acme/app/issues/42"},"runner":{"operation":"claim"}}`, ts(5)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":6,"branch":0,"time":%q,"type":"stage.started","stage":"deploy","attempt":1,"attemptClass":"policy"}`, ts(6)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":7,"branch":0,"time":%q,"type":"error","stage":"deploy","attempt":1,"error":{"code":"provider.rate_limit","message":"github secondary rate limit hit"}}`, ts(7)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":8,"branch":0,"time":%q,"type":"stage.finished","stage":"deploy","attempt":1,"status":"failure"}`, ts(8)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":9,"branch":0,"time":%q,"type":"run.finished","status":"failed"}`, ts(9)),
	}
	events := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(events), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
	return dir
}

// writeMinimalFixtureRun writes the smallest valid run: started, one stage,
// finished — used where a test only needs a second independent run to exist
// (e.g. the rebuild-vs-incremental comparison), not the full event variety.
func writeMinimalFixtureRun(t *testing.T, runsDir, runID string, startedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	runYAML := fmt.Sprintf(`schema: goobers.dev/journal/run/v1
runId: %s
workflow: nominate
workflowVersion: 1
gaggle: web
trigger:
  kind: schedule
  ref: "*/5 * * * *"
startedAt: %s
`, runID, startedAt.UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, fileRunYAML), []byte(runYAML), 0o644); err != nil {
		t.Fatalf("write run.yaml: %v", err)
	}
	t0 := startedAt
	lines := []string{
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":1,"branch":0,"time":%q,"type":"run.started"}`, t0.UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":2,"branch":0,"time":%q,"type":"stage.started","stage":"scan","attempt":1,"attemptClass":"policy"}`, t0.Add(time.Second).UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":3,"branch":0,"time":%q,"type":"stage.finished","stage":"scan","attempt":1,"status":"success"}`, t0.Add(2*time.Second).UTC().Format(time.RFC3339Nano)),
		fmt.Sprintf(`{"schema":"goobers.dev/journal/event/v1","seq":4,"branch":0,"time":%q,"type":"run.finished","status":"completed"}`, t0.Add(3*time.Second).UTC().Format(time.RFC3339Nano)),
	}
	events := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, fileEvents), []byte(events), 0o644); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
	return dir
}
