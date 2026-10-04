//go:build integration

package main

import (
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationDispatchExecWorkspaceRevision(t *testing.T) {
	testdep.Require(t, "sh")
	sha := strings.Repeat("a", 40)
	valid := fmt.Sprintf(`{"repository":{"provider":"github","owner":"org","name":"repo"},"commitSha":%q}`, sha)
	for _, tc := range []struct {
		name, control, extra string
		exit                 int
		want                 apiv1.ResultStatus
		invalid              bool
	}{
		{name: "success", control: valid, want: apiv1.ResultSuccess},
		{name: "failure", control: valid, exit: 3, want: apiv1.ResultFailure},
		{name: "typed-failure", control: valid, extra: `,"errorCode":"provider_failure","errorMessage":"provider failed","errorRetryable":true`, exit: 3, want: apiv1.ResultFailure},
		{name: "no-work", control: valid, extra: `,"noWork":true`, want: apiv1.ResultNoWork},
		{name: "scalar", control: `"not-authority"`, want: apiv1.ResultFailure, invalid: true},
		{name: "null", control: `null`, want: apiv1.ResultFailure, invalid: true},
		{name: "malformed-sha", control: strings.Replace(valid, sha, "short", 1), want: apiv1.ResultFailure, invalid: true},
		{name: "unknown-control", control: strings.TrimSuffix(valid, "}") + `,"unknown":true}`, want: apiv1.ResultFailure, invalid: true},
		{name: "unknown-repository", control: strings.Replace(valid, `"name":"repo"`, `"name":"repo","credential":"untrusted"`, 1), want: apiv1.ResultFailure, invalid: true},
		{name: "invalid-on-failure", control: `"not-authority"`, exit: 3, want: apiv1.ResultFailure, invalid: true},
		{name: "invalid-on-no-work", control: `"not-authority"`, extra: `,"noWork":true`, want: apiv1.ResultFailure, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runStageWithResultFile(t, `{"legacy":"kept","workspaceRevision":`+tc.control+tc.extra+`}`, tc.exit)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s: %+v", got.Status, tc.want, got)
			}
			if _, ok := got.Outputs["workspaceRevision"]; ok {
				t.Fatal("revision control escaped into scalar outputs")
			}
			if got.Metrics["exitCode"] != float64(tc.exit) {
				t.Fatalf("exit-code metric was lost: %+v", got.Metrics)
			}
			if tc.name == "typed-failure" && (got.Error == nil || got.Error.Code != "provider_failure" || got.Error.Message != "provider failed" || !got.Error.Retryable) {
				t.Fatalf("typed error was lost: %+v", got.Error)
			}
			if tc.invalid {
				if got.Error == nil || got.Error.Code != "workspace_revision_invalid" || got.Error.Retryable {
					t.Fatalf("invalid control was not refused: %+v", got)
				}
			} else if got.Outputs["legacy"] != "kept" {
				t.Fatalf("legacy output was lost: %+v", got.Outputs)
			}
			if tc.want == apiv1.ResultSuccess {
				if got.WorkspaceRevision == nil || got.WorkspaceRevision.CommitSHA != sha {
					t.Fatalf("pod success lost typed authority: %+v", got)
				}
			} else if got.WorkspaceRevision != nil {
				t.Fatalf("unsuccessful result established authority: %+v", got)
			}
		})
	}
}
