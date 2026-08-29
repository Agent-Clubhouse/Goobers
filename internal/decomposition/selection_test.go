package decomposition

import "testing"

func TestIssueSnapshotDigestIsStableAndLabelOrderIndependent(t *testing.T) {
	d1, err := IssueSnapshotDigest("1", "Title", "Body", []string{"a", "b"}, "open")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := IssueSnapshotDigest("1", "Title", "Body", []string{"b", "a"}, "open")
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("digest depends on label order: %q vs %q", d1, d2)
	}
	d3, err := IssueSnapshotDigest("1", "Title", "Body", []string{"a"}, "open")
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d3 {
		t.Fatalf("digest did not change when labels changed")
	}
}

func TestHasExistingBatchMarker(t *testing.T) {
	cases := []struct {
		name string
		body []string
		want bool
	}{
		{"none", []string{"just a comment", "another one"}, false},
		{"prepared", []string{"goobers-decomposition-prepared: parent=1 digest=sha256:abc children=a,b"}, true},
		{"published", []string{"goobers-decomposition-published: parent=1 digest=sha256:abc children=1,2"}, true},
		{"whitespace padded", []string{"  " + PublishedBatchMarkerPrefix + " parent=1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasExistingBatchMarker(tc.body); got != tc.want {
				t.Fatalf("HasExistingBatchMarker(%v) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
