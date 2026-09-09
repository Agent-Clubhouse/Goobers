package recovery

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestMatchLandedHeadsRequiresExactPairedReceipt(t *testing.T) {
	const repository = "https://api.github.com/repos/acme/app"
	intent := providers.LandingIntent{ID: "intent", Operation: "merge", RepositoryAPIURL: repository, PullID: "42", ExpectedHeadSHA: strings.Repeat("a", 40)}
	confirmation := providers.MergeConfirmation{IntentID: intent.ID, RepositoryAPIURL: repository, PullID: "42", MergeSHA: strings.Repeat("b", 40)}
	for _, mode := range []string{"paired", "duplicate", "intent-only", "receipt-only", "enqueue", "wrong-intent", "wrong-repository", "wrong-pull", "missing-head", "missing-merge", "conflicting-intent", "conflicting-receipt"} {
		t.Run(mode, func(t *testing.T) {
			intents := []providers.LandingIntent{intent}
			receipts := []providers.MergeConfirmation{confirmation}
			switch mode {
			case "duplicate":
				intents = append(intents, intent)
				receipts = append(receipts, confirmation)
			case "intent-only":
				receipts = nil
			case "receipt-only":
				intents = nil
			case "enqueue":
				intents[0].Operation = "enqueue"
			case "wrong-intent":
				receipts[0].IntentID = "different"
			case "wrong-repository":
				receipts[0].RepositoryAPIURL += "-other"
			case "wrong-pull":
				receipts[0].PullID = "43"
			case "missing-head":
				intents[0].ExpectedHeadSHA = ""
			case "missing-merge":
				receipts[0].MergeSHA = ""
			case "conflicting-intent":
				intents = append(intents, intent)
				intents[1].ExpectedHeadSHA = strings.Repeat("c", 40)
			case "conflicting-receipt":
				receipts = append(receipts, confirmation)
				receipts[1].MergeSHA = strings.Repeat("c", 40)
			}
			heads, err := MatchLandedHeads(repository, intents, receipts)
			wantErr := strings.HasPrefix(mode, "conflicting-")
			if (err != nil) != wantErr {
				t.Fatalf("error=%v, want conflict=%t", err, wantErr)
			}
			want := 0
			if mode == "paired" || mode == "duplicate" {
				want = 1
			}
			if len(heads) != want {
				t.Fatalf("heads=%+v, want %d", heads, want)
			}
		})
	}
}
