package scheduleexpr

import "testing"

func TestParseDefinition(t *testing.T) {
	for _, expr := range []string{
		"*/5 * * * *",
		"0 */5 * * * *",
		"0 9 * JAN MON-FRI",
		"@daily",
		"@every 90m",
	} {
		if _, err := ParseDefinition(expr); err != nil {
			t.Errorf("ParseDefinition(%q): %v", expr, err)
		}
	}

	for _, expr := range []string{
		"not a cron",
		"@sometimes",
		"@every tomorrow",
		"0 0 30 2 *",
		"CRON_TZ=UTC 0 9 * * *",
	} {
		if _, err := ParseDefinition(expr); err == nil {
			t.Errorf("ParseDefinition(%q) unexpectedly succeeded", expr)
		}
	}
}

func TestParseRuntimeRejectsSecondsField(t *testing.T) {
	if _, err := ParseRuntime("0 30 2 * * *"); err == nil {
		t.Fatal("ParseRuntime accepted a six-field schedule")
	}
	if _, err := ParseRuntime("30 2 * * *"); err != nil {
		t.Fatalf("ParseRuntime rejected a five-field schedule: %v", err)
	}
}
