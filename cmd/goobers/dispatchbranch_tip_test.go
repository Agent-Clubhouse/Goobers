package main

import (
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestDispatchExecOwnedBranchTipPromotion(t *testing.T) {
	tip := strings.Repeat("a", 40)
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			result := runStageWithResultFile(t, `{"workspaceBranchTip":"`+tip+`"}`, exit)
			if exit == 0 && (result.Status != apiv1.ResultSuccess || result.WorkspaceBranchTip != tip) {
				t.Fatalf("publication tip was lost: %+v; error: %+v", result, result.Error)
			}
			if exit != 0 && (result.Status != apiv1.ResultFailure || result.WorkspaceBranchTip != "") {
				t.Fatalf("failed publication retained authority: %+v", result)
			}
			if _, ok := result.Outputs["workspaceBranchTip"]; ok {
				t.Fatal("publication tip escaped into scalar outputs")
			}
		})
	}
}
