package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestRootsDiscoverReportsIdentityAndFreshness(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	if err := os.WriteFile(layout.ReadDB(), []byte("not a live daemon"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "roots", "discover", "--json", root)
	if code != 0 {
		t.Fatalf("discover: %d %s", code, stderr)
	}
	var got rootsDiscoveryOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.Partial || len(got.Roots) != 1 {
		t.Fatalf("discovery: %+v", got)
	}
	item := got.Roots[0]
	if item.ID == "" || item.Path != canonicalStatusRoot(root) || item.ReadModelModifiedAt == nil || item.OwningPID != 0 || item.DaemonState != "not-running" {
		t.Fatalf("misleading discovery: %+v", item)
	}
	code, stdout, stderr = runArgs(t, "roots", "discover", root)
	if code != 0 || !strings.Contains(stdout, "not daemon liveness") {
		t.Fatalf("text: %d %s %s", code, stdout, stderr)
	}
}

func TestRootsDiscoverWarnsForTwoOwningDaemonsSharingGaggle(t *testing.T) {
	first, second := initScheduledDemo(t), initScheduledDemo(t)
	for _, root := range []string{first, second} {
		owner := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now().UTC(), InstanceRoot: root, Version: "test"}
		release, err := acquireInstanceLockWithIdentity(filepath.Join(instance.NewLayout(root).SchedulerDir(), "up.lock"), &owner)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	code, stdout, stderr := runArgs(t, "roots", "discover", "--json", first, second)
	if code != 0 {
		t.Fatalf("discover: %d %s %s", code, stdout, stderr)
	}
	var got rootsDiscoveryOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, "\n"), "owning daemons") {
		t.Fatalf("duplicate scope hidden: %+v", got)
	}
	if !strings.Contains(strings.Join(got.Warnings, "\n"), "Repository") {
		t.Fatalf("repository overlap hidden: %+v", got)
	}
}

func TestDiscoveryRepositoryKeysIncludeHostAndDeduplicate(t *testing.T) {
	first := apiv1.RepoRef{Provider: "gitea", BaseURL: "https://one.example", Owner: "team", Name: "repo"}
	second := first
	second.BaseURL = "https://two.example"
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{Spec: apiv1.GaggleSpec{Project: first, AdditionalRepos: []apiv1.RepoRef{first, second}}}}}
	keys := discoveryRepositoryKeys(set)
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("host identity collapsed or repeated repo counted twice: %v", keys)
	}
	roots := []discoveredRoot{
		{statusRootIdentity: statusRootIdentity{Path: "/one", OwningPID: 1}, Repositories: keys[:1]},
		{statusRootIdentity: statusRootIdentity{Path: "/two", OwningPID: 2}, Repositories: keys[1:]},
		{statusRootIdentity: statusRootIdentity{Path: "/stopped"}, Repositories: keys},
	}
	if warnings := duplicateRootWarnings(roots); len(warnings) != 0 {
		t.Fatalf("false active overlap: %v", warnings)
	}
}

func TestDuplicateRootWarningsExcludeStoppedScopeButDetectCopiedIdentity(t *testing.T) {
	roots := []discoveredRoot{
		{statusRootIdentity: statusRootIdentity{Path: "/one", ID: "copied", OwningPID: 12}, Gaggles: []string{"shared"}},
		{statusRootIdentity: statusRootIdentity{Path: "/two", ID: "copied"}, Gaggles: []string{"shared"}},
	}
	warnings := duplicateRootWarnings(roots)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "copied identity") {
		t.Fatalf("warnings: %v", warnings)
	}
}

func TestRootsGroupHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var stdout, stderr bytes.Buffer
		if code := runRoots([]string{arg}, &stdout, &stderr); code != 0 || stdout.String() != rootsHelp || stderr.Len() != 0 {
			t.Fatalf("help %q: code=%d stdout=%q stderr=%q", arg, code, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{nil, {"unknown"}} {
		var stdout, stderr bytes.Buffer
		if code := runRoots(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), rootsHelp) {
			t.Fatalf("invalid %v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRootsDecommissionPreservesDataAndWarnsInStatus(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	before, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "roots", "decommission", "--reason=migrated to new root", root)
	if code != 0 || !strings.Contains(stdout, "No data was deleted") {
		t.Fatalf("decommission: %d %s %s", code, stdout, stderr)
	}
	first, err := os.ReadFile(filepath.Join(root, instance.RootDecommissionFileName))
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runArgs(t, "roots", "decommission", "--reason=retry", root)
	if code != 0 {
		t.Fatal(stderr)
	}
	again, err := os.ReadFile(filepath.Join(root, instance.RootDecommissionFileName))
	if err != nil || string(again) != string(first) {
		t.Fatalf("retry replaced marker: %q %v", again, err)
	}
	after, err := os.ReadFile(layout.ConfigFile())
	if err != nil || string(before) != string(after) {
		t.Fatal("decommission modified config")
	}
	code, stdout, stderr = runArgs(t, "status", root)
	if code != 0 || !strings.Contains(stdout, "Historical root; do not use") || !strings.Contains(stdout, "migrated to new root") {
		t.Fatalf("historical root hidden: %d %s %s", code, stdout, stderr)
	}
	code, _, stderr = runArgs(t, "up", root)
	if code != 2 || !strings.Contains(stderr, "historical root; do not use") {
		t.Fatalf("historical startup allowed: %d %s", code, stderr)
	}
	owner := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now(), InstanceRoot: root, Version: "test"}
	if release, err := acquireInstanceLockWithIdentity(filepath.Join(layout.SchedulerDir(), "up.lock"), &owner); err == nil {
		release()
		t.Fatal("daemon acquisition bypassed historical-root guard")
	}
}

func TestRootsDecommissionRefusesLiveDaemon(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	owner := daemonIdentity{PID: os.Getpid(), StartedAt: time.Now(), InstanceRoot: root, Version: "test"}
	release, err := acquireInstanceLockWithIdentity(filepath.Join(layout.SchedulerDir(), "up.lock"), &owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	code, _, stderr := runArgs(t, "roots", "decommission", "--reason=migration", root)
	if code != 2 || !strings.Contains(stderr, "root must be stopped") {
		t.Fatalf("live root decommissioned: %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, instance.RootDecommissionFileName)); !os.IsNotExist(err) {
		t.Fatalf("marker exists: %v", err)
	}
}

func TestRootsDecommissionDisplayFailurePreventsMutation(t *testing.T) {
	root := initScheduledDemo(t)
	layout := instance.NewLayout(root)
	if err := os.RemoveAll(layout.SchedulerDir()); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := runRootsDecommission([]string{"--reason=migration", root}, brokenRootBannerWriter{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "display instance identity") {
		t.Fatalf("display failure ignored: %d %s", code, stderr.String())
	}
	for _, path := range []string{layout.SchedulerDir(), filepath.Join(root, instance.RootDecommissionFileName)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("mutation before successful display at %s: %v", path, err)
		}
	}
}
