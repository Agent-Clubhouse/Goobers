package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeGit answers ls-tree for each ref from a fixed listing.
func fakeGit(trees map[string][]string) gitFunc {
	return func(args ...string) ([]byte, error) {
		if len(args) < 4 || args[0] != "ls-tree" {
			return nil, errors.New("unexpected git call")
		}
		ref := strings.TrimSuffix(args[3], "^{commit}")
		files, ok := trees[ref]
		if !ok {
			return nil, errors.New("unknown revision " + ref)
		}
		var out []string
		for _, name := range files {
			out = append(out, migrationsDir+"/"+name)
		}
		return []byte(strings.Join(out, "\n") + "\n"), nil
	}
}

func TestAddedMigrationsMustBeNumberedAboveTheBase(t *testing.T) {
	base := []string{"0001_init.expand.sql", "0025_recorded_at.expand.sql", "0030_projector.expand.sql"}
	for name, tc := range map[string]struct {
		head []string
		code int
		want string
	}{
		"above the base":        {head: append(append([]string(nil), base...), "0035_custody.expand.sql"), code: 0},
		"filling a gap below":   {head: append(append([]string(nil), base...), "0024_custody.expand.sql"), code: 1, want: "0024_custody.expand.sql is numbered at or below 0030"},
		"equal to the base max": {head: append(append([]string(nil), base...), "0030_other.expand.sql"), code: 1, want: "0030_other.expand.sql"},
		"nothing added":         {head: base, code: 0},
		"stale branch, base moved past it": {
			head: []string{"0001_init.expand.sql", "0025_recorded_at.expand.sql", "0031_mine.expand.sql"},
			code: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{"-base-ref", "base", "-head-ref", "head"}, fakeGit(map[string][]string{"base": base, "head": tc.head}), &stdout, &stderr)
			if code != tc.code || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("code=%d stderr=%q, want code %d mentioning %q", code, stderr.String(), tc.code, tc.want)
			}
		})
	}
}

// TestStaleBranchFailsOnceItsBaseGainsAHigherMigration: comparing against the
// base, not the merge-base, is what makes update-branch catch a stale number.
func TestStaleBranchFailsOnceItsBaseGainsAHigherMigration(t *testing.T) {
	head := []string{"0001_init.expand.sql", "0031_mine.expand.sql"}
	trees := map[string][]string{
		"old-base": {"0001_init.expand.sql"},
		"new-base": {"0001_init.expand.sql", "0031_theirs.expand.sql", "0032_theirs.expand.sql"},
		"head":     head,
	}
	var out bytes.Buffer
	if code := run([]string{"-base-ref", "old-base", "-head-ref", "head"}, fakeGit(trees), &out, &out); code != 0 {
		t.Fatalf("against its original base: code=%d %s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"-base-ref", "new-base", "-head-ref", "head"}, fakeGit(trees), &out, &out); code != 1 || !strings.Contains(out.String(), "renumber it above 0032") {
		t.Fatalf("after the base moved: code=%d %s", code, out.String())
	}
}

func TestMigrationOrderRefusesBadInput(t *testing.T) {
	var out bytes.Buffer
	if code := run(nil, fakeGit(nil), &out, &out); code != 2 {
		t.Fatalf("no base ref: code=%d", code)
	}
	trees := map[string][]string{"base": {"0001_init.expand.sql"}, "head": {"0001_init.expand.sql", "35_bad-name.sql"}}
	if code := run([]string{"-base-ref", "base", "-head-ref", "head"}, fakeGit(trees), &out, &out); code != 2 {
		t.Fatalf("misnamed migration: code=%d", code)
	}
	if code := run([]string{"-base-ref", "missing", "-head-ref", "head"}, fakeGit(trees), &out, &out); code != 2 {
		t.Fatalf("unknown base: code=%d", code)
	}
}
