package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

func TestStatusDaemonReportsLiveDroppedAppends(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	t.Setenv("GOOBERS_API_TOKEN", "status-token")
	root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != apicontract.InstancePath {
			t.Errorf("request = %s %s, want GET %s", request.Method, request.URL.Path, apicontract.InstancePath)
			http.NotFound(w, request)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer status-token" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(readservice.Instance{
			RootIdentity:  &readservice.RootIdentity{ID: identity},
			JournalHealth: &readservice.JournalHealthStatus{AppendsDropped: 7},
		})
	})
	writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), identity+"\n")
	layout := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 0 || stderr != "" {
		t.Fatalf("status --daemon: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "instance journal dropped 7 best-effort append(s) in this daemon process") {
		t.Fatalf("status --daemon output = %q", stdout)
	}
}

func TestStatusDaemonReportsLiveStorageHealth(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	t.Setenv("GOOBERS_API_TOKEN", "status-token")
	root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != apicontract.InstancePath {
			t.Errorf("request = %s %s, want GET %s", request.Method, request.URL.Path, apicontract.InstancePath)
			http.NotFound(w, request)
			return
		}
		_ = json.NewEncoder(w).Encode(readservice.Instance{
			RootIdentity: &readservice.RootIdentity{ID: identity},
			StorageHealth: &readservice.StorageHealthStatus{
				Tier: "admission-stopped", FreeBytes: 1 << 30, TotalBytes: 100 << 30, Path: "/instances/fixture",
			},
		})
	})
	writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), identity+"\n")
	layout := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 0 || stderr != "" {
		t.Fatalf("status --daemon: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "storage health admission stopped") {
		t.Fatalf("status --daemon output = %q, want a storage health line", stdout)
	}
}

func TestStatusDaemonOmitsHealthyStorageHealth(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	root, _ := interventionCLIFixture(t, func(w http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(w).Encode(readservice.Instance{
			RootIdentity:  &readservice.RootIdentity{ID: identity},
			StorageHealth: &readservice.StorageHealthStatus{Tier: "healthy", FreeBytes: 90 << 30, TotalBytes: 100 << 30},
		})
	})
	writeFileContent(t, filepath.Join(root, instance.RootIdentityFileName), identity+"\n")
	layout := instance.NewLayout(root)
	release, err := acquireDaemonLock(filepath.Join(layout.SchedulerDir(), "up.lock"), root, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	code, stdout, stderr := runArgs(t, "status", "--daemon", root)
	if code != 0 || stderr != "" {
		t.Fatalf("status --daemon: code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	if strings.Contains(stdout, "storage health") {
		t.Fatalf("status --daemon output = %q, want no storage health line while healthy", stdout)
	}
}
