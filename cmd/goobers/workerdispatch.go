package main

// workerdispatch.go wires the #3513 dispatcher behind the engine's mode-3
// stage-dispatch seam (#3588): `goobers worker --dispatch-namespace <ns>`
// binds the per-(gaggle × runner-type) dispatch queues beside the workflow
// queue(s) and serves DispatchStage with a real dispatcher — pods created
// through the cluster credentials this process runs under (in-cluster
// ServiceAccount, or the standard kubeconfig loading rules outside a pod).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/version"
)

// stageDispatch is what buildStageDispatch wires: the dispatcher seam, the
// surrender plane, and the dispatch queues this worker must serve.
type stageDispatch struct {
	Dispatcher engine.StageDispatcher
	Surrenders dispatcher.SurrenderPlane
	Queues     []string
}

// dispatchKubeClient builds the typed clientset for pod creation: in-cluster
// config when running as a pod (the deployed dispatcher shape), else the
// standard kubeconfig loading rules. A seam so tests can substitute a fake.
var dispatchKubeClient = func() (kubernetes.Interface, error) {
	if restConfig, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(restConfig)
	}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubernetes config for stage dispatch: %w", err)
	}
	return kubernetes.NewForConfig(restConfig)
}

// buildStageDispatch loads the instance's runner inventory and constructs the
// dispatcher-backed seam. blobRoot is the worker's --blob-store directory;
// the surrender plane lives beside the content-addressed tree under
// <blobRoot>/surrender (identity-keyed, so it cannot ride the digest-verified
// store — see dispatcher/surrender.go), which keeps one operator-provided
// volume backing both planes.
func buildStageDispatch(instanceRoot, namespace, daemonAPI, blobRoot string) (stageDispatch, error) {
	if blobRoot == "" {
		return stageDispatch{}, fmt.Errorf("stage dispatch: a surrender plane is required — pass --blob-store")
	}
	l := instance.NewLayout(instanceRoot)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: load instance config: %w", err)
	}
	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		if issues := validationIssueSummary(report); issues != "" {
			return stageDispatch{}, fmt.Errorf("stage dispatch: load config directory: %w (%s)", err, issues)
		}
		return stageDispatch{}, fmt.Errorf("stage dispatch: load config directory: %w", err)
	}

	surrenders, err := dispatcher.NewSurrenderDir(filepath.Join(blobRoot, "surrender"))
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}

	specs := make([]dispatcher.RunnerSpec, 0, len(cfg.Runners))
	for _, entry := range cfg.ResolvedRunners() {
		spec, serr := dispatcher.SpecFromEntry(entry)
		if serr != nil {
			return stageDispatch{}, fmt.Errorf("stage dispatch: %w", serr)
		}
		specs = append(specs, spec)
	}
	gaggles := make([]string, 0, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggles = append(gaggles, set.Gaggles[i].Name)
	}
	queues := dispatcher.Queues(gaggles, specs)
	if len(queues) == 0 {
		return stageDispatch{}, fmt.Errorf("stage dispatch: the instance at %s declares no non-self runner; --dispatch-namespace has nothing to serve", instanceRoot)
	}

	client, err := dispatchKubeClient()
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}
	// The dispatcher runs HERE, in the worker — a different process from the
	// daemon that will receive the surrender. So the pod's bearer must be
	// verifiable without shared memory: a configured shared key gives
	// stateless signed tokens (Goobers#3701). Without one, PodToken stays
	// empty and the pod surrenders unauthenticated, which only works against
	// a null-auth loopback daemon in the same process.
	signed, kerr := podTokenMinter(cfg)
	if kerr != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", kerr)
	}
	// Assigned through an explicitly-typed nil interface rather than passing
	// `signed` straight in. A nil *SignedKey stored in a TokenMinter makes the
	// interface NON-nil, so the dispatcher's `TokenMinter != nil` guard would
	// pass and then call Mint on a nil pointer — the no-key posture would panic
	// instead of dispatching unauthenticated.
	var minter dispatcher.TokenMinter
	if signed != nil {
		minter = signed
	}

	build := version.Get()
	d, err := dispatcher.New(dispatcher.Config{
		TokenMinter: minter,
		// The kit writer needs the same signing key's peer facility — the blob
		// plane — plus the instance config only the worker has. Nil when no
		// blob endpoint is configured, which makes Dispatch refuse agentic
		// stages explicitly instead of creating a pod that would find no kit.
		KitWriter:       agenticKitWriterFor(instanceRoot, os.Getenv("GOOBERS_BLOB_ENDPOINT"), signed),
		Namespace:       namespace,
		EmbeddedCommit:  build.Commit,
		EmbeddedVersion: build.Version,
		BlobEndpoint:    os.Getenv("GOOBERS_BLOB_ENDPOINT"),
		WriteAPIBase:    daemonAPI,
	}, dispatcher.NewKubernetesPodAPI(client), nil, dispatcher.PlaneSurrenderGate{Plane: surrenders}, nil)
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}
	return stageDispatch{Dispatcher: d, Surrenders: surrenders, Queues: queues}, nil
}

// mergeQueues appends every dispatch queue not already served, preserving
// declaration order for the ones that are.
func mergeQueues(existing []string, dispatch []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, q := range existing {
		seen[q] = struct{}{}
	}
	merged := append([]string(nil), existing...)
	for _, q := range dispatch {
		if _, dup := seen[q]; dup {
			continue
		}
		seen[q] = struct{}{}
		merged = append(merged, q)
	}
	return merged
}

// podTokenMinter builds the shared-key minter from a loaded instance config, or
// returns nil when no key is configured (the loopback/no-auth posture).
//
// Factored out because TWO consumers in this process need the same key and must
// not drift: the dispatcher mints the bearer it stamps on a stage pod, and the
// live-journal emitter mints the bearer it presents to the daemon's journal
// plane. Both are "this worker proving which run it speaks for", and a second
// copy of this loading would be a second place for the key path to go stale.
func podTokenMinter(cfg *instance.Config) (*podauth.SignedKey, error) {
	if cfg == nil {
		return nil, nil
	}
	path := strings.TrimSpace(cfg.API.PodTokenKeyFile)
	if path == "" {
		return nil, nil
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pod token key %s: %w", path, err)
	}
	signed, err := podauth.NewSignedKey(bytes.TrimSpace(key))
	if err != nil {
		return nil, err
	}
	return signed, nil
}

// agenticKitWriterFor builds the kit writer, or returns nil when this worker
// cannot publish kits. Returning a TYPED nil would make the dispatcher's
// KitWriter != nil check pass and then panic — the same interface trap the
// token minter already documents — so the nil is returned through the
// interface type explicitly.
func agenticKitWriterFor(instanceRoot, blobEndpoint string, minter *podauth.SignedKey) dispatcher.KitWriter {
	if strings.TrimSpace(instanceRoot) == "" || strings.TrimSpace(blobEndpoint) == "" {
		return nil
	}
	return agenticKitWriter{
		instanceRoot: instanceRoot,
		blobEndpoint: blobEndpoint,
		minter:       minter,
		registrar:    journal.NewRegistryScrubber(),
	}
}
