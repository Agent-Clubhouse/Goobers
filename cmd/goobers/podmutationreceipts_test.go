package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestPodMutationReceiptsSurviveSurrender(t *testing.T) {
	for _, mode := range []string{"success", "failed-stage", "refused", "malformed", "credential", "legacy", "conflicting"} {
		t.Run(mode, func(t *testing.T) {
			workspace, custody := t.TempDir(), t.TempDir()
			t.Chdir(workspace)
			data := []byte(`{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"42","operation":"merge"}`)
			switch mode {
			case "malformed":
				data = append(data, []byte("\n{broken")...)
			case "credential":
				data = []byte(`{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"42","url":"https://example.invalid/pod-secret-token"}`)
			case "legacy":
				data = []byte(`{"provider":"github","kind":"pr","id":"42"}`)
			case "conflicting":
				data = append(data, []byte("\n"+`{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"43","operation":"merge"}`)...)
			}
			path := filepath.Join(workspace, "mutations.jsonl")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			plane, err := dispatcher.NewSurrenderDir(custody)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/runs/source-run/stages/land/attempts/1/surrender" {
					t.Errorf("unexpected live-only publication: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if mode == "refused" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err == nil {
					err = plane.Put(r.Context(), "source-run", "land", 1, body)
				}
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			t.Setenv(dispatcher.EnvRunID, "source-run")
			t.Setenv(dispatcher.EnvGaggle, "gaggle")
			t.Setenv(dispatcher.EnvStage, "land")
			t.Setenv(dispatcher.EnvAttempt, "1")
			t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
			t.Setenv(dispatcher.EnvPodToken, "pod-secret-token")
			t.Setenv(dispatcher.EnvStageScript, "")
			t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","exit 0"]`)
			if mode == "failed-stage" {
				t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","exit 3"]`)
			}
			code := runDispatchExecContext(t.Context(), io.Discard, io.Discard)
			wantSuccess := mode == "success" || mode == "failed-stage"
			confirmed, err := (dispatcher.PlaneSurrenderGate{Plane: plane}).Confirmed(t.Context(), dispatcher.Attempt{RunID: "source-run", Stage: "land", Number: 1})
			if err != nil || (code == 0) != wantSuccess || confirmed != wantSuccess {
				t.Fatalf("exit=%d disposal confirmed=%v error=%v", code, confirmed, err)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
				t.Fatalf("receipt source changed: %v", err)
			}
			if !wantSuccess {
				return
			}
			reopened, err := dispatcher.NewSurrenderDir(custody)
			if err != nil {
				t.Fatal(err)
			}
			body, err := reopened.Get(t.Context(), "source-run", "land", 1)
			if err != nil {
				t.Fatal(err)
			}
			var result dispatcher.SurrenderedResult
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Mutations) != 1 || result.Mutations[0].ReceiptID != "receipt-1" || result.Mutations[0].ID != "42" || result.Mutations[0].Operation != "merge" {
				t.Fatalf("durable surrender lost receipt: %+v", result.Mutations)
			}
			if mode == "failed-stage" && result.Result.Status != apiv1.ResultFailure {
				t.Fatal("receipt handoff changed the stage's failure outcome")
			}
		})
	}
}
