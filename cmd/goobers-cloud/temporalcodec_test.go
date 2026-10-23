package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
func assertCodecProtectsPayload(t *testing.T, dc converter.DataConverter) {
	t.Helper()
	if dc == nil {
		t.Fatal("codec converter missing")
	}
	p, err := dc.ToPayloads("private-input")
	// The file-key backend requires POSIX private modes. Windows cannot
	// represent those modes; the installed codec must refuse, never fall back
	// to the SDK's plaintext converter. POSIX still proves actual encryption.
	if runtime.GOOS == "windows" {
		if !errors.Is(err, temporalcodec.ErrInvalidPayload) || len(p.GetPayloads()) != 0 {
			t.Fatalf("file-key codec did not fail closed on Windows: payload=%v err=%v", p, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(p.GetPayloads()) != 1 || string(p.Payloads[0].Metadata["encoding"]) != temporalcodec.Encoding {
		t.Fatal("configured client would write plaintext")
	}
}
func TestTemporalCodecWorkerWiring(t *testing.T) {
	root := initDemo(t)
	cfg := configureTestTemporalCodec(t, root)
	got := captureWorkerHost(t)
	var stdout, stderr bytes.Buffer
	code := runWorker([]string{"--instance", root, "--work-root", filepath.Join(t.TempDir(), "work"), "--blob-store", filepath.Join(t.TempDir(), "blobs"), "--config-reload-interval", "0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("worker=%d: %s", code, stderr.String())
	}
	assertCodecProtectsPayload(t, got.DataConverter)
	// Exercise the refusal assertion on POSIX too: losing private key-directory
	// permissions must abort encoding without returning any sendable payloads.
	for _, store := range cfg.SecretStores {
		if store.Name == "codec-keys" {
			if err := os.Chmod(store.Directory, 0777); err != nil {
				t.Fatal(err)
			}
		}
	}
	payloads, err := got.DataConverter.ToPayloads("private-input")
	if !errors.Is(err, temporalcodec.ErrInvalidPayload) || len(payloads.GetPayloads()) != 0 {
		t.Fatalf("unsafe key permissions did not refuse encoding: payloads=%v err=%v", payloads, err)
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
