package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporaldial"
)

func TestRunWorkerEndpointDispatchReadsSurrenderWithoutMount(t *testing.T) {
	root := initDemo(t)
	declareDispatchRunner(t, root, "linux-pod")
	configureDispatchAuthority(t, root)
	server, _ := workerBlobDaemon(t, root, t.TempDir())
	t.Setenv("GOOBERS_BLOB_ENDPOINT", "")
	t.Setenv("GOOBERS_BLOB_STORE", "")
	oldKube, oldPreflight, oldDial := dispatchKubeClient, preflightGaggleNamespaces, dialWorkerSweepTemporal
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	preflightGaggleNamespaces = fakeClusterHasNoRealRBACSoSkipPreflight
	dialWorkerSweepTemporal = func(string, string, *temporaldial.TLS) (client.Client, error) { return nil, context.DeadlineExceeded }
	t.Cleanup(func() {
		dispatchKubeClient, preflightGaggleNamespaces, dialWorkerSweepTemporal = oldKube, oldPreflight, oldDial
	})
	got := captureWorkerHost(t)
	var out, errs bytes.Buffer
	code := runWorker([]string{"--instance", root, "--work-root", t.TempDir(), "--blob-endpoint", server.URL, "--daemon-api", server.URL, "--dispatch-namespace", "stages", "--config-reload-interval", "0"}, &out, &errs)
	if code != 0 {
		t.Fatalf("startup %d: %s", code, &errs)
	}
	plane, ok := got.Deps.Surrenders.(*dispatcher.SurrenderReadClient)
	if !ok {
		t.Fatalf("surrender transport %T", got.Deps.Surrenders)
	}
	ctx := context.Background()
	if seen, err := plane.Has(ctx, "run-1", "stage-1", 1); err != nil || seen {
		t.Fatalf("missing Seen=%v %v", seen, err)
	}
	if _, err := plane.Get(ctx, "run-1", "stage-1", 1); !errors.Is(err, dispatcher.ErrNoSurrender) {
		t.Fatalf("missing Get=%v", err)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := podTokenMinter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Mint("run-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pod := &dispatcher.SurrenderPutClient{BaseURL: server.URL, Token: token}
	payload := []byte(`{"result":{"status":"success"},"recoveryAcknowledged":true}`)
	if err := pod.Put(ctx, "run-1", "stage-1", 1, payload); err != nil {
		t.Fatal(err)
	}
	if seen, err := plane.Has(ctx, "run-1", "stage-1", 1); err != nil || !seen {
		t.Fatalf("written Seen=%v %v", seen, err)
	}
	if data, err := plane.Get(ctx, "run-1", "stage-1", 1); err != nil || !bytes.Equal(data, payload) {
		t.Fatalf("written Get=%q %v", data, err)
	}
	if err := plane.Put(ctx, "run-1", "stage-1", 2, payload); err == nil {
		t.Fatal("worker reader allowed surrender writes")
	}
}
