package presentation

import "testing"

func TestFormatAIC(t *testing.T) {
	tests := []struct {
		name    string
		nanoAIU int64
		want    string
	}{
		{name: "fraction rounds down", nanoAIU: 12_490_000_000, want: "12 AIC"},
		{name: "half rounds up", nanoAIU: 12_500_000_000, want: "13 AIC"},
		{name: "zero", nanoAIU: 0, want: "0 AIC"},
		{name: "large value", nanoAIU: 9_000_000_000_000_000_000, want: "9000000000 AIC"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FormatAIC(test.nanoAIU); got != test.want {
				t.Fatalf("FormatAIC(%d) = %q, want %q", test.nanoAIU, got, test.want)
			}
		})
	}
}
