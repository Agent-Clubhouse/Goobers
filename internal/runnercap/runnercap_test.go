package runnercap

import (
	"reflect"
	"testing"
)

func TestValidToken(t *testing.T) {
	valid := []string{
		"dotnet",
		"dotnet@8",
		"dotnet@8.0",
		"netfx@4.8",
		"xcode",
		"os=windows",
		"x86_64",
		"build-tools",
		"c++",
		"node@20",
		"A0",
	}
	for _, s := range valid {
		if !ValidToken(s) {
			t.Errorf("ValidToken(%q) = false, want true", s)
		}
		if err := ValidateToken(s); err != nil {
			t.Errorf("ValidateToken(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		" dotnet",
		"dotnet ",
		"dot net",
		"@8",          // must start alphanumeric
		"=windows",    // must start alphanumeric
		"-tools",      // must start alphanumeric
		"dotnet\t8",   // whitespace
		"os=win/dows", // slash not allowed
		"emoji😀",      // non-ascii
		"a b",         // internal space
	}
	for _, s := range invalid {
		if ValidToken(s) {
			t.Errorf("ValidToken(%q) = true, want false", s)
		}
		if err := ValidateToken(s); err == nil {
			t.Errorf("ValidateToken(%q) = nil, want error", s)
		}
	}
}

func TestClaimedHas(t *testing.T) {
	c := NewClaimed([]string{"dotnet@8", "xcode", "dotnet@8"}) // duplicate collapses
	if !c.Has("dotnet@8") {
		t.Error("Has(dotnet@8) = false, want true")
	}
	if !c.Has("xcode") {
		t.Error("Has(xcode) = false, want true")
	}
	// Exact match only — a different version is a different capability.
	if c.Has("dotnet@10") {
		t.Error("Has(dotnet@10) = true, want false (exact string match, no ranges)")
	}
	if c.Has("") {
		t.Error("Has(\"\") = true, want false")
	}
}

func TestClaimedMissing(t *testing.T) {
	claimed := NewClaimed([]string{"dotnet@8", "xcode"})

	tests := []struct {
		name     string
		required []string
		want     []string
	}{
		{"none required is satisfied", nil, nil},
		{"empty required is satisfied", []string{}, nil},
		{"all met", []string{"dotnet@8", "xcode"}, nil},
		{"one missing", []string{"dotnet@10"}, []string{"dotnet@10"}},
		{"mixed met and missing", []string{"dotnet@8", "dotnet@10", "os=windows"}, []string{"dotnet@10", "os=windows"}},
		{"duplicates reported once, in first-seen order", []string{"os=windows", "os=windows", "dotnet@10"}, []string{"os=windows", "dotnet@10"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := claimed.Missing(tc.required)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Missing(%v) = %v, want %v", tc.required, got, tc.want)
			}
		})
	}
}

func TestEmptyClaimedMissesEverythingRequired(t *testing.T) {
	var empty Claimed // zero value claims nothing
	if got := empty.Missing(nil); got != nil {
		t.Errorf("empty.Missing(nil) = %v, want nil", got)
	}
	if got := empty.Missing([]string{"dotnet@8"}); !reflect.DeepEqual(got, []string{"dotnet@8"}) {
		t.Errorf("empty.Missing([dotnet@8]) = %v, want [dotnet@8]", got)
	}
}
