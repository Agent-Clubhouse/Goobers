package memstat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCgroupFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The v2 numbers here are the ones measured on the OOMKilled goobers-api pod in
// #3949: a 10Gi limit 76% consumed, of which 573Mi is anonymous and 5.8Gi is
// page cache. The whole point of the reading is that those two terms are
// reported separately, so the case that motivated the package is the case the
// test asserts.
func TestReadCgroupV2SplitsAnonFromPageCache(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.current": "8217665536\n",
		"memory.max":     "10737418240\n",
		"memory.stat":    "anon 610402304\nfile 6251020288\nslab_reclaimable 228764880\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v2 reading")
	}
	if cgroup.Current != 8217665536 || cgroup.Limit != 10737418240 {
		t.Fatalf("current/limit = %d/%d, want 8217665536/10737418240", cgroup.Current, cgroup.Limit)
	}
	if cgroup.Anon != 610402304 || cgroup.File != 6251020288 {
		t.Fatalf("anon/file = %d/%d, want 610402304/6251020288", cgroup.Anon, cgroup.File)
	}
	fraction, ok := cgroup.UsedFraction()
	if !ok || fraction < 0.76 || fraction > 0.77 {
		t.Fatalf("UsedFraction = %v (ok=%v), want ~0.765", fraction, ok)
	}
}

func TestReadCgroupV2TreatsMaxAsUnlimited(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.current": "4096\n",
		"memory.max":     "max\n",
		"memory.stat":    "anon 1024\nfile 3072\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v2 reading")
	}
	if cgroup.Limit != 0 {
		t.Fatalf("Limit = %d, want 0 for an unlimited cgroup", cgroup.Limit)
	}
	// A zero Limit must never reach a division. UsedFraction reporting not-ok
	// is what keeps callers off that path.
	if fraction, ok := cgroup.UsedFraction(); ok {
		t.Fatalf("UsedFraction = %v (ok=true), want not-ok when unlimited", fraction)
	}
}

func TestReadCgroupV1MapsRSSAndCacheOntoAnonAndFile(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, filepath.Join(root, "memory"), map[string]string{
		"memory.usage_in_bytes": "2000\n",
		"memory.limit_in_bytes": "8000\n",
		"memory.stat":           "cache 1500\nrss 500\nrss_huge 0\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v1 reading")
	}
	if cgroup.Current != 2000 || cgroup.Limit != 8000 {
		t.Fatalf("current/limit = %d/%d, want 2000/8000", cgroup.Current, cgroup.Limit)
	}
	if cgroup.Anon != 500 || cgroup.File != 1500 {
		t.Fatalf("anon/file = %d/%d, want v1 rss/cache mapped to 500/1500", cgroup.Anon, cgroup.File)
	}
}

// A runtime may bind-mount the v1 memory controller onto the cgroup root rather
// than into a "memory" subdirectory. Both layouts must read.
func TestReadCgroupV1AtRootWithoutMemorySubdirectory(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.usage_in_bytes": "2000\n",
		"memory.limit_in_bytes": "8000\n",
		"memory.stat":           "cache 1500\nrss 500\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v1 reading from the cgroup root")
	}
	if cgroup.Anon != 500 || cgroup.File != 1500 {
		t.Fatalf("anon/file = %d/%d, want 500/1500", cgroup.Anon, cgroup.File)
	}
}

// v1 encodes "unlimited" as the largest page-aligned value that fits, which
// varies with page size. Every such variant must classify as unlimited rather
// than as a limit the reading would then report a meaningless percentage of.
func TestReadCgroupV1TreatsSentinelLimitsAsUnlimited(t *testing.T) {
	for _, sentinel := range []string{
		"9223372036854771712", // 4 KiB pages
		"9223372036854710784", // 64 KiB pages
		"9223372036854775807", // max int64
	} {
		root := t.TempDir()
		writeCgroupFiles(t, filepath.Join(root, "memory"), map[string]string{
			"memory.usage_in_bytes": "2000\n",
			"memory.limit_in_bytes": sentinel + "\n",
			"memory.stat":           "cache 1500\nrss 500\n",
		})

		cgroup := readCgroup(root)
		if cgroup == nil {
			t.Fatalf("sentinel %s: readCgroup = nil, want a v1 reading", sentinel)
		}
		if cgroup.Limit != 0 {
			t.Fatalf("sentinel %s: Limit = %d, want 0", sentinel, cgroup.Limit)
		}
	}
}

// v2 takes precedence: a host exposing both layouts must not be read as v1,
// whose "rss" excludes terms v2's "anon" includes.
func TestReadCgroupPrefersV2WhenBothLayoutsExist(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.current": "111\n",
		"memory.max":     "999\n",
		"memory.stat":    "anon 11\nfile 100\n",
	})
	writeCgroupFiles(t, filepath.Join(root, "memory"), map[string]string{
		"memory.usage_in_bytes": "222\n",
		"memory.limit_in_bytes": "888\n",
		"memory.stat":           "cache 200\nrss 22\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil || cgroup.Current != 111 {
		t.Fatalf("readCgroup = %+v, want the v2 reading (current 111)", cgroup)
	}
}

func TestReadCgroupReportsNilWhenUnavailable(t *testing.T) {
	for name, root := range map[string]string{
		"empty path":     "",
		"missing dir":    filepath.Join(t.TempDir(), "absent"),
		"no memory file": t.TempDir(),
	} {
		if cgroup := readCgroup(root); cgroup != nil {
			t.Fatalf("%s: readCgroup = %+v, want nil", name, cgroup)
		}
	}
}

// A malformed line must cost only itself. memory.stat gains keys across kernel
// versions, and one unparseable line must not discard the keys alongside it.
func TestReadCgroupSkipsMalformedStatLines(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.current": "50\n",
		"memory.max":     "100\n",
		"memory.stat":    "anon 30\ngarbage\nfile notanumber\nslab 5\nfile 20\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a reading despite malformed lines")
	}
	if cgroup.Anon != 30 || cgroup.File != 20 {
		t.Fatalf("anon/file = %d/%d, want 30/20", cgroup.Anon, cgroup.File)
	}
}

// Read must produce a usable reading on any platform, including one with no
// cgroup at all — a diagnostic that only works in production cannot be trusted
// in production.
func TestReadAlwaysReportsRuntimeTermsWithoutACgroup(t *testing.T) {
	footprint := read(filepath.Join(t.TempDir(), "absent"))
	if footprint.Cgroup != nil {
		t.Fatalf("Cgroup = %+v, want nil without a cgroup", footprint.Cgroup)
	}
	if footprint.HeapBytes == 0 || footprint.RetainedBytes == 0 || footprint.Goroutines == 0 {
		t.Fatalf("footprint = %+v, want non-zero runtime terms", footprint)
	}
	if footprint.RetainedBytes < footprint.HeapBytes {
		t.Fatalf("retained %d < heap %d, want retained to bound heap",
			footprint.RetainedBytes, footprint.HeapBytes)
	}
	rendered := footprint.String()
	for _, want := range []string{"heap ", "retained ", "goroutine(s)"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("String() = %q, want it to contain %q", rendered, want)
		}
	}
	if strings.Contains(rendered, "cgroup") {
		t.Fatalf("String() = %q, want no cgroup clause when none was read", rendered)
	}
}

func TestFootprintStringCarriesTheAnonCacheSplit(t *testing.T) {
	footprint := Footprint{
		HeapBytes:     42 * 1024 * 1024,
		RetainedBytes: 96 * 1024 * 1024,
		Goroutines:    128,
		Cgroup: &Cgroup{
			Current:      8217665536,
			Limit:        10737418240,
			Anon:         610402304,
			File:         6251020288,
			AtLimitKnown: true,
		},
	}

	got := footprint.String()
	want := "heap 42Mi, retained 96Mi, 128 goroutine(s), cgroup 7.7Gi/10Gi (77%) = anon 582Mi + cache 5.8Gi"
	if got != want {
		t.Fatalf("String() =\n  %q\nwant\n  %q", got, want)
	}
}

func TestFootprintStringMarksAnUnlimitedCgroup(t *testing.T) {
	footprint := Footprint{Cgroup: &Cgroup{Current: 1024 * 1024}}
	if got := footprint.String(); !strings.Contains(got, "cgroup 1.0Mi/unlimited") {
		t.Fatalf("String() = %q, want an unlimited cgroup clause", got)
	}
}

func TestFormatBytesKeepsSmallClimbsVisible(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{999, "999B"},
		{1024, "1.0Ki"},
		{10 * 1024, "10Ki"},
		{1023 * 1024, "1023Ki"},
		{1024 * 1024, "1.0Mi"},
		{610402304, "582Mi"},
		{6251020288, "5.8Gi"},
		{10737418240, "10Gi"},
		{1 << 50, "1.0Pi"},
		{1 << 60, "1024Pi"},
	} {
		if got := FormatBytes(tc.in); got != tc.want {
			t.Fatalf("FormatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUsedFractionOnNilCgroup(t *testing.T) {
	var cgroup *Cgroup
	if fraction, ok := cgroup.UsedFraction(); ok {
		t.Fatalf("UsedFraction = %v (ok=true), want not-ok on a nil cgroup", fraction)
	}
}

// The counters below use the values measured on the #3949 pod, where
// memory.events reported 6198 reclaim-at-limit episodes while a point-in-time
// reading of memory.current looked unremarkable. That gap is the reason these
// fields exist, so the fixture keeps it.
func TestReadCgroupV2CarriesTheAtLimitAndOOMKillCounters(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.current": "8083791872\n",
		"memory.max":     "10737418240\n",
		"memory.stat":    "anon 245760000\nfile 7490000000\n",
		"memory.events":  "low 0\nhigh 0\nmax 6198\noom 12\noom_kill 3\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v2 reading")
	}
	if cgroup.AtLimit != 6198 {
		t.Fatalf("AtLimit = %d, want 6198", cgroup.AtLimit)
	}
	if !cgroup.AtLimitKnown {
		t.Fatal("AtLimitKnown = false, want true when memory.events was read")
	}
	if cgroup.OOMKills != 3 {
		t.Fatalf("OOMKills = %d, want 3", cgroup.OOMKills)
	}
}

func TestReadCgroupV2WithoutEventsFileReportsZeroCounters(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, root, map[string]string{
		"memory.current": "1024\n",
		"memory.max":     "4096\n",
		"memory.stat":    "anon 512\nfile 512\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v2 reading")
	}
	// An unreadable counter must not masquerade as a calm one: the value is
	// zero either way, so AtLimitKnown is the only thing that separates them,
	// and the memory gate refuses to arm without it.
	if cgroup.AtLimitKnown {
		t.Fatal("AtLimitKnown = true, want false when memory.events is absent")
	}
	if cgroup.AtLimit != 0 || cgroup.OOMKills != 0 {
		t.Fatalf("at-limit/oom-kills = %d/%d, want 0/0 without memory.events", cgroup.AtLimit, cgroup.OOMKills)
	}
}

// v1 has no "max" equivalent, so a v1 reading must still produce the kill
// count it does export rather than losing both counters together.
func TestReadCgroupV1ReadsOOMKillsFromOOMControl(t *testing.T) {
	root := t.TempDir()
	writeCgroupFiles(t, filepath.Join(root, "memory"), map[string]string{
		"memory.usage_in_bytes": "2048\n",
		"memory.limit_in_bytes": "8192\n",
		"memory.stat":           "rss 1024\ncache 1024\n",
		"memory.oom_control":    "oom_kill_disable 0\nunder_oom 0\noom_kill 7\n",
	})

	cgroup := readCgroup(root)
	if cgroup == nil {
		t.Fatal("readCgroup = nil, want a v1 reading")
	}
	if cgroup.OOMKills != 7 {
		t.Fatalf("OOMKills = %d, want 7", cgroup.OOMKills)
	}
	if cgroup.AtLimit != 0 || cgroup.AtLimitKnown {
		t.Fatalf("AtLimit = %d (known %t), want 0/false (v1 exports no equivalent)", cgroup.AtLimit, cgroup.AtLimitKnown)
	}
}

func TestFootprintStringAppendsPressureCountersOnlyWhenNonZero(t *testing.T) {
	base := func() Footprint {
		return Footprint{Cgroup: &Cgroup{Current: 8217665536, Limit: 10737418240, Anon: 610402304, File: 6251020288, AtLimitKnown: true}}
	}

	quiet := base().String()
	if strings.Contains(quiet, "at-limit") || strings.Contains(quiet, "oom-kill") {
		t.Fatalf("String() = %q, want no pressure clause on a quiet cgroup", quiet)
	}

	pressured := base()
	pressured.Cgroup.AtLimit = 6198
	pressured.Cgroup.OOMKills = 3
	got := pressured.String()
	if !strings.Contains(got, "6198 at-limit") || !strings.Contains(got, "3 oom-kill(s)") {
		t.Fatalf("String() = %q, want both pressure counters", got)
	}
}

func TestCgroupBreakdownOnNilCgroup(t *testing.T) {
	var cgroup *Cgroup
	if got := cgroup.Breakdown(); got != "" {
		t.Fatalf("Breakdown() = %q, want empty for a nil cgroup", got)
	}
}

// A cgroup with a limit but no readable at-limit counter leaves the memory
// gate unable to ever fire. That is a safety property an operator has to be
// able to SEE, so the heartbeat says so rather than printing a zero that is
// indistinguishable from a healthy, idle cgroup.
func TestFootprintStringFlagsAnUnavailableAtLimitCounter(t *testing.T) {
	unavailable := Footprint{Cgroup: &Cgroup{Current: 1024, Limit: 4096, Anon: 512, File: 512}}.String()
	if !strings.Contains(unavailable, "at-limit counter unavailable") {
		t.Fatalf("String() = %q, want it to flag the unavailable counter", unavailable)
	}

	// Known-and-zero is a real reading, not a gap, so it stays quiet.
	known := Footprint{Cgroup: &Cgroup{Current: 1024, Limit: 4096, AtLimitKnown: true}}.String()
	if strings.Contains(known, "unavailable") {
		t.Fatalf("String() = %q, want no warning when the counter reads zero", known)
	}

	// Neither does an unlimited cgroup, where the gate is inert regardless.
	unlimited := Footprint{Cgroup: &Cgroup{Current: 1024}}.String()
	if strings.Contains(unlimited, "unavailable") {
		t.Fatalf("String() = %q, want no warning without a limit to be near", unlimited)
	}
}
