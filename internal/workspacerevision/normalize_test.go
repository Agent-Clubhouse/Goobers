package workspacerevision

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestWorkspaceRevisionResultNormalization(t *testing.T) {
	for _, status := range []apiv1.ResultStatus{apiv1.ResultSuccess, apiv1.ResultFailure, apiv1.ResultBlocked, apiv1.ResultNoWork} {
		for _, deterministic := range []bool{false, true} {
			for _, malformed := range []bool{false, true} {
				revision := validRevision()
				if malformed {
					revision.CommitSHA = "short"
				}
				result := apiv1.ResultEnvelope{Status: status, WorkspaceRevision: revision}
				err := NormalizeResult(&result, deterministic)
				wantCode := ""
				if malformed {
					wantCode = CodeInvalid
				} else if !deterministic {
					wantCode = CodeUnauthorized
				}
				if (err == nil) != (wantCode == "") || (err != nil && err.Code != wantCode) {
					t.Fatalf("status=%s deterministic=%v malformed=%v: %v", status, deterministic, malformed, err)
				}
				if err == nil && (result.WorkspaceRevision != nil) != (status == apiv1.ResultSuccess) {
					t.Fatalf("status=%s retained wrong authority: %+v", status, result)
				}
				if result.Status != status {
					t.Fatal("normalization changed the original status")
				}
			}
		}
	}
}
