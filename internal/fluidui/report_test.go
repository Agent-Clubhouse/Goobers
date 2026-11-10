package fluidui

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func learningStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	store := ForInstance(t.TempDir())
	store.Now = func() time.Time { return c.now }
	return store, c
}

func compact(hidden ...string) Customization {
	doc := Customization{Version: Version, Density: DensityCompact}
	if len(hidden) > 0 {
		doc.Pages = map[string]PageLayout{"overview": {Hidden: hidden}}
	}
	return doc
}

func failingFirst() Customization {
	return Customization{Version: Version, RunList: &RunList{Sort: SortFailingFirst}}
}

func candidate(t *testing.T, report Report, shape string) Candidate {
	t.Helper()
	for _, c := range report.Candidates {
		if strings.Join(c.Shape, ",") == shape {
			return c
		}
	}
	t.Fatalf("shape %q not in report %+v", shape, report.Candidates)
	return Candidate{}
}

func TestReportCountsOneUsersRevisionsOnce(t *testing.T) {
	store, c := learningStore(t)
	// One enthusiastic user revises the same shape many times.
	for i := range 10 {
		mustPut(t, store, "issuer:alice", compact("activity", []string{"warnings", "summary"}[i%2]))
		c.advance(time.Hour)
	}
	mustPut(t, store, "issuer:bob", failingFirst())
	mustPut(t, store, "issuer:carol", failingFirst())
	c.advance(8 * 24 * time.Hour)

	report, err := store.Report()
	if err != nil {
		t.Fatal(err)
	}
	busy := candidate(t, report, "density:compact,overview.hidden")
	if busy.Users != 1 || busy.SustainedUsers != 1 {
		t.Fatalf("revisions inflated the shape: %+v", busy)
	}
	popular := candidate(t, report, "runs.sort:failing-first")
	if popular.Rank != 1 || popular.Users != 2 || popular.SustainedUsers != 2 {
		t.Fatalf("two distinct users should outrank one user's revisions: %+v", report.Candidates)
	}
	if report.Users != 3 {
		t.Fatalf("users = %d, want 3", report.Users)
	}
}

func TestReportRanksRevertedShapesBelowSustained(t *testing.T) {
	store, c := learningStore(t)
	mustPut(t, store, "issuer:c", failingFirst())
	for _, user := range []string{"issuer:a", "issuer:b", "issuer:c"} {
		mustPut(t, store, user, compact())
	}
	mustPut(t, store, "issuer:d", failingFirst())
	c.advance(time.Hour)
	if _, err := store.Reset("issuer:a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reset("issuer:b"); err != nil {
		t.Fatal(err)
	}
	// Undo to an earlier, different shape is also a reversion.
	if _, err := store.Undo("issuer:c"); err != nil {
		t.Fatal(err)
	}
	c.advance(8 * 24 * time.Hour)

	report, err := store.Report()
	if err != nil {
		t.Fatal(err)
	}
	reverted := candidate(t, report, "density:compact")
	sustained := candidate(t, report, "runs.sort:failing-first")
	if reverted.RevertedUsers != 3 || reverted.ReversionRate != 1 || reverted.ActiveUsers != 0 {
		t.Fatalf("reverted shape evidence = %+v", reverted)
	}
	if sustained.SustainedUsers != 2 || sustained.Users != 2 {
		t.Fatalf("sustained shape evidence = %+v", sustained)
	}
	if sustained.Rank >= reverted.Rank {
		t.Fatalf("sustained shape (rank %d) must outrank reverted shape (rank %d, 3 users)", sustained.Rank, reverted.Rank)
	}
}

func TestReportSustainedNeedsSevenUninterruptedDays(t *testing.T) {
	store, c := learningStore(t)
	mustPut(t, store, "issuer:a", compact())
	c.advance(6 * 24 * time.Hour)
	mustPut(t, store, "issuer:a", compact("activity")) // a new shape restarts the stint
	c.advance(2 * 24 * time.Hour)
	mustPut(t, store, "issuer:a", compact("summary")) // the same shape does not

	report, err := store.Report()
	if err != nil {
		t.Fatal(err)
	}
	if got := candidate(t, report, "density:compact"); got.ActiveUsers != 0 || got.SustainedUsers != 0 || got.RevertedUsers != 0 {
		t.Fatalf("revised-away shape = %+v", got)
	}
	if got := candidate(t, report, "density:compact,overview.hidden"); got.ActiveUsers != 1 || got.SustainedUsers != 0 {
		t.Fatalf("2-day-old shape = %+v", got)
	}
	c.advance(5 * 24 * time.Hour)
	report, err = store.Report()
	if err != nil {
		t.Fatal(err)
	}
	if got := candidate(t, report, "density:compact,overview.hidden"); got.SustainedUsers != 1 {
		t.Fatalf("7-day-old shape = %+v", got)
	}
}

func TestReportOmitsIdentityAndPromptText(t *testing.T) {
	store, _ := learningStore(t)
	secrets := []string{"https://login.example.test", "alice@example.test", "Secret Launch View", "#/runs?gaggle=skunkworks", "skunkworks", "bobs-panel", "bobs-page", "deploy-phase"}
	user := UserKey("https://login.example.test", "alice@example.test")
	mustPut(t, store, user, Customization{
		Version: Version,
		Pages:   map[string]PageLayout{"bobs-page": {Hidden: []string{"bobs-panel"}}},
		RunList: &RunList{Phases: []string{"deploy-phase"}},
		SavedViews: []SavedView{
			{Name: "Secret Launch View", Route: "#/runs?gaggle=skunkworks"},
		},
	})
	prompt := "please make the skunkworks dashboard purple for alice@example.test before launch"
	if err := store.RecordUnsupported(user, prompt); err == nil {
		t.Fatal("request text was accepted as an unmet-need label")
	}
	if err := store.RecordUnsupported(user, " Dark Mode "); err != nil {
		t.Fatal(err)
	}

	var jsonOut, textOut, stderr bytes.Buffer
	root := filepath.Dir(filepath.Dir(store.Dir))
	if code := RunReportCLI([]string{"--json", root}, flag.NewFlagSet("report", flag.ContinueOnError), &jsonOut, &stderr); code != 0 {
		t.Fatalf("--json exit %d: %s", code, stderr.String())
	}
	if code := RunReportCLI([]string{root}, flag.NewFlagSet("report", flag.ContinueOnError), &textOut, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report Report
	if err := json.Unmarshal(jsonOut.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(report.Candidates[0].Shape, ","); got != "other.hidden,runs.phase,savedViews" {
		t.Fatalf("shape = %q", got)
	}
	if len(report.UnmetNeeds) != 1 || report.UnmetNeeds[0] != (UnmetNeed{Label: "dark-mode", Users: 1}) {
		t.Fatalf("unmet needs = %+v", report.UnmetNeeds)
	}
	if !strings.Contains(textOut.String(), "never change global defaults") || !strings.Contains(textOut.String(), "other.hidden + runs.phase + savedViews") {
		t.Fatalf("human report:\n%s", textOut.String())
	}
	salt, err := os.ReadFile(filepath.Join(store.Dir, eventSaltName))
	if err != nil {
		t.Fatal(err)
	}
	events, err := readEvents(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(store.Dir, eventLogName))
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"json": jsonOut.String(), "text": textOut.String(), "log": string(log)} {
		for _, secret := range append(secrets, "purple", user) {
			if strings.Contains(output, secret) {
				t.Errorf("%s output contains %q", name, secret)
			}
		}
		if name != "log" && (strings.Contains(output, events[0].User) || strings.Contains(output, string(salt))) {
			t.Errorf("%s output exposes the pseudonymous token or salt", name)
		}
	}
}

func TestTransitionKinds(t *testing.T) {
	a, b := []string{"density:compact"}, []string{"savedViews"}
	cases := []struct {
		from, to []string
		revert   bool
		want     string
	}{
		{nil, nil, false, ""},
		{nil, a, false, eventApplied},
		{nil, a, true, eventApplied},
		{a, b, false, eventRevised},
		{a, a, true, eventRevised},
		{a, b, true, eventReverted},
		{a, nil, false, eventReverted},
	}
	for _, tc := range cases {
		if got := transitionKind(tc.from, tc.to, tc.revert); got != tc.want {
			t.Errorf("transitionKind(%v, %v, %v) = %q, want %q", tc.from, tc.to, tc.revert, got, tc.want)
		}
	}
}

func TestEventLogRotates(t *testing.T) {
	store, _ := learningStore(t)
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, eventLogName), bytes.Repeat([]byte("x"), maxEventLogBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUnsupported("issuer:a", "dark-mode"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Dir, eventLogName))
	if err != nil || info.Size() >= maxEventLogBytes {
		t.Fatalf("log not rotated: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, rotatedEventLogName)); err != nil {
		t.Fatal(err)
	}
}
