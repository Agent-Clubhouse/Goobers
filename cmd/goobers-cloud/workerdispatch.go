package main

// workerdispatch.go wires the #3513 dispatcher behind the engine's mode-3
// stage-dispatch seam (#3588): `goobers-cloud worker --dispatch-namespace <ns>`
// binds the per-(gaggle × runner-type) dispatch queues beside the workflow
// queue(s) and serves DispatchStage with a real dispatcher — pods created
// through the cluster credentials this process runs under (in-cluster
// ServiceAccount, or the standard kubeconfig loading rules outside a pod).

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/runtimewiring"

	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/goobers/goobers/internal/childadmissionclient"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/version"
)

// dispatchNamespacePreflightTimeout bounds the startup RBAC/existence check
// (#4897) so an unreachable API server fails the worker's boot loudly and
// promptly instead of hanging it indefinitely before it ever polls a queue.
const dispatchNamespacePreflightTimeout = 30 * time.Second

// preflightGaggleNamespaces is a seam beside dispatchKubeClient/newStageDispatcher:
// a fake clientset's default SelfSubjectAccessReview reactor answers
// "not allowed" rather than emulating real RBAC, which would fail every
// wiring test that fakes the cluster. Tests exercising this seam directly
// override it; every other wiring test overrides it to a no-op success, the
// same way they already fake dispatchKubeClient.
var preflightGaggleNamespaces = dispatcher.PreflightNamespaces

// runtimewiring.GaggleNamespacesFromConfig builds the gaggle -> isolation namespace map
// (#4897) a dispatcher.Config needs to route each stage pod to its OWN
// gaggle's declared namespace instead of one process-wide value. Every
// gaggle's isolation.namespace is schema-required, so an empty value here
// means a config that loaded despite failing that requirement — refused
// rather than silently dispatching that gaggle nowhere.

// stageDispatch is what buildStageDispatch wires: the dispatcher seam, the
// surrender plane, the dispatch queues this worker must serve, and the same
// dispatcher typed for the boot-time orphan sweep.
type stageDispatch struct {
	Dispatcher engine.StageDispatcher
	Surrenders dispatcher.SurrenderPlane
	Queues     []string
	// Sweeper is the SAME *dispatcher.Dispatcher as Dispatcher, named through
	// the narrow sweep interface. Two fields rather than a type assertion make
	// the boot and worker-lifetime reconciliation wiring explicit.
	Sweeper stageOrphanSweeper
	// Children is the engine child-admission plane client, or nil when the
	// instance has no signing key (the engine then refuses child custody).
	Children engine.ChildService
	// ChildAuthority acquires stage attempts' child-workflow grants over the
	// same plane, or nil without the key or the worker's secret registry.
	ChildAuthority engine.ChildWorkflowAuthority
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

// newStageDispatcher is a seam beside dispatchKubeClient, for the same reason:
// what buildStageDispatch does that nothing else does is ASSEMBLE the
// dispatcher's Config from the instance's config, and that assembly is
// unobservable once it is inside a constructed Dispatcher. Config.EnvPassthrough
// is the sharp one — it is the operator's env:default-deny hatch on the pod
// substrate (#3725/#736), and deleting the line that threads it would leave the
// helper-level tests green and the hatch dead on the far side.
var newStageDispatcher = dispatcher.New

// buildStageDispatch loads the instance's runner inventory and constructs the
// dispatcher-backed seam. Directory mode reads surrenders under blobRoot;
// endpoint mode reads the identity-keyed surrender plane through daemonAPI
// with a dedicated worker credential, requiring no shared artifact volume.
// owner is this worker's dispatcher identity (its hostname; in-cluster, its
// pod name), stamped on every pod as rollout-scoped diagnostic provenance.
// The durable identity read from instanceRoot scopes orphan sweeps.
// seams is the worker's own config-snapshot store, shared so the mode-3 kit
// writer resolves a stage pod's kit through the SAME current-plus-retained
// config trees the self-execution path resolves against (#3884). Nil disables
// the kit writer entirely, which makes Dispatch refuse agentic stages
// explicitly rather than create a pod that would find no kit — and, crucially,
// rather than fall back to reading whatever config tree is mounted, which is
// the substitution the pin exists to prevent.
func buildStageDispatch(instanceRoot, daemonAPI, blobRoot, owner string, seams *workerSeams, workerBlobEndpoint string) (stageDispatch, error) {
	if blobRoot == "" && workerBlobEndpoint == "" {
		return stageDispatch{}, fmt.Errorf("stage dispatch: a surrender plane is required — pass --blob-store or --blob-endpoint")
	}
	if strings.TrimSpace(owner) == "" {
		return stageDispatch{}, fmt.Errorf("stage dispatch: a dispatcher owner identity is required for pod provenance")
	}
	l := instance.NewLayout(instanceRoot)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: load instance config: %w", err)
	}
	instanceID, err := instance.ReadRootIdentity(l.Root)
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: read instance identity: %w", err)
	}
	blobEndpoint := stageBlobEndpoint(workerBlobEndpoint)
	signed, err := validateStageDispatchConfig(cfg, daemonAPI, blobEndpoint)
	if err != nil {
		return stageDispatch{}, err
	}
	recoveryCustodyTimeout, err := cfg.Runner.RecoveryCustodyTimeoutDuration()
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}

	set, report, err := loadConfigDirectory(l.ConfigDir())
	if err != nil {
		if issues := runtimewiring.ValidationIssueSummary(report); issues != "" {
			return stageDispatch{}, fmt.Errorf("stage dispatch: load config directory: %w (%s)", err, issues)
		}
		return stageDispatch{}, fmt.Errorf("stage dispatch: load config directory: %w", err)
	}

	surrenders, err := workerSurrenderPlane(blobRoot, daemonAPI, owner, signed)
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
	gaggleNamespaces, err := runtimewiring.GaggleNamespacesFromConfig(set.Gaggles)
	if err != nil {
		return stageDispatch{}, err
	}

	client, err := dispatchKubeClient()
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}
	// #4897: fail startup clearly, before this worker polls or dispatches
	// anything, when a declared gaggle namespace does not exist or this
	// worker's own credentials lack the RBAC grants the dispatch/cleanup path
	// depends on — never discover a Forbidden after a stage was already
	// claimed from the backlog.
	preflightCtx, cancelPreflight := context.WithTimeout(context.Background(), dispatchNamespacePreflightTimeout)
	_, preflightErr := preflightGaggleNamespaces(preflightCtx, client, gaggleNamespaces, dispatcher.ServiceAccounts(set.Gaggles))
	cancelPreflight()
	if preflightErr != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: gaggle namespace preflight: %w", preflightErr)
	}
	// Each gaggle's backlog settings, resolved here for the same reason as
	// BotLogins below: a stage pod cannot read the instance config (#5957).
	gaggleBacklogs, err := stageGaggleBacklogStamps(set.Gaggles)
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}
	build := version.Get()
	tmpfsSize, err := cfg.Runner.ResolvePodTmpfsSize()
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}
	d, err := newStageDispatcher(dispatcher.Config{
		TmpfsSizeLimit: tmpfsSize,
		TmpDiskBacked:  cfg.Runner.PodTmpDiskBacked(),
		PodEgressProxy: cfg.Runner.PodEgressProxy,
		// Validation guarantees a non-nil signer before it enters the interface.
		TokenMinter: signed,
		// The kit writer uses the same key and the worker's pinned config
		// snapshots. A test constructor without seams still refuses agentic
		// dispatch explicitly instead of creating a pod that would find no kit.
		KitWriter:              agenticKitWriterFor(instanceRoot, seams, blobEndpoint, signed),
		GaggleNamespaces:       gaggleNamespaces,
		GaggleServiceAccounts:  dispatcher.ServiceAccounts(set.Gaggles),
		InstanceID:             instanceID,
		Owner:                  owner,
		EmbeddedCommit:         build.Commit,
		EmbeddedVersion:        build.Version,
		BlobEndpoint:           blobEndpoint,
		WriteAPIBase:           daemonAPI,
		RecoveryCustodyTimeout: recoveryCustodyTimeout,
		// The same operator-declared passthrough list the local executor gets
		// (runnerwiring_executors.go: shell.ExtraEnvAllowlist), so a stage on a
		// runner class enforcing env:default-deny keeps the vars an operator
		// declared for it instead of losing them by substrate (#3725/#736).
		EnvPassthrough: cfg.Runner.EnvPassthrough,
		// The configured bot logins, resolved HERE because this process can
		// read the instance config and a stage pod cannot (#3914). Deleting
		// this line leaves every helper-level test green and silently returns
		// every pod stage to GET /user — which a GitHub App installation token
		// cannot call — so it is pinned by
		// TestBuildStageDispatchThreadsTheConfiguredBotLoginToTheStagePod.
		BotLogins:                   cfg.GitHubBotLogins(),
		ExternalTelemetryConnectors: cfg.ExternalTelemetryConnectorsByName(),
		NeedsHumanAssignee:          cfg.NeedsHumanAssignee,
		GaggleBacklogs:              gaggleBacklogs,
	}, dispatcher.NewKubernetesPodAPI(client), nil, dispatcher.PlaneSurrenderGate{Plane: surrenders}, nil)
	if err != nil {
		return stageDispatch{}, fmt.Errorf("stage dispatch: %w", err)
	}
	dispatch := stageDispatch{Dispatcher: d, Surrenders: surrenders, Queues: queues, Sweeper: d, Children: childadmissionclient.ForWorker(daemonAPI, owner, signed)}
	if seams != nil {
		// Pods reach control at WriteAPIBase, which is daemonAPI.
		// Probe the plane the stage pods are handed (GOOBERS_BLOB_ENDPOINT),
		// not the worker's own store: only that proves pods can read what
		// control writes.
		podPlane := &dispatcher.BlobClient{BaseURL: blobEndpoint, TokenSource: func() (string, error) { return signed.MintWorkerBlob(owner, 2*time.Minute) }}
		dispatch.ChildAuthority = probedChildAuthority(childadmissionclient.AuthorityForWorker(daemonAPI, daemonAPI, owner, signed, seams.SharedRegistry()), podPlane, instanceID, seams.log)
	}
	return dispatch, nil
}

// canaryPlane is the blob plane a canary probe reads through.
type canaryPlane interface {
	Get(ctx context.Context, digest string) ([]byte, error)
	Describe() string
}

// probedChildAuthority withholds child-workflow authority until the blob
// plane the stage pods use is shown to serve control's object store:
// control's canary for this instance must be readable through it (#8085 d,
// #8188). The probe runs at boot and again, paced, while it fails. Without a
// plane to probe it fails closed.
func probedChildAuthority(authority engine.ChildWorkflowAuthority, plane canaryPlane, instanceID string, logf func(string, ...any)) engine.ChildWorkflowAuthority {
	if authority == nil {
		return nil
	}
	digest, _ := childresult.BlobPlaneCanary(instanceID)
	probed := &childadmissionclient.ProbedAuthority{Inner: authority}
	if plane == nil {
		probed.Probe = func(context.Context) error { return errors.New("the worker has no stage blob plane to probe") }
	} else {
		probed.Probe = func(ctx context.Context) error {
			_, err := plane.Get(ctx, digest)
			return err
		}
		probed.Report = func(err error) {
			if logf != nil {
				logf("ERROR: %v: canary %s not readable through the stage blob plane %s", err, digest, plane.Describe())
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = probed.Verify(ctx)
	return probed
}

// validateStageDispatchConfig checks the remote planes before the worker can
// poll or create pods. Even a scratch deterministic stage must authenticate its
// surrender; agentic kits and artifact outputs additionally use the blob plane.
// Keep errors independent of the supplied URL: it may contain a leaked secret.
func validateStageDispatchConfig(cfg *instance.Config, daemonAPI, blobEndpoint string) (*podauth.SignedKey, error) {
	for _, endpoint := range []struct{ name, value string }{
		{"--daemon-api (or GOOBERS_DAEMON_API)", daemonAPI},
		{"GOOBERS_BLOB_ENDPOINT", blobEndpoint},
	} {
		if err := validateStageDispatchEndpoint(endpoint.value); err != nil {
			return nil, fmt.Errorf("stage dispatch: %s %w", endpoint.name, err)
		}
	}
	signed, err := podTokenMinter(cfg)
	if err != nil {
		return nil, fmt.Errorf("stage dispatch: api.podTokenKeyFile / api.podTokenKeyRingFile: %w", err)
	}
	if signed == nil {
		return nil, fmt.Errorf("stage dispatch: api.podTokenKeyFile is required for authenticated stage surrender (or api.podTokenKeyRingFile for cloud)")
	}
	return signed, nil
}

func validateStageDispatchEndpoint(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return fmt.Errorf("must be an absolute HTTP(S) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return fmt.Errorf("must not contain credentials, a query, or a fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("must have a valid TCP port")
		}
	}
	return nil
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
// Dispatch, journal, blob and worker-plane credentials share this loader.
// Raw files keep legacy semantics; cloud rings explicitly select current and
// include its key ID so control can verify tokens during rotation.
func podTokenMinter(cfg *instance.Config) (*podauth.SignedKey, error) {
	if cfg == nil {
		return nil, nil
	}
	path := strings.TrimSpace(cfg.API.PodTokenKeyFile)
	ringPath := strings.TrimSpace(cfg.API.PodTokenKeyRingFile)
	if path != "" && ringPath != "" {
		return nil, fmt.Errorf("api.podTokenKeyFile and api.podTokenKeyRingFile are mutually exclusive")
	}
	if ringPath != "" {
		return podauth.LoadKeyRingFile(ringPath, "")
	}
	if path == "" {
		return nil, nil
	}
	signed, err := podauth.LoadSignedKeyFile(path)
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
// A nil seams also returns nil: a kit writer with no snapshot store could only
// resolve a kit from ambient config, and publishing a pod kit that ignores the
// run's pinned goober digest is exactly the silent staleness #3884 closes.
func agenticKitWriterFor(instanceRoot string, seams *workerSeams, blobEndpoint string, minter *podauth.SignedKey) dispatcher.KitWriter {
	if strings.TrimSpace(instanceRoot) == "" || strings.TrimSpace(blobEndpoint) == "" || seams == nil {
		return nil
	}
	return agenticKitWriter{
		instanceRoot: instanceRoot,
		seams:        seams,
		blobEndpoint: blobEndpoint,
		minter:       minter,
		registrar:    journal.NewRegistryScrubber(),
	}
}
