package configtree

import (
	"path/filepath"
	"testing"
)

func TestIsGaggleSkillsDir(t *testing.T) {
	root := filepath.Join("repo", "config")
	tests := []struct {
		path string
		want bool
	}{
		{path: filepath.Join(root, "gaggles", "alpha", "skills"), want: true},
		{path: filepath.Join(root, "gaggles", "alpha", "skills", "testing")},
		{path: filepath.Join(root, "gaggles", "skills")},
		{path: filepath.Join(root, "skills")},
		{path: filepath.Join(root, "gaggles", "alpha", "workflows")},
	}
	for _, test := range tests {
		if got := IsGaggleSkillsDir(root, test.path); got != test.want {
			t.Errorf("IsGaggleSkillsDir(%q, %q) = %t, want %t", root, test.path, got, test.want)
		}
	}
}

func TestSkipDir(t *testing.T) {
	root := filepath.Join("repo", "config")
	tests := []struct {
		path string
		want bool
	}{
		{path: root, want: false},
		{path: filepath.Join(root, "gaggles", "alpha", "skills"), want: true},
		{path: filepath.Join(root, ".git"), want: true},
		{path: filepath.Join(root, "gaggles", "alpha", "workflows"), want: false},
	}
	for _, test := range tests {
		if got := SkipDir(root, test.path); got != test.want {
			t.Errorf("SkipDir(%q, %q) = %t, want %t", root, test.path, got, test.want)
		}
	}
}
