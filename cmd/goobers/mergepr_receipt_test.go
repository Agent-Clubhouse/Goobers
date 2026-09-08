package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestMergePRReceiptFailurePreservesSuccessfulMerge(t *testing.T) {
	st := &mergePRServerState{checkState: "success", headSHA: "head123", baseSHA: "base456"}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})
	// Preserve the durable intent but make its original path unwritable after
	// the mutation begins. Only fixture-owned files are moved.
	st.beforeMergeReply = func() error {
		path := filepath.Join(dir, mutationsSidecarFile)
		if err := os.Rename(path, path+".saved"); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	}
	code, stdout, stderr := runArgs(t, "merge-pr", root)
	if code != 1 || !strings.Contains(stderr, "durable receipt persistence failed") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if st.mergeCalls != 1 || st.deleteCalls != 0 || st.pullListCalls != 0 {
		t.Fatalf("merge=%d delete=%d cleanup-list=%d", st.mergeCalls, st.deleteCalls, st.pullListCalls)
	}
	result := readMergeResult(t, dir)
	for key, want := range map[string]interface{}{
		"merged": true, "landOutcome": "merged", "mergeSha": "merge-commit-sha",
		"selectedNumber": "9", "selectedHeadSha": "head123", "reason": "",
		executor.OutputErrorCode:      "landing_receipt_persistence_failed",
		executor.OutputErrorRetryable: false,
	} {
		if result[key] != want {
			t.Errorf("%s=%v, want %v", key, result[key], want)
		}
	}
	if strings.Contains(stdout, "not merged") || strings.Contains(stderr, dir) {
		t.Fatalf("misleading refusal or private storage detail: stdout=%q stderr=%q", stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, mutationsSidecarFile+".saved"))
	if err != nil || !strings.Contains(string(data), `"operation":"merge-intent"`) {
		t.Fatalf("original intent missing: %s, %v", data, err)
	}
}

func TestLandingReceiptSidecarReturnsFailureAndFlushesCancelledContext(t *testing.T) {
	t.Chdir(t.TempDir())
	recorder := sidecarMutationRecorder{kind: "pr"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ref := providers.ExternalRef{Provider: providers.ProviderGitHub, Ref: "org/repo#9", Operation: "merge"}
	if err := recorder.RecordLandingReceipt(ctx, ref); err != nil {
		t.Fatalf("cancelled request discarded successful receipt: %v", err)
	}
	if facts := readMutationFacts(t, "."); len(facts) != 1 || facts[0].Operation != "merge" {
		t.Fatalf("receipt not flushed: %+v", facts)
	}
	if err := os.Rename(mutationsSidecarFile, mutationsSidecarFile+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mutationsSidecarFile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLandingReceipt(ctx, ref); err == nil {
		t.Fatal("receipt persistence failure swallowed")
	}
}

func TestMergePRReceiptFailurePreservesAcceptedQueue(t *testing.T) {
	st := &mergePRServerState{checkState: "success", headSHA: "head123", baseSHA: "base456", mergeQueueRules: true}
	server := newMergePRServer(t, "your-org", "your-repo", st)
	root, dir := mergePREnv(t, server.URL, false, map[string]string{
		"pullNumber": "9", "verdict": "pass", "headSha": "head123", "baseSha": "base456",
	})
	st.beforeMergeReply = func() error {
		path := filepath.Join(dir, mutationsSidecarFile)
		if err := os.Rename(path, path+".saved"); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	}
	code, stdout, stderr := runArgs(t, "merge-pr", root)
	if code != 1 || !strings.Contains(stderr, "durable receipt persistence failed") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if st.enqueueCalls != 1 || st.mergeCalls != 0 || st.deleteCalls != 0 {
		t.Fatalf("enqueue=%d merge=%d delete=%d", st.enqueueCalls, st.mergeCalls, st.deleteCalls)
	}
	result := readMergeResult(t, dir)
	if result["merged"] != false || result["landOutcome"] != "enqueued" || result[executor.OutputErrorCode] != "landing_receipt_persistence_failed" || result[executor.OutputErrorRetryable] != false {
		t.Fatalf("queue receipt failure lost acceptance or promoted merge: %+v", result)
	}
	if _, exists := result["mergeSha"]; exists {
		t.Fatalf("invented merge SHA: %+v", result)
	}
}
