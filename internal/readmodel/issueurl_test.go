package readmodel

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestProjectionIssueURL(t *testing.T) {
	const source = "https://example.test/incidents/123"
	for _, tc := range []struct {
		name   string
		url    any
		status string
		want   string
	}{
		{"https", source, "success", source},
		{"http", "http://example.test/items/123", "success", "http://example.test/items/123"},
		{"missing", nil, "success", ""},
		{"non-string", 123, "success", ""},
		{"relative", "/items/123", "success", ""},
		{"protocol-relative", "//example.test/items/123", "success", ""},
		{"script", "javascript:alert(1)", "success", ""},
		{"credentials", "https://user:password@example.test/123", "success", ""},
		{"hostless", "https:///123", "success", ""},
		{"control", source + "\n", "success", ""},
		{"oversized", source + strings.Repeat("a", 2048), "success", ""},
		{"failed", source, "failure", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []journal.Event{
				displayTitleFinish(1, tc.status, map[string]any{"id": "123", "title": "Claimed item", "url": tc.url}),
				displayTitleFinish(2, "success", map[string]any{"id": "456", "title": "Unrelated item", "url": "https://example.test/456"}),
			}
			whole := ProjectRun(testIdentity(), Projection{}, events)
			if got := whole.Run.Operator.IssueURL; got != tc.want {
				t.Fatalf("URL = %q, want %q", got, tc.want)
			}
			for split := 0; split <= len(events); split++ {
				first := ProjectRun(testIdentity(), Projection{}, events[:split])
				incremental := ProjectRun(testIdentity(), first, events[split:])
				if !reflect.DeepEqual(incremental.Run.Operator, whole.Run.Operator) {
					t.Fatalf("split %d differs from full replay", split)
				}
			}
		})
	}
}
