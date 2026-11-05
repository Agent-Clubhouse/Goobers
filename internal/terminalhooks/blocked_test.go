package terminalhooks

import (
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestParseBlockedByAndFilter(t *testing.T) {
	if got := ParseBlockedBy(map[string]interface{}{OutputBlockedBy: "#12; 13, 12, x"}); !slices.Equal(got, []string{"12", "13"}) {
		t.Fatalf("ParseBlockedBy = %v", got)
	}
	kept, dropped := FilterSelfBlockers([]string{"owner/repo#441", "7"}, "441")
	if !slices.Equal(kept, []string{"7"}) || !slices.Equal(dropped, []string{"owner/repo#441"}) {
		t.Fatalf("kept %v dropped %v", kept, dropped)
	}
	if BlockedReason(apiv1.ResultEnvelope{}) == "" || BlockedReason(apiv1.ResultEnvelope{Error: &apiv1.ErrorInfo{Code: "X", Message: "m"}}) != "X: m" {
		t.Fatal("BlockedReason is wrong")
	}
}
