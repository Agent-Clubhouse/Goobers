package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporalcodec"
	"github.com/goobers/goobers/internal/temporaldial"
)

func configureTestTemporalCodec(t *testing.T, root string) *instance.Config {
	t.Helper()
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	keys := t.TempDir()
	if err := os.Chmod(keys, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(keys, "history"), 0700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "history", "v1.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.SecretStores = append(cfg.SecretStores, instance.SecretStoreConfig{Name: "codec-keys", Kind: instance.SecretStoreKindFileKey, Directory: keys})
	cfg.Temporal = &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{KeyRef: &instance.KeyRef{Store: "codec-keys", Name: "history", Version: "v1"}}}
	cfg.Engine = &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "codec-test"}
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func assertSealedConverter(t *testing.T, dc converter.DataConverter) {
	t.Helper()
	if dc == nil {
		t.Fatal("codec converter missing")
	}
	p, err := dc.ToPayload("private-input")
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Metadata["encoding"]) != temporalcodec.Encoding {
		t.Fatal("configured client would write plaintext")
	}
}
func TestTemporalCodecDaemonAndWorkerWiring(t *testing.T) {
	root := initDemo(t)
	cfg := configureTestTemporalCodec(t, root)
	previous := dialDaemonEngine
	t.Cleanup(func() { dialDaemonEngine = previous })
	var captured converter.DataConverter
	dialDaemonEngine = func(_, _ string, _ *temporaldial.TLS, dc ...converter.DataConverter) (client.Client, error) {
		if len(dc) != 1 {
			t.Fatal("missing daemon converter")
		}
		captured = dc[0]
		return nil, nil
	}
	daemon, err := newDaemonEngineClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertSealedConverter(t, captured)
	if daemon.dataConverter != captured {
		t.Fatal("memo readers use different converter")
	}
	got := captureWorkerHost(t)
	var stdout, stderr bytes.Buffer
	code := runWorker([]string{"--instance", root, "--work-root", filepath.Join(t.TempDir(), "work"), "--blob-store", filepath.Join(t.TempDir(), "blobs"), "--config-reload-interval", "0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("worker=%d: %s", code, stderr.String())
	}
	assertSealedConverter(t, got.DataConverter)
}
func TestTemporalCodecProjectAndQueuesWiring(t *testing.T) {
	root := initDemo(t)
	configureTestTemporalCodec(t, root)
	project, queues := dialEngineProject, dialEngineQueues
	t.Cleanup(func() { dialEngineProject, dialEngineQueues = project, queues })
	calls := 0
	stop := errors.New("stop after options")
	dialEngineProject = func(_ context.Context, opts client.Options) (engineProjectClient, error) {
		calls++
		assertSealedConverter(t, opts.DataConverter)
		return nil, stop
	}
	dialEngineQueues = func(_ context.Context, opts client.Options) (engineQueuesClient, error) {
		calls++
		assertSealedConverter(t, opts.DataConverter)
		return nil, stop
	}
	var stdout, stderr bytes.Buffer
	if code := runEngineProject([]string{"--gaggle", "demo", "run-id", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("project=%d: %s", code, stderr.String())
	}
	if code := runEngineQueues([]string{root}, &stdout, &stderr); code != 1 {
		t.Fatalf("queues=%d: %s", code, stderr.String())
	}
	if calls != 2 {
		t.Fatalf("dialed %d commands: %s", calls, stderr.String())
	}
}
func TestDoctorTemporalCodecAndServerRefusal(t *testing.T) {
	root := initDemo(t)
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--temporal-codec", "--report", "json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor=%d: %s", code, stderr.String())
	}
	var status struct {
		Enabled, Strict bool
		Verification    string
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.Strict || !strings.Contains(status.Verification, "configuration-only") {
		t.Fatal("incorrect disabled report")
	}
	configureTestTemporalCodec(t, root)
	stdout.Reset()
	if code := runDoctor([]string{"--temporal-codec", "--report", "json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor=%d: %s", code, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.Strict {
		t.Fatal("incorrect enabled report")
	}
	stderr.Reset()
	if code := runTemporalCodecServer([]string{root}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "api.auth.oidc") {
		t.Fatalf("server accepted local anonymous configuration: %d %s", code, stderr.String())
	}
}

func TestTemporalCodecOrphanSweepsKeepConverter(t *testing.T) {
	root := initDemo(t)
	cfg := configureTestTemporalCodec(t, root)
	dc, err := temporalcodec.DataConverter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	previous := dialWorkerSweepTemporal
	t.Cleanup(func() { dialWorkerSweepTemporal = previous })
	seen := make(chan converter.DataConverter, 2)
	dialWorkerSweepTemporal = func(_, _ string, _ *temporaldial.TLS, values ...converter.DataConverter) (client.Client, error) {
		var got converter.DataConverter
		if len(values) == 1 {
			got = values[0]
		}
		select {
		case seen <- got:
		default:
		}
		return nil, errors.New("stop after dial options")
	}
	sweeper := &recordingSweeper{}
	sweepWorkerStageOrphans(sweeper, "host", "namespace", nil, io.Discard, io.Discard, dc)
	if got := <-seen; got != dc {
		t.Fatal("boot sweep lost converter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startPeriodicWorkerStageOrphanSweeps(ctx, sweeper, "host", "namespace", nil, io.Discard, io.Discard, time.Millisecond, dc)
	select {
	case got := <-seen:
		if got != dc {
			t.Error("periodic sweep lost converter")
		}
	case <-time.After(time.Second):
		t.Error("periodic sweep did not dial")
	}
	cancel()
	<-done
}
