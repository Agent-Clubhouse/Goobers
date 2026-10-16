package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

// receiptWritingAdapter stands in for goobers-io: it appends one publication
// receipt during the session, then runs the publication repair adapter.
type receiptWritingAdapter struct {
	publicationRepairingAdapter
	receipt mcpio.PublicationReceipt
}

func (a *receiptWritingAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if err := appendPublicationReceipt(req.Workspace, a.receipt); err != nil {
		return Outcome{}, err
	}
	return a.publicationRepairingAdapter.Run(ctx, req)
}

func appendPublicationReceipt(workspace string, receipt mcpio.PublicationReceipt) error {
	full := filepath.Join(workspace, goobersIOPublicationReceiptFile())
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}

func publicationDiagnostics(t *testing.T, rec *fakeRecorder) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, event := range rec.events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != PublicationDiagnosticsKind {
			continue
		}
		// Round-trip through JSON as the journal does.
		data, err := json.Marshal(event.Runner)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		out = append(out, decoded)
	}
	return out
}

func TestExecutorJournalsPublicationRepairDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, first, repair string
		// emptyManifest stages a manifest that omits the schema-bound entry.
		emptyManifest bool
		wantCode      string
		wantOutcomes  []string
	}{
		{name: "invalid then corrected", first: `{"summary":["hunter2-secret-value"]}`, repair: `{"summary":"fixed"}`, wantOutcomes: []string{"rejected", "accepted"}},
		{name: "omitted then published", repair: `{"summary":"fixed"}`, wantOutcomes: []string{"rejected", "accepted"}},
		{name: "slot omitted from manifest then published", emptyManifest: true, repair: `{"summary":"fixed"}`, wantCode: artifactset.MissingSlotCode, wantOutcomes: []string{"rejected", "accepted"}},
		{name: "repair exhausted", first: `{"summary":3}`, repair: `{"summary":4}`, wantOutcomes: []string{"rejected", "rejected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := appendPublicationReceipt(workspace, mcpio.PublicationReceipt{Outcome: "stale-from-prior-attempt"}); err != nil {
				t.Fatal(err)
			}
			if tc.emptyManifest {
				if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), []byte(`{"schemaVersion":"`+artifactset.SchemaVersion+`","entries":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			wantCode := tc.wantCode
			if wantCode == "" {
				wantCode = "invalid_declared_artifact_set"
			}
			rec := &fakeRecorder{}
			adapter := &receiptWritingAdapter{
				publicationRepairingAdapter: publicationRepairingAdapter{status: apiv1.ResultSuccess, first: tc.first, repair: tc.repair},
				receipt:                     mcpio.PublicationReceipt{Outcome: mcpio.PublicationAccepted, ManifestDigest: "sha256:abc"},
			}
			if _, err := newPostconditionExecutor(t, adapter, rec).Invoke(reportSchemaContext(t), reportPublicationEnvelope(workspace)); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			diagnostics := publicationDiagnostics(t, rec)
			if len(diagnostics) != 1 {
				t.Fatalf("publication diagnostics = %+v, want exactly one per attempt", diagnostics)
			}
			got := diagnostics[0]
			if got["attempt"] != float64(1) || got["publishReceipts"] != float64(1) {
				t.Fatalf("diagnostics=%+v, want attempt 1 and only this attempt's receipt", got)
			}
			receipts, _ := got["receipts"].([]any)
			if len(receipts) != 1 || receipts[0].(map[string]any)["manifestDigest"] != "sha256:abc" {
				t.Fatalf("receipts=%+v", got["receipts"])
			}
			checks, _ := got["completionChecks"].([]any)
			if len(checks) != len(tc.wantOutcomes) {
				t.Fatalf("completionChecks=%+v, want %v", checks, tc.wantOutcomes)
			}
			for i, want := range tc.wantOutcomes {
				check := checks[i].(map[string]any)
				if check["outcome"] != want {
					t.Fatalf("check %d = %+v, want %s", i, check, want)
				}
				if want == "rejected" && check["code"] != wantCode {
					t.Fatalf("rejected check %d = %+v, want the lift failure code", i, check)
				}
			}
			rejected := checks[0].(map[string]any)
			// No publish_output receipt names the omitted slot, so the check
			// itself must carry the declared slot's schema identity.
			if tc.emptyManifest && (rejected["slot"] != "report" || rejected["schemaId"] != "schemas/report.schema.json") {
				t.Fatalf("omitted-output check = %+v, want the declared slot and its schema identity", rejected)
			}
			if tc.first != "" && (rejected["slot"] != "report" || rejected["schemaId"] != "schemas/report.schema.json" || !strings.Contains(mustJSON(t, rejected["issues"]), `"/summary"`)) {
				t.Fatalf("rejected check = %+v, want the schema identity and validation location", rejected)
			}
			if raw := mustJSON(t, got); strings.Contains(raw, "hunter2-secret-value") {
				t.Fatalf("publication diagnostics echoed a payload value: %s", raw)
			}
		})
	}
}

func TestExecutorJournalsNoPublicationDiagnosticsWithoutSchemas(t *testing.T) {
	workspace := t.TempDir()
	rec := &fakeRecorder{}
	adapter := &publicationRepairingAdapter{status: apiv1.ResultSuccess, first: `{"summary":3}`}
	if _, err := newPostconditionExecutor(t, adapter, rec).Invoke(t.Context(), reportPublicationEnvelope(workspace)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if diagnostics := publicationDiagnostics(t, rec); len(diagnostics) != 0 {
		t.Fatalf("a stage without schema-bound slots journaled %+v", diagnostics)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExecutorJournalsTheCompletionValidatedPayloadDigest(t *testing.T) {
	workspace := t.TempDir()
	published, final := `{"summary":"as published"}`, `{"summary":"rewritten after publish"}`
	rec := &fakeRecorder{}
	// The tool-time receipt names the published bytes; the agent then rewrites
	// the payload with different schema-valid bytes before completing.
	adapter := &receiptWritingAdapter{
		publicationRepairingAdapter: publicationRepairingAdapter{status: apiv1.ResultSuccess, first: final},
		receipt: mcpio.PublicationReceipt{Outcome: mcpio.PublicationAccepted, ManifestDigest: "sha256:abc", Outputs: []mcpio.PublicationReceiptOutput{
			{Slot: "report", SchemaID: "schemas/report.schema.json", PayloadDigest: apiv1.Digest([]byte(published))},
		}},
	}
	if _, err := newPostconditionExecutor(t, adapter, rec).Invoke(reportSchemaContext(t), reportPublicationEnvelope(workspace)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	diagnostics := publicationDiagnostics(t, rec)
	if len(diagnostics) != 1 {
		t.Fatalf("publication diagnostics = %+v", diagnostics)
	}
	receipts, _ := diagnostics[0]["receipts"].([]any)
	checks, _ := diagnostics[0]["completionChecks"].([]any)
	if len(receipts) != 1 || len(checks) != 1 {
		t.Fatalf("receipts=%+v checks=%+v", receipts, checks)
	}
	toolDigest := receipts[0].(map[string]any)["outputs"].([]any)[0].(map[string]any)["payloadDigest"]
	check := checks[0].(map[string]any)
	digests, _ := check["payloadDigests"].(map[string]any)
	if check["outcome"] != mcpio.PublicationAccepted || digests["report"] != apiv1.Digest([]byte(final)) {
		t.Fatalf("completion check = %+v, want accepted with the final payload digest", check)
	}
	if digests["report"] == toolDigest {
		t.Fatalf("completion digest %v matches the tool-time digest; the rewrite was not observed", toolDigest)
	}
}

func TestPublicationPostconditionRecordsANonDeclaredFailureAsErrored(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cancel   bool
		wantCode string
	}{
		{name: "context cancelled", cancel: true, wantCode: publicationCheckCancelled},
		// The workspace vanished, so the artifact set cannot be read at all.
		{name: "artifact read error", wantCode: publicationCheckFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := stageReport(workspace, `{"summary":"valid"}`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(reportSchemaContext(t))
			defer cancel()
			req := RunRequest{Workspace: workspace}
			p := newPostconditionExecutor(t, &FakeAdapter{}, &fakeRecorder{}).armPublicationPostcondition(ctx, ModeInvoke, reportPublicationEnvelope(workspace), &req)
			if p == nil {
				t.Fatal("publication postcondition not armed")
			}
			if tc.cancel {
				cancel()
			} else if err := os.RemoveAll(workspace); err != nil {
				t.Fatal(err)
			}
			// A non-declared failure does not spend the repair turn — but it
			// must not be journaled as accepted either.
			if err := req.ValidateCompletion([]byte(`{"status":"success"}`)); err != nil {
				t.Fatalf("ValidateCompletion = %v, want no repairable postcondition", err)
			}
			if len(p.checks) != 1 || p.checks[0].Outcome != publicationCheckErrored || p.checks[0].Code != tc.wantCode || p.checks[0].PayloadDigests != nil {
				t.Fatalf("checks=%+v, want one errored check coded %s", p.checks, tc.wantCode)
			}
		})
	}
}

func TestGoobersIORuntimeRequestsPublicationReceiptsOnlyForSchemaBoundSlots(t *testing.T) {
	for _, tc := range []struct {
		name    string
		schemas []mcpio.PublicationSchema
		want    string
	}{
		{name: "schema-bound", schemas: []mcpio.PublicationSchema{{Slot: "report", SchemaID: "s", Document: json.RawMessage(`{}`)}}, want: goobersIOPublicationReceiptFile()},
		{name: "unstructured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := RunRequest{Workspace: t.TempDir(), Envelope: apiv1.InvocationEnvelope{RunID: "run"}, PublicationSchemas: tc.schemas}
			runtime, ok, err := prepareGoobersIOMCPRuntime(req, "goobers")
			if err != nil || !ok {
				t.Fatalf("prepare: ok=%v err=%v", ok, err)
			}
			cfg, err := mcpio.LoadConfig(runtime.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PublicationReceiptFile != tc.want {
				t.Fatalf("publicationReceiptFile=%q, want %q", cfg.PublicationReceiptFile, tc.want)
			}
		})
	}
}
