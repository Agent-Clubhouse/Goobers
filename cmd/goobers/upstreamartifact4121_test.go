package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// upstreamartifact4121_test.go pins Goobers#4121: an artifact an upstream
// stage of this same run owed this one is the SUBSTRATE's job to carry. When
// it does not arrive, the failure carries no evidence about the work item, so
// it must not accumulate failure-streak strikes — and it must be retryable,
// because on the pod arm the producer emits the artifact op over the journal
// plane and a DIFFERENT pod reads it back.
//
// Measured: three terminal `gather-ci-failures` runs classified provider_error
// parked live PR #3900 goobers:needs-human (#4119). #4103 and #4106 did the
// same to #3894 and #3908 through `gather-pr-context`.

func TestUpstreamArtifactFailureIsAnInfraFault(t *testing.T) {
	for name, err := range map[string]error{
		"missing":    upstreamArtifactMissing("gather-pr-context", remediationBriefArtifact),
		"unreadable": upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, errors.New("blob plane unreachable")),
		"wrapped":    fmt.Errorf("read brief: %w", upstreamArtifactMissing("gather-pr-context", remediationBriefArtifact)),
	} {
		t.Run(name, func(t *testing.T) {
			code, retryable, _ := classifyProviderError(err)
			if code != telemetry.ErrCodeInfraJournal {
				t.Fatalf("code = %q, want %q", code, telemetry.ErrCodeInfraJournal)
			}
			if !retryable {
				t.Fatal("an artifact the plane has not applied yet clears on its own; the stage retry budget is the instrument for it")
			}
			// The property the circuit breaker actually reads
			// (runnerwiring_notifications.go's #3361/#3364 exemption).
			if class := telemetry.ClassifyError(code); !class.InfraFault() {
				t.Fatalf("ClassifyError(%q) = %q, which is not an InfraFault: the failure-streak breaker will strike the item for it and eventually park it goobers:needs-human", code, class)
			}
		})
	}
}

// The branch is LAST, so nothing a provider actually said gets reclassified as
// this run's own infrastructure — the narrowing #4106 had to learn the hard
// way with rejected pushes.
func TestProviderConditionsStillWinOverTheUpstreamArtifactBranch(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		wantCode  string
		wantRetry bool
	}{
		"rate limit": {
			err:       fmt.Errorf("read brief: %w", &providers.RateLimitError{}),
			wantCode:  providers.ErrorCodeRateLimited,
			wantRetry: true,
		},
		"server error": {
			err:       fmt.Errorf("upstream artifact: status 503 unavailable"),
			wantCode:  errorCodeServerError,
			wantRetry: true,
		},
		"plain provider error": {
			err:       errors.New("status 422 unprocessable"),
			wantCode:  errorCodeProvider,
			wantRetry: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			code, retryable, _ := classifyProviderError(tc.err)
			if code != tc.wantCode || retryable != tc.wantRetry {
				t.Fatalf("classifyProviderError = (%q, %v), want (%q, %v)", code, retryable, tc.wantCode, tc.wantRetry)
			}
		})
	}
}

// The message an operator reads must still name the stage that owed the
// artifact and what it owed; the classification is not allowed to cost that.
func TestUpstreamArtifactErrorNamesItsProducer(t *testing.T) {
	got := upstreamArtifactMissing("gather-pr-context", remediationBriefArtifact).Error()
	want := "gather-pr-context produced no remediation brief artifact"
	if got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	wrapped := upstreamArtifactUnreadable("gather-pr-context", remediationBriefArtifact, errors.New("boom"))
	if wrapped.Error() != "read remediation brief artifact from gather-pr-context: boom" {
		t.Fatalf("Error() = %q", wrapped.Error())
	}
	if !errors.Is(wrapped, errors.Unwrap(wrapped)) {
		t.Fatal("the cause must stay reachable through errors.Is/As")
	}
}
