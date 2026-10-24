package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childadmissionclient"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/cloud/childengine"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
)

type grantingAuthority struct{ acquired int }

func (g *grantingAuthority) AcquireChildAuthority(context.Context, engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	g.acquired++
	return engine.ChildAuthorityGrant{GrantID: "g"}, nil
}
func (g *grantingAuthority) ReleaseChildAuthority(context.Context, engine.ChildAuthorityRelease) error {
	return nil
}

// A worker lends stages child-workflow authority only when control's canary
// for this instance is readable through the stage blob plane; a plane that
// does not serve control's store withholds it and logs why (#8085 d, #8188).
func TestWorkerChildAuthorityRequiresControlsCanaryOnTheBlobPlane(t *testing.T) {
	const instanceID = "instance-probe"
	for _, served := range []bool{false, true} {
		store, err := blobstore.NewDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if served {
			digest, data := childresult.BlobPlaneCanary(instanceID)
			if err := store.Put(t.Context(), digest, data); err != nil {
				t.Fatal(err)
			}
		}
		var logged []string
		inner := &grantingAuthority{}
		authority := probedChildAuthority(inner, store, instanceID, func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
		_, err = authority.AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{})
		switch {
		case served && (err != nil || inner.acquired != 1 || len(logged) != 0):
			t.Fatalf("plane serving control's canary: acquire = %v (control reached %d, logged %q)", err, inner.acquired, logged)
		case !served && (!errors.Is(err, childresult.ErrBlobPlaneUnverified) || inner.acquired != 0 || len(logged) == 0):
			t.Fatalf("plane without control's canary: acquire = %v (control reached %d, logged %q), want withheld and logged", err, inner.acquired, logged)
		}
	}
	if probedChildAuthority(nil, store0(t), instanceID, nil) != nil {
		t.Fatal("a worker without child authority gained one")
	}
}

// Control puts exactly the canary a worker for the same instance probes for,
// and refuses to compose the protocol without an object store.
func TestControlPutsTheCanaryWorkersProbeFor(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobPlaneCanary(t.Context(), store, "instance-probe"); err != nil {
		t.Fatal(err)
	}
	if err := putBlobPlaneCanary(t.Context(), store, "instance-probe"); err != nil {
		t.Fatalf("a repeated put at the next boot = %v", err)
	}
	inner := &grantingAuthority{}
	if _, err := probedChildAuthority(inner, store, "instance-probe", nil).AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{}); err != nil || inner.acquired != 1 {
		t.Fatalf("a worker probing control's own canary = %v (reached %d)", err, inner.acquired)
	}
	if err := putBlobPlaneCanary(t.Context(), nil, "instance-probe"); err == nil {
		t.Fatal("composed the protocol without an object store")
	}
}

// The stage dispatch the worker builds lends child-workflow authority only
// through the canary probe: dropping the wrapper would lend unprobed
// authority, so this pins the composition, not just the wrapper (#8191 P2-1).
func TestBuildStageDispatchProbesBeforeLendingChildAuthority(t *testing.T) {
	root := initDemo(t)
	configureDispatchAuthority(t, root)
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Engine = &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "goobers-engine"}
	cfg.Runners = append(cfg.Runners, instance.RunnerEntry{Name: "linux-cli", Host: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		Provides: instance.RunnerProvides{OS: "linux", CPU: "2000m", Memory: "4Gi", Disk: "20Gi", Shell: true}})
	if err := instance.WriteConfig(layout.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return fake.NewClientset(), nil }
	t.Cleanup(func() { dispatchKubeClient = previousClient })
	previousPreflight := preflightGaggleNamespaces
	preflightGaggleNamespaces = fakeClusterHasNoRealRBACSoSkipPreflight
	t.Cleanup(func() { preflightGaggleNamespaces = previousPreflight })

	// The worker's own store holds control's canary; the plane the pods are
	// handed does not. Only the pods' plane counts.
	workerStore, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	podStore, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := instance.ReadRootIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobPlaneCanary(t.Context(), workerStore, instanceID); err != nil {
		t.Fatal(err)
	}
	var bearers []string
	podPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearers = append(bearers, r.Header.Get("Authorization"))
		digest := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(apicontract.BlobDigestPath, "{digest}"))
		data, err := podStore.Get(r.Context(), digest)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(podPlane.Close)
	seams, err := newWorkerSeams(root, workerStore)
	if err != nil {
		t.Fatal(err)
	}
	seams.logf = func(format string, args ...any) { t.Logf(format, args...) }
	dispatch, err := buildStageDispatch(root, "https://daemon.example:8080", t.TempDir(), "goobers-worker-0", seams, podPlane.URL)
	if err != nil {
		t.Fatalf("buildStageDispatch: %v", err)
	}
	probed, ok := dispatch.ChildAuthority.(*childadmissionclient.ProbedAuthority)
	if !ok {
		t.Fatalf("stage dispatch child authority = %T, want the canary-probed authority", dispatch.ChildAuthority)
	}
	if _, err := probed.AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{}); !errors.Is(err, childresult.ErrBlobPlaneUnverified) {
		t.Fatalf("acquire with the canary only in the worker's own store = %v, want withheld: the pods' plane does not serve it", err)
	}
	if len(bearers) == 0 || !strings.HasPrefix(bearers[0], "Bearer ") {
		t.Fatalf("the probe did not reach the pods' plane with a worker bearer: %q", bearers)
	}
	if err := putBlobPlaneCanary(t.Context(), podStore, instanceID); err != nil {
		t.Fatal(err)
	}
	if err := probed.Verify(t.Context()); err != nil {
		t.Fatalf("verify once control's canary is on the pods' plane = %v", err)
	}
}

// Without a stage blob plane to probe, the worker fails closed.
func TestWorkerWithoutABlobPlaneWithholdsChildAuthority(t *testing.T) {
	inner := &grantingAuthority{}
	authority := probedChildAuthority(inner, nil, "instance-probe", nil)
	if _, err := authority.AcquireChildAuthority(t.Context(), engine.ChildAuthorityRequest{}); !errors.Is(err, childresult.ErrBlobPlaneUnverified) || inner.acquired != 0 {
		t.Fatalf("acquire = %v (reached %d), want withheld", err, inner.acquired)
	}
}

func store0(t *testing.T) blobstore.Store {
	t.Helper()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Composing control's engine child protocol puts the canary workers probe for.
func TestControlCompositionPutsTheCanary(t *testing.T) {
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	planes := controlPlanes{engineChildren: &childengine.Fenced{}}
	db := databaseServices{Blobs: store, InstanceID: "instance-probe"}
	_ = constructEngineChildProtocol(t.Context(), db, controlAuth{}, &lease.Manager{}, func() *lease.Handle { return nil }, &planes)
	digest, _ := childresult.BlobPlaneCanary("instance-probe")
	if has, err := store.Has(t.Context(), digest); err != nil || !has {
		t.Fatalf("canary after composing the protocol: present %v (%v)", has, err)
	}
}
