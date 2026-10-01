package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	apivalidate "github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// #6131: durable review-thread publication receipts. These tests drive the
// real resolve-review-threads stage against the GitHub and Azure DevOps fakes
// and play the executor's part between attempts: whatever the stage leaves
// in its result file is journaled as "<runID>:resolve-review-threads/result"
// (recordReceipt), exactly what a stage retry, a daemon restart or a
// crash-resume sees on the next attempt.

// receiptAttempt is one resolve-review-threads invocation and the receipt it
// left in its result file.
type receiptAttempt struct {
	code    int
	raw     []byte
	receipt apiv1.ReviewThreadPublication
}

// runReceiptAttempt runs resolve-review-threads once in a fresh result
// directory (a scratch workspace never survives an attempt) and validates
// the receipt it left against the published schema.
func runReceiptAttempt(t *testing.T, w feedbackWorld) receiptAttempt {
	t.Helper()
	resultFile := filepath.Join(t.TempDir(), resolveReviewThreadsResultFile)
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	code, stdout, stderr := runArgs(t, "resolve-review-threads", w.root)
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("resolve-review-threads left no receipt: %v (code = %d, stdout = %q, stderr = %q)", err, code, stdout, stderr)
	}
	validateReceiptSchema(t, data)
	var receipt apiv1.ReviewThreadPublication
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receiptAttempt{code: code, raw: data, receipt: receipt}
}

func validateReceiptSchema(t *testing.T, data []byte) {
	t.Helper()
	validator, err := apivalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON(schemas.ReviewThreadPublication, data); err != nil {
		t.Fatalf("receipt does not match %s: %v\n%s", schemas.ReviewThreadPublication, err, data)
	}
}

// recordReceipt journals an attempt's result file the way the executor does
// on every exit of a provider stage.
func recordReceipt(t *testing.T, run *revisionRun, attempt receiptAttempt) {
	t.Helper()
	if _, err := run.run.RecordArtifact(run.runID+":resolve-review-threads/result", attempt.raw); err != nil {
		t.Fatal(err)
	}
}

// newReceiptWorld is a published remediation pass ready to answer one
// addressed review thread at revisionPublishedSHA.
func newReceiptWorld(t *testing.T, kind providers.ProviderKind) (feedbackWorld, *revisionRun) {
	t.Helper()
	w := newFeedbackWorld(t, kind)
	setDaemonStageAttributionEnv(t)
	run := newRevisionRun(t, w.root, w.runID)
	run.selectHead("77", revisionSelectedSHA)
	gatherIntoJournal(t, w, run)
	seedResolutionAfterGather(t, run, w.threadID, revisionPublishedSHA)
	w.setHead(revisionPublishedSHA)
	return w, run
}

func onlyThread(t *testing.T, receipt apiv1.ReviewThreadPublication) apiv1.ReviewThreadReceipt {
	t.Helper()
	if len(receipt.Threads) != 1 {
		t.Fatalf("receipt threads = %+v, want exactly the one answered thread", receipt.Threads)
	}
	return receipt.Threads[0]
}

// TestReviewThreadReceiptRecoversAtEveryMutationBoundary injects a failure at
// each mutation boundary — before and after the provider applies the reply,
// before and after it applies the resolution — and checks that the stopped
// attempt's receipt says exactly which mutations completed, and that the next
// attempt reconciles it with provider state and finishes with exactly one
// reply and one resolution.
func TestReviewThreadReceiptRecoversAtEveryMutationBoundary(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		for _, tc := range []struct {
			fault string
			// After the faulted attempt.
			wantStatus, wantReply, wantResolution string
			// After the recovering attempt.
			wantRecovery string
		}{
			{fault: "reply", wantStatus: apiv1.ReviewThreadPublicationFailed,
				wantReply: apiv1.ReviewThreadMutationFailed, wantResolution: apiv1.ReviewThreadMutationPending},
			{fault: "reply-applied", wantStatus: apiv1.ReviewThreadPublicationFailed,
				wantReply: apiv1.ReviewThreadMutationFailed, wantResolution: apiv1.ReviewThreadMutationPending,
				wantRecovery: apiv1.ReviewThreadRecoveryProviderAdopted},
			{fault: "resolve", wantStatus: apiv1.ReviewThreadPublicationPartial,
				wantReply: apiv1.ReviewThreadMutationVerified, wantResolution: apiv1.ReviewThreadMutationFailed,
				wantRecovery: apiv1.ReviewThreadRecoveryReceiptConfirmed},
			{fault: "resolve-applied", wantStatus: apiv1.ReviewThreadPublicationPartial,
				wantReply: apiv1.ReviewThreadMutationVerified, wantResolution: apiv1.ReviewThreadMutationFailed,
				wantRecovery: apiv1.ReviewThreadRecoveryProviderAdopted},
		} {
			t.Run(string(kind)+"/"+tc.fault, func(t *testing.T) {
				w, run := newReceiptWorld(t, kind)
				w.arm(tc.fault)

				stopped := runReceiptAttempt(t, w)
				entry := onlyThread(t, stopped.receipt)
				if stopped.code != 1 || stopped.receipt.Status != tc.wantStatus || stopped.receipt.ErrorCode == "" {
					t.Fatalf("faulted attempt: code = %d, receipt = %s; want exit 1 with a typed %s receipt", stopped.code, stopped.raw, tc.wantStatus)
				}
				if entry.ReplyState != tc.wantReply || entry.ResolutionState != tc.wantResolution || entry.LastError == "" {
					t.Fatalf("faulted attempt thread = %+v, want reply %s, resolution %s and the error recorded", entry, tc.wantReply, tc.wantResolution)
				}
				if entry.ReplyState == apiv1.ReviewThreadMutationVerified && entry.ProviderReplyID == "" {
					t.Fatalf("verified reply recorded no provider id: %+v", entry)
				}
				recordReceipt(t, run, stopped)

				resumed := runReceiptAttempt(t, w)
				entry = onlyThread(t, resumed.receipt)
				if resumed.code != 0 || resumed.receipt.Status != apiv1.ReviewThreadPublicationComplete || !resumed.receipt.ResumedFromReceipt {
					t.Fatalf("recovering attempt: code = %d, receipt = %s; want a complete receipt resumed from the stopped one", resumed.code, resumed.raw)
				}
				if entry.ReplyState != apiv1.ReviewThreadMutationVerified || entry.ResolutionState != apiv1.ReviewThreadMutationVerified ||
					entry.ProviderReplyID == "" || entry.Recovery != tc.wantRecovery {
					t.Fatalf("recovered thread = %+v, want both mutations verified with recovery %q", entry, tc.wantRecovery)
				}
				if w.replies() != 1 || !w.resolved() {
					t.Fatalf("replies = %d resolved = %v, want exactly one reply and the thread resolved", w.replies(), w.resolved())
				}
				if resumed.receipt.StaleInput != "" || resumed.receipt.UnresolvedThreadCount != "0" {
					t.Fatalf("complete receipt outputs = %s, want staleInput \"\" and no unresolved threads", resumed.raw)
				}
			})
		}
	}
}

// TestReviewThreadReceiptAdoptsUnrecordedMutationsAfterCrash: a daemon crash
// can lose the attempt's result before the executor journals it. The next
// attempt then has no receipt to resume, so provider state alone decides: the
// reply the lost attempt published is adopted by its marker, never posted a
// second time.
func TestReviewThreadReceiptAdoptsUnrecordedMutationsAfterCrash(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		t.Run(string(kind), func(t *testing.T) {
			w, _ := newReceiptWorld(t, kind)
			w.arm("reply-applied")
			if lost := runReceiptAttempt(t, w); lost.code != 1 {
				t.Fatalf("faulted attempt: code = %d, receipt = %s", lost.code, lost.raw)
			}
			resumed := runReceiptAttempt(t, w)
			entry := onlyThread(t, resumed.receipt)
			if resumed.code != 0 || resumed.receipt.Status != apiv1.ReviewThreadPublicationComplete || resumed.receipt.ResumedFromReceipt {
				t.Fatalf("code = %d, receipt = %s; want a complete receipt that resumed nothing", resumed.code, resumed.raw)
			}
			if entry.Recovery != apiv1.ReviewThreadRecoveryProviderAdopted || w.replies() != 1 || !w.resolved() {
				t.Fatalf("thread = %+v replies = %d resolved = %v; want the lost attempt's reply adopted, not reposted", entry, w.replies(), w.resolved())
			}
		})
	}
}

// TestReviewThreadReceiptStopsOnDivergedProviderState: a mutation the receipt
// records as verified but the provider no longer shows was undone by someone
// else — this run's reply deleted, or the thread it resolved reopened. The
// retry never silently redoes it: it publishes nothing and stops as typed
// stale input, with the receipt as evidence, so the workflow re-gathers.
func TestReviewThreadReceiptStopsOnDivergedProviderState(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		for _, tc := range []struct {
			name    string
			fault   string
			undo    func(w feedbackWorld)
			replies int
		}{
			{name: "reply deleted", fault: "resolve", undo: func(w feedbackWorld) { w.deleteOwnReplies() }, replies: 0},
			{name: "resolution reopened", undo: func(w feedbackWorld) { w.reopen() }, replies: 1},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				w, run := newReceiptWorld(t, kind)
				if tc.fault != "" {
					w.arm(tc.fault)
				}
				first := runReceiptAttempt(t, w)
				recordReceipt(t, run, first)
				tc.undo(w)

				retry := runReceiptAttempt(t, w)
				entry := onlyThread(t, retry.receipt)
				if retry.code != 0 || retry.receipt.Status != apiv1.ReviewThreadPublicationStale ||
					retry.receipt.StaleInput != staleReasonThreadState || !retry.receipt.ResumedFromReceipt {
					t.Fatalf("code = %d, receipt = %s; want typed changed_thread_state stale input", retry.code, retry.raw)
				}
				if len(retry.receipt.StaleReasons) != 1 || retry.receipt.StaleReasons[0].ID != w.threadID || entry.LastError == "" {
					t.Fatalf("receipt = %s, want the diverged thread named as evidence", retry.raw)
				}
				if w.replies() != tc.replies || (tc.name == "resolution reopened" && w.resolved()) {
					t.Fatalf("replies = %d resolved = %v, want the undone mutation left undone", w.replies(), w.resolved())
				}
			})
		}
	}
}

// TestReviewThreadReceiptRecordsStaleStops: publication that stops on changed
// head or feedback still leaves a receipt naming exactly what completed.
func TestReviewThreadReceiptRecordsStaleStops(t *testing.T) {
	for _, kind := range mutatingFeedbackProviders {
		t.Run(string(kind)+"/stale feedback", func(t *testing.T) {
			w, _ := newReceiptWorld(t, kind)
			w.addComment("One more thing.")
			stopped := runReceiptAttempt(t, w)
			entry := onlyThread(t, stopped.receipt)
			if stopped.code != 0 || stopped.receipt.Status != apiv1.ReviewThreadPublicationStale || stopped.receipt.StaleInput != staleReasonNew {
				t.Fatalf("code = %d, receipt = %s; want a stale receipt", stopped.code, stopped.raw)
			}
			if entry.ReplyState != apiv1.ReviewThreadMutationPending || entry.ResolutionState != apiv1.ReviewThreadMutationPending || w.replies() != 0 {
				t.Fatalf("thread = %+v replies = %d, want nothing published", entry, w.replies())
			}
		})
		t.Run(string(kind)+"/moved head", func(t *testing.T) {
			w, _ := newReceiptWorld(t, kind)
			w.setHead(revisionMovedSHA)
			stopped := runReceiptAttempt(t, w)
			if stopped.code != 0 || !stopped.receipt.NoWork || stopped.receipt.Status != apiv1.ReviewThreadPublicationStale ||
				stopped.receipt.Outcome != staleReasonHead || stopped.receipt.LiveHeadSHA != revisionMovedSHA {
				t.Fatalf("code = %d, receipt = %s; want a stale-head no-work receipt", stopped.code, stopped.raw)
			}
		})
	}
}

// TestReviewThreadReceiptIgnoresAnotherPass: a receipt for another published
// head or feedback snapshot belongs to an earlier pass of the remediation
// loop and is not resumed.
func TestReviewThreadReceiptIgnoresAnotherPass(t *testing.T) {
	w, run := newReceiptWorld(t, providers.ProviderGitHub)
	other := newReviewThreadReceipt("77", revisionMovedSHA, nil, []reviewThreadDisposition{{ThreadID: w.threadID, Disposition: "addressed", Detail: "x"}})
	other.Threads[0].ReplyState = apiv1.ReviewThreadMutationVerified
	data, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	recordReceipt(t, run, receiptAttempt{raw: data})
	attempt := runReceiptAttempt(t, w)
	if attempt.code != 0 || attempt.receipt.ResumedFromReceipt || attempt.receipt.Status != apiv1.ReviewThreadPublicationComplete || w.replies() != 1 {
		t.Fatalf("code = %d, receipt = %s, replies = %d; want a fresh, complete transaction", attempt.code, attempt.raw, w.replies())
	}
}

// TestReviewThreadReceiptUsesTheRunnersTaskName: a workflow may name the
// stage something other than the command; the receipt is found under the
// task name the runner journals it as.
func TestReviewThreadReceiptUsesTheRunnersTaskName(t *testing.T) {
	t.Setenv(executor.TaskEnvVar, "answer-threads")
	if got := reviewThreadReceiptStage(); got != "answer-threads" {
		t.Fatalf("stage = %q, want the runner's task name", got)
	}
	t.Setenv(executor.TaskEnvVar, "")
	if got := reviewThreadReceiptStage(); got != defaultResolveReviewThreadsStage {
		t.Fatalf("stage = %q, want the command's default", got)
	}
}
