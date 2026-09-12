package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitRefusesHistoricalRootBeforeReplacingMissingConfig(t *testing.T) {
	for name, initialize := range map[string]func(string) (*InitResult, error){
		"starter":    func(root string) (*InitResult, error) { return Init(root) },
		"demo":       func(root string) (*InitResult, error) { return InitDemo(root) },
		"quickstart": InitQuickstart,
	} {
		t.Run(name, func(t *testing.T) {
			root, marker := historicalRootFixture(t)
			configPath := NewLayout(root).ConfigFile()
			if err := os.Remove(configPath); err != nil {
				t.Fatal(err)
			}
			if _, err := initialize(root); !errors.Is(err, ErrHistoricalRoot) {
				t.Fatalf("historical root reinitialized: %v", err)
			}
			if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("init wrote config before refusal: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(root, RootDecommissionFileName))
			if err != nil || string(after) != string(marker) {
				t.Fatalf("historical marker changed: %v", err)
			}
		})
	}
}

func historicalRootFixture(t *testing.T) (string, []byte) {
	t.Helper()
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	if _, err := DecommissionRoot(context.Background(), root, "migrated", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, RootDecommissionFileName))
	if err != nil {
		t.Fatal(err)
	}
	return root, data
}

func TestHistoricalRootCannotBeReadoptedWhenIdentityIsMissing(t *testing.T) {
	root, marker := historicalRootFixture(t)
	if err := os.Remove(filepath.Join(root, RootIdentityFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRootDecommission(root); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing marker became absent: %v", err)
	}
	if _, err := EnsureRootIdentity(context.Background(), root); err == nil {
		t.Fatal("historical root silently assigned new identity")
	}
	if _, err := os.Stat(filepath.Join(root, RootIdentityFileName)); !os.IsNotExist(err) {
		t.Fatalf("identity recreated: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, RootDecommissionFileName))
	if err != nil || string(after) != string(marker) {
		t.Fatal("historical marker changed")
	}
}

func TestRootDecommissionRejectsMalformedAndOversizedMarkers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(string) string
	}{
		{"version", func(s string) string { return strings.Replace(s, "v1", "v99", 1) }},
		{"extra record", func(s string) string { return s + "extra\n" }},
		{"oversized", func(string) string { return strings.Repeat("x", maxRootDecommissionBytes+1) }},
		{"foreign identity", func(s string) string {
			parts := strings.Split(s, "\n")
			parts[1] = strings.Repeat("0", 32)
			return strings.Join(parts, "\n")
		}},
		{"invalid time", func(s string) string { return strings.Replace(s, "2026-09-08T00:00:00Z", "not-a-time", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, data := historicalRootFixture(t)
			corrupt := tc.corrupt(string(data))
			path := filepath.Join(root, RootDecommissionFileName)
			if err := os.WriteFile(path, []byte(corrupt), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRootDecommission(root); err == nil {
				t.Fatal("invalid marker accepted")
			}
			if _, err := DecommissionRoot(context.Background(), root, "replace", time.Now()); err == nil {
				t.Fatal("invalid marker overwritten")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != corrupt {
				t.Fatal("marker was modified")
			}
		})
	}
}

func TestRootDecommissionCancellationAndReasonBound(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DecommissionRoot(ctx, root, "migration", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	for _, reason := range []string{"", "  ", strings.Repeat("x", 2049), strings.Repeat("\x00", 2048)} {
		if _, err := DecommissionRoot(context.Background(), root, reason, time.Now()); err == nil {
			t.Fatal("unbounded or blank reason accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(root, RootDecommissionFileName)); !os.IsNotExist(err) {
		t.Fatalf("failed request published marker: %v", err)
	}
}
