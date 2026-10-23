package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
)

func TestFreshWorkerRestoresAdmittedGenerationFromConfigPlane(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	assetPath := filepath.Join(layout.ConfigDir(), "gaggles", pinGaggle, "goobers", pinGoober, "assets", "identity.txt")
	writeFixture(t, assetPath, "admitted-asset")
	admittedInstructions := readFileContent(t, gooberInstructionsPath(root, pinGaggle, pinGoober))
	pin := currentPin(t, workerReloadSeams(t, root))

	ownerID, err := layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	archive, generation, err := configgeneration.CaptureForInstance(t.Context(), layout.ConfigDir(), ownerID)
	if err != nil {
		t.Fatal(err)
	}
	archives, err := blobstore.NewDir(filepath.Join(layout.BlobStoreDir(), "config-generations"))
	if err != nil {
		t.Fatal(err)
	}
	if err := archives.Put(t.Context(), generation, archive); err != nil {
		t.Fatal(err)
	}

	owner, err := layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	shared, err := blobstore.NewDir(layout.BlobStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	// The exact store fronted by the artifact HTTP plane cannot retrieve a full
	// archive, even if a stage pod learns the archive's content address.
	if _, err := shared.Get(t.Context(), generation); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("full fleet configuration exposed through artifact store: %v", err)
	}
	workerRoot := initDemo(t)
	if err := os.Remove(filepath.Join(workerRoot, instance.RootIdentityFileName)); err != nil {
		t.Fatal(err)
	}
	if err := instance.InstallRootIdentity(t.Context(), workerRoot, owner); err != nil {
		t.Fatal(err)
	}
	editInstructions(t, workerRoot, "different worker-mounted generation")
	writeFixture(t, filepath.Join(instance.NewLayout(workerRoot).ConfigDir(), "gaggles", pinGaggle, "goobers", pinGoober, "assets", "identity.txt"), "changed-asset")
	seams, err := newWorkerSeams(workerRoot, shared)
	if err != nil {
		t.Fatal(err)
	}
	if seams.snapshot.Load() != nil || len(seams.history) != 0 {
		t.Fatal("worker unexpectedly has admission history")
	}
	generationBlobs, err := blobstore.NewDir(filepath.Join(layout.BlobStoreDir(), "config-generations"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		digest := request.URL.Query().Get("digest")
		data, err := generationBlobs.Get(request.Context(), digest)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		w.Header().Set(apicontract.DigestHeader, digest)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	client := configmirror.Client{BaseURL: server.URL, HTTP: server.Client()}
	seams.configPlane = &client
	env := apiv1.InvocationEnvelope{RunID: "recoverable-run", TaskID: "recoverable-run:implement", WorkflowID: pinWorkflow, Gaggle: pinGaggle, Goober: pinGoober, GooberDigest: pin, InstanceID: owner, ConfigGeneration: generation}
	writer := agenticKitWriter{instanceRoot: workerRoot, seams: seams}
	kit, err := writer.buildKitContext(t.Context(), env, "invoke")
	if err != nil {
		t.Fatal(err)
	}
	if kit.Instructions[pinGoober] != admittedInstructions {
		t.Fatal("fresh worker kit used mounted instructions")
	}
	found := false
	for _, entry := range kit.Assets[pinGoober].Entries {
		if entry.Path == "identity.txt" {
			found = true
			if string(entry.Data) != "admitted-asset" {
				t.Fatalf("kit substituted asset %q", entry.Data)
			}
		}
	}
	if !found {
		t.Fatal("kit omitted admitted asset")
	}
	for _, agentic := range []bool{false, true} {
		restored, release, err := seams.forInvocationGaggle(t.Context(), env, agentic)
		if err != nil {
			t.Fatal(err)
		}
		if restored.cfg.ConfigGeneration != generation {
			t.Fatal("worker executor did not carry admitted generation")
		}
		release()
	}
	env.ConfigGeneration = "sha256:" + strings.Repeat("f", 64)
	if _, err := writer.buildKitContext(t.Context(), env, "invoke"); err == nil {
		t.Fatal("missing archive fell back to mounted generation")
	}
	env.ConfigGeneration, env.InstanceID = generation, strings.Repeat("0", 32)
	if _, err := writer.buildKitContext(t.Context(), env, "invoke"); err == nil {
		t.Fatal("cross-instance generation was accepted")
	}
}
