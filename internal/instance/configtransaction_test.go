package instance

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigTransactionCrashHelper(t *testing.T) {
	root := os.Getenv("GOOBERS_CONFIG_CRASH_ROOT")
	if root == "" {
		return
	}
	boundary := func(name string) {
		if name == os.Getenv("GOOBERS_CONFIG_CRASH_AT") {
			os.Exit(73)
		}
	}
	layout := NewLayout(root)
	var err error
	if os.Getenv("GOOBERS_CONFIG_CRASH_MODE") == "recover" {
		err = recoverConfigTransaction(layout, boundary)
	} else {
		stagedInstance := ""
		if os.Getenv("GOOBERS_CONFIG_CRASH_INSTANCE") == "yes" {
			stagedInstance = filepath.Join(root, "candidate", ConfigFileName)
		}
		var swap *PreparedConfigSwap
		swap, err = prepareConfigTransaction(layout, filepath.Join(root, "candidate", ConfigDirName), stagedInstance, boundary)
		if err == nil {
			if os.Getenv("GOOBERS_CONFIG_CRASH_MODE") == "commit" {
				err = swap.Commit()
			} else {
				err = swap.Rollback()
			}
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash boundary was not reached")
}

func crashConfigProcess(t *testing.T, root, mode, point string, withInstance bool) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestConfigTransactionCrashHelper$")
	instance := "no"
	if withInstance {
		instance = "yes"
	}
	command.Env = append(os.Environ(), "GOOBERS_CONFIG_CRASH_ROOT="+root, "GOOBERS_CONFIG_CRASH_MODE="+mode, "GOOBERS_CONFIG_CRASH_AT="+point, "GOOBERS_CONFIG_CRASH_INSTANCE="+instance)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("crash %s/%s: %v\n%s", mode, point, err, output)
	}
}

func configCrashFixture(t *testing.T) Layout {
	t.Helper()
	layout := NewLayout(t.TempDir())
	for _, generation := range []string{"old", "new"} {
		root := layout.Root
		if generation == "new" {
			root = filepath.Join(root, "candidate")
		}
		if err := os.MkdirAll(filepath.Join(root, ConfigDirName, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{ConfigFileName, filepath.Join(ConfigDirName, "nested", "workflow.yaml")} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(generation), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return layout
}

func assertConfigGeneration(t *testing.T, layout Layout, generation string, withInstance bool) {
	t.Helper()
	for _, name := range []string{ConfigFileName, filepath.Join(ConfigDirName, "nested", "workflow.yaml")} {
		want := generation
		if name == ConfigFileName && !withInstance {
			want = "old"
		}
		got, err := os.ReadFile(filepath.Join(layout.Root, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %s", name, got, err, want)
		}
	}
}

func TestConfigTransactionCrashBoundaries(t *testing.T) {
	for _, withInstance := range []bool{false, true} {
		names := []string{ConfigDirName}
		if withInstance {
			names = append(names, ConfigFileName)
		}
		for _, mode := range []string{"commit", "rollback"} {
			points := []string{"snapshots", "prepared", "retired", "cleaned"}
			if mode == "commit" {
				points = append(points, "committed")
			}
			for _, name := range names {
				for _, step := range []string{"staged", "removed", "installed"} {
					points = append(points, "new-"+step+"-"+name)
					if mode == "rollback" {
						points = append(points, "old-"+step+"-"+name)
					}
				}
			}
			for _, point := range points {
				t.Run(mode+"/"+strings.Join(names, "+")+"/"+point, func(t *testing.T) {
					layout := configCrashFixture(t)
					crashConfigProcess(t, layout.Root, mode, point, withInstance)
					// The caller is allowed to discard staging immediately after prepare.
					if err := os.RemoveAll(filepath.Join(layout.Root, "candidate")); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if err := RecoverConfigTransaction(layout); err != nil {
							t.Fatal(err)
						}
					}
					want := "old"
					if mode == "commit" && (point == "committed" || point == "retired" || point == "cleaned") {
						want = "new"
					}
					assertConfigGeneration(t, layout, want, withInstance)
				})
			}
		}
	}
}

func TestConfigTransactionRecoveryCanCrashAgain(t *testing.T) {
	for _, name := range []string{ConfigFileName, ConfigDirName} {
		for _, step := range []string{"staged", "removed", "installed"} {
			t.Run(step+"/"+name, func(t *testing.T) {
				layout := configCrashFixture(t)
				crashConfigProcess(t, layout.Root, "commit", "new-removed-"+ConfigDirName, true)
				crashConfigProcess(t, layout.Root, "recover", "old-"+step+"-"+name, true)
				if err := RecoverConfigTransaction(layout); err != nil {
					t.Fatal(err)
				}
				assertConfigGeneration(t, layout, "old", true)
			})
		}
	}
}

func TestConfigTransactionRejectsUnsafeRecovery(t *testing.T) {
	for _, kind := range []string{"malformed", "symlink", "unknown", "historical"} {
		t.Run(kind, func(t *testing.T) {
			layout := configCrashFixture(t)
			crashConfigProcess(t, layout.Root, "commit", "prepared", true)
			dir := filepath.Join(layout.Root, configTransactionName)
			switch kind {
			case "malformed":
				if err := os.WriteFile(filepath.Join(dir, "intent"), []byte(`{"schema":99}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if err := os.WriteFile(filepath.Join(dir, "unexpected"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "old", ConfigDirName, "escape")); err != nil {
					t.Skip(err)
				}
			case "historical":
				if _, err := EnsureRootIdentity(context.Background(), layout.Root); err != nil {
					t.Fatal(err)
				}
				if _, err := DecommissionRoot(context.Background(), layout.Root, "retired", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := RecoverConfigTransaction(layout); err == nil {
				t.Fatal("unsafe transaction recovered")
			}
			assertConfigGeneration(t, layout, "old", true)
		})
	}
}

func TestPreparedConfigTransactionExcludesRecovery(t *testing.T) {
	layout := configCrashFixture(t)
	swap, err := PrepareConfigDirSwap(layout, filepath.Join(layout.Root, "candidate", ConfigDirName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = swap.Rollback() }()
	if err := RecoverConfigTransaction(layout); err == nil {
		t.Fatal("recovery stole a pending transaction")
	}
	if _, err := PrepareConfigDirSwap(layout, filepath.Join(layout.Root, "candidate", ConfigDirName)); err == nil {
		t.Fatal("second writer stole a pending transaction")
	}
	if err := os.RemoveAll(filepath.Join(layout.Root, "candidate")); err != nil {
		t.Fatal(err)
	}
	if err := swap.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertConfigGeneration(t, layout, "old", false)
}
