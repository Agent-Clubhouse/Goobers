package cgroupfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadUintRejectsEverythingThatIsNotOneInteger(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name   string
		body   string
		want   uint64
		wantOK bool
	}{
		{name: "plain integer", body: "10737418240\n", want: 10737418240, wantOK: true},
		{name: "no trailing newline", body: "300000", want: 300000, wantOK: true},
		{name: "surrounding whitespace", body: "  4096  \n", want: 4096, wantOK: true},
		// The two sentinels the cgroup generations use for "no limit". Reading
		// either as a number would produce a limit no caller could act on.
		{name: "cgroup v2 max sentinel", body: "max\n"},
		{name: "cgroup v1 unlimited sentinel", body: "-1\n"},
		{name: "empty", body: "\n"},
		{name: "not a number", body: "unavailable\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "value")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := ReadUint(path)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("ReadUint(%q) = %d, %v; want %d, %v", tc.body, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A missing file is the ordinary case off a container, not an error condition.
func TestReadUintReportsNotOKForAMissingFile(t *testing.T) {
	if got, ok := ReadUint(filepath.Join(t.TempDir(), "absent")); ok {
		t.Fatalf("ReadUint(absent) = %d, true; want not-ok", got)
	}
}

// Both generations grow keys in their stat files across kernel versions, so an
// unrecognized or malformed line must cost the caller nothing but that line.
func TestReadKeyedSkipsUnparseableLinesWithoutLosingTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpu.stat")
	body := "usage_usec 9550117762\nnr_periods 33103\nmalformed\nnr_throttled 26328\nnr_bursts not_a_number\nthrottled_usec 2111356121\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	values := ReadKeyed(path)
	for key, want := range map[string]uint64{
		"usage_usec":     9550117762,
		"nr_periods":     33103,
		"nr_throttled":   26328,
		"throttled_usec": 2111356121,
	} {
		if got, ok := values[key]; !ok || got != want {
			t.Fatalf("ReadKeyed[%q] = %d, %v; want %d, true", key, got, ok, want)
		}
	}
	for _, absent := range []string{"malformed", "nr_bursts"} {
		if _, ok := values[absent]; ok {
			t.Fatalf("ReadKeyed kept the unparseable key %q", absent)
		}
	}
}

func TestReadKeyedIsEmptyForAMissingFile(t *testing.T) {
	if values := ReadKeyed(filepath.Join(t.TempDir(), "absent")); len(values) != 0 {
		t.Fatalf("ReadKeyed(absent) = %v, want empty", values)
	}
}
