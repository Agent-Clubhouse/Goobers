package startuphint

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSetParseRoundTripAndRejectsMalformed(t *testing.T) {
	h := http.Header{}
	Set(h, Hints{BudgetRemaining: 90*time.Second + 500*time.Millisecond, HasBudget: true, Progress: "7"})
	if got := Parse(h); got != (Hints{BudgetRemaining: 90 * time.Second, HasBudget: true, Progress: "7"}) {
		t.Fatalf("round trip = %+v", got)
	}

	h = http.Header{}
	Set(h, Hints{BudgetRemaining: -time.Minute, HasBudget: true})
	if got := Parse(h); !got.HasBudget || got.BudgetRemaining != 0 {
		t.Fatalf("spent budget = %+v, want an advertised zero", got)
	}

	for _, raw := range []string{"-5", "soon", "1.5"} {
		h = http.Header{HeaderBudgetRemaining: {raw}, HeaderProgress: {strings.Repeat("x", maxProgressLen+1)}}
		if got := Parse(h); got != (Hints{}) {
			t.Fatalf("malformed %q parsed as %+v", raw, got)
		}
	}

	h = http.Header{HeaderBudgetRemaining: {"999999999999"}}
	if got := Parse(h); got.BudgetRemaining != maxBudgetRemaining {
		t.Fatalf("oversized budget = %s, want capped at %s", got.BudgetRemaining, maxBudgetRemaining)
	}
}
