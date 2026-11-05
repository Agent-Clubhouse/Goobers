//go:build cloudintegration

package childengine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
)

// Scenario 8: an unreadable per-instance canary withholds authority, reports
// the reason. A worker with a readable canary admits and completes the child.
func TestChildQualificationCanaryGuard(t *testing.T) {
	t.Run("unreadable_withholds_and_logs", func(t *testing.T) {
		h := newQualificationHarness(t)
		digest, _ := childresult.BlobPlaneCanary(testInstance)
		if err := h.blobs.Remove(h.ctx, digest); err != nil {
			t.Fatal(err)
		}
		run := h.StartParent()
		// Wait for the production workflow to report the probe refusal, then join
		// the worker before reading its captured log buffer.
		var result engine.RunResult
		err := run.Get(h.ctx, &result)
		h.KillWorker()
		if err == nil || !strings.Contains(err.Error(), childresult.ErrBlobPlaneUnverified.Error()) {
			t.Fatalf("parent without readable canary: result=%+v err=%v", result, err)
		}
		output := h.workerLogs[0].String()
		if !strings.Contains(output, "child authority withheld: "+childresult.ErrBlobPlaneUnverified.Error()) {
			t.Fatalf("no canary refusal logged:\n%s", output)
		}
		for _, table := range []string{"child_authority", "child_lineage"} {
			var count int
			if err := h.pool.QueryRow(h.ctx, "SELECT count(*) FROM control."+table+" WHERE instance_id=$1", testInstance).Scan(&count); err != nil || count != 0 {
				t.Fatalf("%s before canary verification: count=%d err=%v", table, count, err)
			}
		}
		// A pod cannot bypass the withheld grant by submitting with its ordinary
		// bearer. Exercise the real authenticated child-start route as well.
		key, err := podauth.LoadKeyRingFile(h.keyFile, "")
		if err != nil {
			t.Fatal(err)
		}
		token, err := key.MintScoped(run.GetID(), time.Minute, podauth.ScopeBlob)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]string{"source": generatedChild})
		if err != nil {
			t.Fatal(err)
		}
		path := strings.Replace(apicontract.ChildWorkflowStartPath, "{run}", run.GetID(), 1)
		req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, h.server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(httpapi.HeaderIdempotencyKey, "canary-refused-child")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("child request without authority: HTTP %d", resp.StatusCode)
		}
	})
	t.Run("readable_grants_authority", func(t *testing.T) {
		h := newQualificationHarness(t)
		run := h.StartParent()
		var result engine.RunResult
		if err := run.Get(h.ctx, &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "parent read child receipt through blob plane" {
			t.Fatalf("parent with readable canary: %+v", result)
		}
		var count int
		if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND parent_run_id=$2 AND state='completed'`, testInstance, run.GetID()).Scan(&count); err != nil || count != 1 {
			t.Fatalf("completed children after verification=%d: %v", count, err)
		}
	})
}
