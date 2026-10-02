package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

func TestDoctorRecordedFailureRecoveryAndStatus(t *testing.T) {
	withFakeDoctorCluster(t)
	root := t.TempDir()
	args := []string{"--k8s", "--checks", "apiserver-ipblock-drift", "--record-instance", root, "--result-max-age", "1h"}
	// A mismatched endpoint makes the real detector fail against the fake API.
	failed := append(append([]string{}, args...), "--apiserver-endpoint", "https://127.0.0.2")
	if code := runDoctor(failed, io.Discard, io.Discard); code != 1 {
		t.Fatalf("failed check exit = %d", code)
	}
	service, err := readservice.NewLocal(readservice.LocalSources{Layout: instance.NewLayout(root), Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}}}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	checkStatus := func(want string) {
		t.Helper()
		status, err := service.SchedulerStatus(context.Background())
		if err != nil || len(status.ClusterChecks) != 1 || status.ClusterChecks[0].State != want {
			t.Fatalf("status = %+v, %v; want %s", status, err, want)
		}
		var text strings.Builder
		renderSchedulerStatusSignals(&text, status, time.Now())
		if !strings.Contains(text.String(), "Cluster check apiserver-ipblock-drift: "+want) {
			t.Fatalf("text = %s", text.String())
		}
		data, err := json.Marshal(statusJSONOutput{ClusterChecks: status.ClusterChecks})
		if err != nil || !strings.Contains(string(data), `"clusterChecks":[`) || !strings.Contains(string(data), `"expiresAt":`) {
			t.Fatalf("JSON = %s, %v", data, err)
		}
	}
	checkStatus("degraded")
	if code := runDoctor(args, io.Discard, io.Discard); code != 0 {
		t.Fatalf("recovered check exit = %d", code)
	}
	checkStatus("healthy")
}

func TestDoctorRecordsClientFailureWithoutDiagnostics(t *testing.T) {
	orig := doctorKubeClient
	doctorKubeClient = func(string, string, time.Duration) (kubernetes.Interface, string, error) {
		return nil, "", errors.New("SECRET client setup error")
	}
	t.Cleanup(func() { doctorKubeClient = orig })
	root := t.TempDir()
	code := runDoctor([]string{"--k8s", "--checks", "overlay-pin-agreement,apiserver-ipblock-drift", "--record-instance", root}, io.Discard, io.Discard)
	if code != 2 {
		t.Fatalf("exit = %d", code)
	}
	events, err := journal.ReadInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil || len(events) != 2 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	for _, event := range events {
		if event.Runner["outcome"] != "fail" {
			t.Fatalf("event = %+v", event)
		}
		data, err := json.Marshal(event)
		if err != nil || strings.Contains(string(data), "SECRET") {
			t.Fatalf("unexpected diagnostic in %s: %v", data, err)
		}
	}
}

func TestDoctorRecordFlagValidationAndWriteFailure(t *testing.T) {
	for _, args := range [][]string{
		{"--repo", "--record-instance", "root"},
		{"--k8s", "--result-max-age", "1h"},
		{"--k8s", "--record-instance", "root", "--result-max-age", "0s"},
		{"--k8s", "--record-instance", ""},
	} {
		if code := runDoctor(args, io.Discard, io.Discard); code != 2 {
			t.Fatalf("%v exit = %d", args, code)
		}
	}
	withFakeDoctorCluster(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "scheduler"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if code := runDoctor([]string{"--k8s", "--checks", "apiserver-ipblock-drift", "--record-instance", root}, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "record check") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}
