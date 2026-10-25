//go:build cloudintegration

package childengine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
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
// the reason, and recovers on the same worker once the pod blob plane can read it.
func TestChildQualificationCanaryGuard(t *testing.T) {
	h := newQualificationHarness(t)
	digest, canary := childresult.BlobPlaneCanary(testInstance)
	if err := h.blobs.Remove(h.ctx, digest); err != nil {
		t.Fatal(err)
	}
	run := h.StartParent()
	t.Run("unreadable_withholds_and_logs", func(t *testing.T) {
		// Synchronize on the worker's actual probe refusal, not a boot delay.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			data, err := os.ReadFile(h.workerLog.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "child authority withheld: "+childresult.ErrBlobPlaneUnverified.Error()) {
				break
			}
			select {
			case <-h.ctx.Done():
				t.Fatalf("no canary refusal logged: %v\n%s", h.ctx.Err(), data)
			case <-ticker.C:
			}
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
	if t.Failed() {
		return
	}
	t.Run("readable_grants_on_same_worker", func(t *testing.T) {
		if err := h.blobs.Put(h.ctx, digest, canary); err != nil {
			t.Fatal(err)
		}
		var result engine.RunResult
		if err := run.Get(h.ctx, &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != engine.StatusCompleted || result.Outputs["plan"].Summary != "parent read child receipt through blob plane" {
			t.Fatalf("parent after canary restoration: %+v", result)
		}
		var count int
		if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM control.child_lineage WHERE instance_id=$1 AND parent_run_id=$2 AND state='completed'`, testInstance, run.GetID()).Scan(&count); err != nil || count != 1 {
			t.Fatalf("completed children after verification=%d: %v", count, err)
		}
	})
}
