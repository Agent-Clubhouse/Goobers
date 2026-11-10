package journal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBoundedEventParserStopsAtCountAndUnknownSchema(t *testing.T) {
	line := `{"schema":"` + EventSchema + `","seq":1}` + "\n"
	records, _, err := parseEventRecordsContext(context.Background(), []byte(strings.Repeat(line, 10000)), 3)
	if !errors.Is(err, ErrReadSnapshotLimit) || len(records) != 0 {
		t.Fatalf("%d %v", len(records), err)
	}
	records, _, err = parseEventRecordsContext(context.Background(), []byte(strings.Repeat("{}\n", 10000)), 3)
	if err == nil || errors.Is(err, ErrReadSnapshotLimit) || len(records) != 0 {
		t.Fatalf("schema validation must precede retention: %d %v", len(records), err)
	}
	records, torn, err := parseEventRecordsContext(context.Background(), []byte(line+"{torn"), 3)
	if err != nil || len(records) != 1 || torn != 5 {
		t.Fatalf("committed tail semantics changed: %d %d %v", len(records), torn, err)
	}
}
