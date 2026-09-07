package packaging

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docs/guides/supervision.md restates the packaged units' shutdown deadlines in
// prose an operator copies into their own unit. When the systemd unit moved to
// an indefinite drain, the guide kept recommending TimeoutStopSec=45, so
// copying it could SIGKILL an in-flight agentic run (#4519). Pin the restated
// values to the shipped templates so prose and package cannot diverge again.
func TestSupervisionGuideRestatesPackagedStopTimeouts(t *testing.T) {
	t.Parallel()

	guideRaw, err := os.ReadFile(filepath.Join("..", "docs", "guides", "supervision.md"))
	if err != nil {
		t.Fatalf("read supervision guide: %v", err)
	}
	guide := string(guideRaw)

	for _, tc := range []struct {
		asset   string
		setting *regexp.Regexp
		render  func(value string) string
	}{
		{
			asset:   "systemd/goobers.service",
			setting: regexp.MustCompile(`(?m)^TimeoutStopSec=(\S+)\s*$`),
			render:  func(value string) string { return "`TimeoutStopSec=" + value + "`" },
		},
		{
			asset:   "launchd/com.agent-clubhouse.goobers.plist",
			setting: regexp.MustCompile(`(?s)<key>ExitTimeOut</key>\s*<integer>(\d+)</integer>`),
			render:  func(value string) string { return "`ExitTimeOut=" + value + "`" },
		},
	} {
		raw, err := ServiceFiles.ReadFile(tc.asset)
		if err != nil {
			t.Fatalf("read embedded %s: %v", tc.asset, err)
		}
		match := tc.setting.FindStringSubmatch(string(raw))
		if match == nil {
			t.Fatalf("%s has no stop-timeout setting matching %s", tc.asset, tc.setting)
		}
		want := tc.render(match[1])
		if !strings.Contains(guide, want) {
			t.Errorf("supervision.md does not restate %s's shipped value; want a mention of %s", tc.asset, want)
		}
	}
}

// The guide must not tell an operator the drain is bounded by a fixed grace
// period. `goobers up --drain-timeout` defaults to 0 (wait indefinitely), and
// the removed `drainGrace` constant is the exact phrasing that went stale.
func TestSupervisionGuideDoesNotCiteRemovedDrainGrace(t *testing.T) {
	t.Parallel()

	guideRaw, err := os.ReadFile(filepath.Join("..", "docs", "guides", "supervision.md"))
	if err != nil {
		t.Fatalf("read supervision guide: %v", err)
	}
	if strings.Contains(string(guideRaw), "drainGrace") {
		t.Error("supervision.md cites drainGrace, which no longer exists; the drain is unbounded unless --drain-timeout is set")
	}
}
