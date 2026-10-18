package recovery

import (
	"maps"
	"slices"
	"testing"
)

func TestParseSnapshotTrackedEntriesSelectsHiddenEditsAndSkipsSparse(t *testing.T) {
	tracked, updates, err := parseSnapshotTrackedEntries([]byte("H clean.ps1\x00h assumed.txt\x00S sparse.txt\x00s sparse-assumed.txt\x00M conflict.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(tracked), "clean.ps1\x00assumed.txt\x00conflict.txt\x00"; got != want {
		t.Fatalf("tracked = %q, want %q", got, want)
	}
	if got := slices.Sorted(maps.Keys(updates)); !slices.Equal(got, []string{"assumed.txt"}) {
		t.Fatalf("updates = %q, want only the assume-unchanged path", got)
	}
	for _, bad := range []string{"H clean.ps1", "Hclean.ps1\x00", "H\x00"} {
		if _, _, err := parseSnapshotTrackedEntries([]byte(bad)); err == nil {
			t.Fatalf("malformed listing %q accepted", bad)
		}
	}
}

func TestParseSnapshotFilterAttributesSelectsDriverPaths(t *testing.T) {
	updates := map[string]bool{}
	output := "clean.ps1\x00filter\x00unspecified\x00off.bin\x00filter\x00unset\x00bare.bin\x00filter\x00set\x00a.lfs\x00filter\x00lfs\x00b.lfs\x00filter\x00lfs\x00c.enc\x00filter\x00crypt\x00"
	drivers, err := parseSnapshotFilterAttributes([]byte(output), updates)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(drivers, []string{"lfs", "crypt"}) {
		t.Fatalf("drivers = %q", drivers)
	}
	if got := slices.Sorted(maps.Keys(updates)); !slices.Equal(got, []string{"a.lfs", "b.lfs", "c.enc"}) {
		t.Fatalf("updates = %q, want only filtered paths", got)
	}
	for _, bad := range []string{"a.lfs\x00filter\x00lfs", "a.lfs\x00filter\x00"} {
		if _, err := parseSnapshotFilterAttributes([]byte(bad), map[string]bool{}); err == nil {
			t.Fatalf("malformed attributes %q accepted", bad)
		}
	}
}

func TestParseSnapshotWorktreeChangesIgnoresStagedOnlyPaths(t *testing.T) {
	updates := map[string]bool{}
	status := "M  staged.ps1\x00 M edited.ps1\x00 D removed.ps1\x00 T link\x00 A intent.txt\x00UU conflict.txt\x00MM both.ps1\x00"
	if err := parseSnapshotWorktreeChanges([]byte(status), updates); err != nil {
		t.Fatal(err)
	}
	want := []string{"both.ps1", "conflict.txt", "edited.ps1", "intent.txt", "link", "removed.ps1"}
	if got := slices.Sorted(maps.Keys(updates)); !slices.Equal(got, want) {
		t.Fatalf("updates = %q, want %q", got, want)
	}
	for _, bad := range []string{" M edited.ps1", " Medited.ps1\x00", "M\x00"} {
		if err := parseSnapshotWorktreeChanges([]byte(bad), map[string]bool{}); err == nil {
			t.Fatalf("malformed status %q accepted", bad)
		}
	}
}
