package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestTelemetryInstanceIdentities(t *testing.T) {
	journalID, rootID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	t.Run("explicit-root-required", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"instance-id", instance.RootIdentityFileName} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(journalID+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Chdir(root)
		for _, absent := range []string{"", " "} {
			if id, attrs := telemetryInstanceIdentities(absent); id != "" || len(attrs) != 0 {
				t.Fatal("missing root adopted ambient working-directory identity")
			}
		}
	})
	for _, tc := range []struct {
		name, journal, root   string
		wantJournal, wantRoot string
	}{
		{"distinct", journalID, rootID, journalID, rootID},
		{"missing", "", "", "", ""},
		{"journal-only", journalID, "", journalID, ""},
		{"root-only", "", rootID, "", rootID},
		{"invalid-journal", "broken", rootID, "", rootID},
		{"invalid-root", journalID, "broken", journalID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{"instance-id": tc.journal, instance.RootIdentityFileName: tc.root}
			count := 0
			for name, value := range files {
				if value != "" {
					count++
					if err := os.WriteFile(filepath.Join(root, name), []byte(value+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			id, attrs := telemetryInstanceIdentities(root)
			got := map[string]string{}
			for _, attr := range attrs {
				got[string(attr.Key)] = attr.Value.AsString()
			}
			if id != tc.wantJournal || got["goobers.instance.id"] != tc.wantJournal || got["goobers.root.id"] != tc.wantRoot {
				t.Fatalf("journal=%q resources=%v", id, got)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != count {
				t.Fatalf("identity observation created state: entries=%d error=%v", len(entries), err)
			}
			for name, value := range files {
				if value != "" {
					data, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(data) != value+"\n" {
						t.Fatalf("identity observation changed %s", name)
					}
				}
			}
		})
	}
}
