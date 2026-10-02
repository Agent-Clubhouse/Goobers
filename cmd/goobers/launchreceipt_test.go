package main

import (
	"bytes"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/podauth"
)

func TestDaemonLaunchReceiptWiringPersistsOutsideRunMounts(t *testing.T) {
	root := t.TempDir()
	key, err := podauth.NewSignedKey(bytes.Repeat([]byte{7}, podauth.MinSignedKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	opts, err := appendLaunchReceiptHandlerOption(nil, root, key)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(key, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	opts = append(opts, httpapi.WithAuthenticator(auth))
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	r := launchreceipt.Receipt{Version: 1, Binding: launchreceipt.Binding{RunID: "run-1", Stage: "build", Number: 1, StartedSeq: 1, AttemptID: journal.StageAttemptID("run-1", 0, "build", 1)}, Facts: launchreceipt.RemoteFacts{Source: "control-plane-prepared", ImageReferenceDigest: launchreceipt.Digest(nil), SelectorDigest: launchreceipt.Digest(nil), ContainerCount: 1, Seccomp: "unknown", NetworkEnforcement: "unknown", SandboxEnforcement: "unknown", ResolvedModel: "unknown", ResolvedEffort: "unknown"}}
	client := launchreceipt.Client{BaseURL: server.URL, Minter: key}
	if err := client.Record(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "runtime-launch-receipts", r.Binding.AttemptID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "runs")); !os.IsNotExist(err) {
		t.Fatal("receipt entered model-accessible run data")
	}
	if err := client.Record(t.Context(), r); err == nil {
		t.Fatal("duplicate accepted")
	}
}
