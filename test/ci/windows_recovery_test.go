package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestCIWindowsRecoveryCoverage(t *testing.T) {
	t.Parallel()
	workflow := loadCIWorkflow(t)
	job := workflow.Jobs["windows-smoke"]
	if job.RunsOn != "windows-latest" || job.If != ciFullGate || job.ContinueOnError ||
		!slices.Contains(job.Needs, "scope") || !slices.Contains(workflow.Jobs["required-ci"].Needs, "windows-smoke") {
		t.Fatal("recovery regressions must run in the required full-profile native Windows gate")
	}
	step := job.step(t, "Windows dispatch heartbeat and recovery fences")
	if step.If != "" || step.ContinueOnError {
		t.Fatal("Windows heartbeat and recovery regressions must not be skipped or advisory")
	}
	invocations := parseGoTestInvocations(step.Run)
	if len(invocations) != 1 || !strings.HasSuffix(step.Run, " -count=1 -timeout=3m") {
		t.Fatal("Windows recovery selection must be one bounded, uncached test command")
	}
	selection := regexp.MustCompile(invocations[0].run)
	for pkg, names := range map[string][]string{
		"./internal/engine": {
			"TestDispatchCancellationOptionsPreserveLegacyAndLocalStages",
			"TestReviewerDispatchReceivesCancellationOptions",
			"TestDispatchHeartbeatCarriesGenerationDiagnostic",
			"TestDispatchCancellationPreservesSurrenderAndDeadlineClassification",
			"TestConfirmedSurrenderReadSurvivesCancellationDuringGetWithBound",
		},
		"./internal/temporaldial": {
			"TestHeartbeatRPCDiagnosticsDistinguishGenerationTransportAndAcceptance",
			"TestHeartbeatRPCDiagnosticsIgnoreOtherRPCs",
			"TestHeartbeatRPCDiagnosticsIgnoreUnrecognizedHeartbeatDetails",
			"TestHeartbeatRPCDiagnosticsBoundRetainedDispatchTasks",
			"TestOptionsUnsetIsTodaysDial",
		},
		"./internal/recovery": {
			"TestHTTPArchivePublisherRequiresCompleteCustodyAcknowledgement",
			"TestHTTPArchivePublisherRejectsPrematureSuccess",
			"TestHTTPArchivePublisherScrubsTransportFailure",
			"TestHTTPArchivePublisherClientTimeoutOutlastsServerBudget",
			"TestHTTPArchivePublisherDiagnosesServerClosedStream",
			"TestHTTPArchiveDownloadTimeoutOutlastsServerBudget",
			"TestClassifyTransportDistinguishesCauses",
		},
		"./cmd/goobers": {
			"TestRecoveryDeliveryServiceStreamsOnlyVerifiedClaimedState",
			"TestRecoveryDeliveryRequiresExclusiveIssueLease",
			"TestRecoveryDeliveryAcknowledgementHoldsExclusiveClaimFence",
			"TestRecoveryPublicationReleasedClaimDispositionFence",
			"TestRecoveryPublicationReleasedSharedClaimCannotBypassRemoteOwnership",
		},
	} {
		if !slices.Contains(invocations[0].packages, pkg) {
			t.Errorf("Windows recovery selection omits package %s", pkg)
		}
		for _, name := range names {
			if !selection.MatchString(name) {
				t.Errorf("Windows recovery selection omits %s", name)
			}
		}
	}
	if selection.MatchString("TestUnrelatedCLIBehavior") {
		t.Fatal("Windows recovery step must not select the whole CLI suite")
	}
	custody := job.step(t, "Windows recovery publication custody")
	if custody.If != "" || custody.ContinueOnError || custody.Env["TESTDEP_STRICT"] != "1" ||
		custody.Run != "go test -tags=integration ./cmd/goobers -run '^TestIntegrationRecoveryPublicationTakesVerifiedHostCustody$' -count=1 -timeout=5m" {
		t.Fatal("native Windows custody must exercise the real Git integration with strict dependency checks")
	}
}
