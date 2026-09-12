package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestConfigMirrorRealDaemonPublishesOnlyAcceptedReloads(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	setAPIListenAddress(t, root, freeLoopbackAddress(t))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	mirror := t.TempDir()
	config.ConfigMirrorPath = mirror
	document, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ConfigFile(), document, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan struct{})
	code := -1
	go func() {
		code = runUpContext(ctx, []string{"--quiet", root}, started, &stderr)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if code != 0 {
				t.Errorf("daemon exit=%d: %s", code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-started.started:
	case <-done:
		t.Fatalf("daemon failed to start: %s", stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("daemon startup timed out")
	}
	initial := waitForConfigValue(t, "initial daemon config mirror", func() (string, bool) {
		body, err := mirroredManifest(mirror)
		return body, err == nil
	})
	updated := strings.Replace(initial, "name: example", "name: mirror-reloaded", 1)
	if updated == initial {
		t.Fatal("manifest fixture name missing")
	}
	manifest := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	if err := os.WriteFile(manifest, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloaded, 1)
	waitForConfigValue(t, "accepted reload mirror", func() (bool, bool) {
		body, err := mirroredManifest(mirror)
		return true, err == nil && body == updated
	})
	if err := os.WriteFile(manifest, []byte("not: [valid yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForConfigEvent(t, layout.SchedulerDir(), journal.EventConfigReloadRejected, 1)
	if body, err := mirroredManifest(mirror); err != nil || body != updated {
		t.Fatalf("rejected source replaced accepted mirror: %q %v", body, err)
	}
}

func mirroredManifest(directory string) (string, error) {
	archive, err := zip.OpenReader(filepath.Join(directory, configmirror.SnapshotName))
	if err != nil {
		return "", err
	}
	defer func() { _ = archive.Close() }()
	for _, entry := range archive.File {
		if entry.Name != "config/manifest.yaml" {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return "", err
		}
		defer func() { _ = reader.Close() }()
		data, err := io.ReadAll(io.LimitReader(reader, 1<<20))
		return string(data), err
	}
	return "", fmt.Errorf("mirror has no manifest")
}
