package providers

import "testing"

func TestADOMergeabilityDistinguishesConflictFromUnknown(t *testing.T) {
	for _, status := range []string{"conflicts", "succeeded", "queued", "notSet", "failure", "rejectedByPolicy", ""} {
		t.Run(status, func(t *testing.T) {
			got := adoMergeable(status)
			switch status {
			case "conflicts":
				if got == nil || *got {
					t.Fatal("confirmed conflict lost")
				}
			case "succeeded":
				if got == nil || !*got {
					t.Fatal("successful merge lost")
				}
			default:
				if got != nil {
					t.Fatalf("%s must remain unknown", status)
				}
			}
		})
	}
}
