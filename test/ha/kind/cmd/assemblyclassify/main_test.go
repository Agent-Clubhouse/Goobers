package main

import "testing"

func TestPendingNeedsFlag(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unimplemented", value: "Claims\tpending\thttps://example/7808\n"},
		{name: "configuration drift", value: "Journal\tpending\thttps://example/7818\tneeds --object-store-url\n", want: true},
		{name: "implemented", value: "Journal\timplemented\thttps://example/7818\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pendingNeedsFlag(test.value); got != test.want {
				t.Fatalf("pendingNeedsFlag() = %v, want %v", got, test.want)
			}
		})
	}
}
