package dispatcher

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

var claimResource = schema.GroupResource{Resource: "persistentvolumeclaims"}

// goModCacheDispatcher builds a dispatcher on pods whose warnings are
// captured instead of logged.
func goModCacheDispatcher(t *testing.T, pods PodAPI) (*Dispatcher, *fakeClock, *[]string) {
	t.Helper()
	var warnings []string
	cfg := testConfig()
	cfg.Logf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	d, clock := newTestDispatcher(t, cfg, pods, nil)
	return d, clock, &warnings
}

// goModCacheVolume returns the rendered pod's module cache volume, failing
// unless the stage container mounts it at path with GOMODCACHE pointing there.
func goModCacheVolume(t *testing.T, pod *corev1.Pod, path string) corev1.Volume {
	t.Helper()
	var volume *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == goBuildCacheVolume {
			volume = &pod.Spec.Volumes[i]
		}
	}
	if volume == nil {
		t.Fatalf("pod has no %q volume", goBuildCacheVolume)
	}
	var mounted bool
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		mounted = mounted || (mount.Name == goBuildCacheVolume && mount.MountPath == path)
	}
	if !mounted {
		t.Fatalf("module cache volume is not mounted at %q", path)
	}
	if got := podEnv(pod)["GOMODCACHE"]; got != path {
		t.Fatalf("GOMODCACHE = %q, want %q", got, path)
	}
	return *volume
}

// #5595: with the claim present nothing changes — the PVC stays mounted and
// the pod carries no cold-cache annotation.
func TestRenderForKeepsGoModCacheClaimWhenPresent(t *testing.T) {
	d, _, warnings := goModCacheDispatcher(t, &fakePodAPI{})
	pod, err := d.renderFor(context.Background(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("renderFor: %v", err)
	}
	volume := goModCacheVolume(t, pod, LinuxGoCachePath)
	if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != goBuildCacheClaim || volume.EmptyDir != nil {
		t.Fatalf("cache volume = %+v, want PVC %q", volume, goBuildCacheClaim)
	}
	if _, ok := pod.Annotations[AnnotationGoModCache]; ok {
		t.Fatalf("annotation %s stamped although the claim exists", AnnotationGoModCache)
	}
	if len(*warnings) != 0 {
		t.Fatalf("warned %v although the claim exists", *warnings)
	}
}

// #5595: a missing claim (NotFound) or an unreadable one (Forbidden, any
// other error) yields a schedulable pod on an emptyDir cache at the same path,
// on Linux and Windows alike, with GOMODCACHE unchanged.
func TestRenderForFallsBackToEmptyDirWhenGoModCacheClaimUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		runner RunnerSpec
		path   string
	}{
		{"linux-not-found", apierrors.NewNotFound(claimResource, goBuildCacheClaim), linuxRunner(), LinuxGoCachePath},
		{"linux-forbidden", apierrors.NewForbidden(claimResource, goBuildCacheClaim, fmt.Errorf("rbac")), linuxRunner(), LinuxGoCachePath},
		{"linux-other-error", fmt.Errorf("connection refused"), linuxRunner(), LinuxGoCachePath},
		{"windows-not-found", apierrors.NewNotFound(claimResource, goBuildCacheClaim), windowsRunner(), WindowsGoCachePath},
		{"windows-forbidden", apierrors.NewForbidden(claimResource, goBuildCacheClaim, fmt.Errorf("rbac")), windowsRunner(), WindowsGoCachePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, warnings := goModCacheDispatcher(t, &fakePodAPI{claimErr: tc.err})
			pod, err := d.renderFor(context.Background(), testAttempt(), tc.runner)
			if err != nil {
				t.Fatalf("renderFor: %v", err)
			}
			volume := goModCacheVolume(t, pod, tc.path)
			if volume.PersistentVolumeClaim != nil || volume.EmptyDir == nil {
				t.Fatalf("cache volume = %+v, want an emptyDir in place of the missing claim", volume)
			}
			if got := pod.Annotations[AnnotationGoModCache]; got != GoModCacheEphemeral {
				t.Fatalf("annotation %s = %q, want %q", AnnotationGoModCache, got, GoModCacheEphemeral)
			}
			if len(*warnings) != 1 || !strings.Contains((*warnings)[0], goBuildCacheClaim) || !strings.Contains((*warnings)[0], "gaggle-alpha") {
				t.Fatalf("warnings = %v, want one naming the claim and namespace", *warnings)
			}
		})
	}
}

// #5595: the lookup is cached per namespace for goModCacheProbeTTL, warns once
// per namespace rather than per pod, and notices a claim created later.
func TestGoModCacheClaimLookupIsCachedAndWarnsOnce(t *testing.T) {
	pods := &fakePodAPI{claimErr: apierrors.NewNotFound(claimResource, goBuildCacheClaim)}
	d, clock, warnings := goModCacheDispatcher(t, pods)
	ctx := context.Background()
	for range 3 {
		if d.goModCacheClaimPresent(ctx, "gaggle-alpha") {
			t.Fatal("claim reported present although lookup failed")
		}
	}
	if pods.claimLookups != 1 {
		t.Fatalf("claim lookups = %d within the TTL, want 1", pods.claimLookups)
	}
	clock.now = clock.now.Add(goModCacheProbeTTL)
	if d.goModCacheClaimPresent(ctx, "gaggle-alpha") || pods.claimLookups != 2 {
		t.Fatalf("expired probe was not re-read (lookups = %d)", pods.claimLookups)
	}
	if len(*warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one while the claim stays missing", *warnings)
	}

	pods.claimErr = nil
	clock.now = clock.now.Add(goModCacheProbeTTL)
	if !d.goModCacheClaimPresent(ctx, "gaggle-alpha") {
		t.Fatal("claim created after the fallback was not picked up once the probe expired")
	}
	if d.goModCacheClaimPresent(ctx, "gaggle-beta"); pods.claimLookups != 4 {
		t.Fatalf("claim lookups = %d, want a separate lookup per namespace", pods.claimLookups)
	}
}

// The client-go PodAPI reads the claim with a single GET: NotFound and a
// Forbidden (a Role without the #5595 verb) both surface as errors, which the
// dispatcher treats as absent.
func TestKubernetesPodAPIGetPersistentVolumeClaim(t *testing.T) {
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{}
	claim.Namespace, claim.Name = "gaggle-web", goBuildCacheClaim
	api := NewKubernetesPodAPI(fake.NewClientset(claim))
	if got, err := api.GetPersistentVolumeClaim(ctx, "gaggle-web", goBuildCacheClaim); err != nil || got.Name != goBuildCacheClaim {
		t.Fatalf("GetPersistentVolumeClaim = %v %v, want the claim", got, err)
	}
	if _, err := api.GetPersistentVolumeClaim(ctx, "gaggle-other", goBuildCacheClaim); !apierrors.IsNotFound(err) {
		t.Fatalf("missing claim error = %v, want NotFound", err)
	}

	forbidden := fake.NewClientset(claim)
	forbidden.PrependReactor("get", "persistentvolumeclaims", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(claimResource, goBuildCacheClaim, fmt.Errorf("rbac"))
	})
	if _, err := NewKubernetesPodAPI(forbidden).GetPersistentVolumeClaim(ctx, "gaggle-web", goBuildCacheClaim); !apierrors.IsForbidden(err) {
		t.Fatalf("forbidden claim read error = %v, want Forbidden", err)
	}
}

// End to end through Dispatch: a namespace without the claim still gets a
// created pod whose cache is an emptyDir (#5595).
func TestDispatchWithoutGoModCacheClaimCreatesSchedulablePod(t *testing.T) {
	pods := &fakePodAPI{claimErr: apierrors.NewNotFound(claimResource, goBuildCacheClaim)}
	d, _, _ := goModCacheDispatcher(t, pods)
	if _, err := d.Dispatch(context.Background(), testAttempt(), []RunnerSpec{linuxRunner()}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(pods.createdSpecs) != 1 {
		t.Fatalf("created %d pods, want 1", len(pods.createdSpecs))
	}
	if volume := goModCacheVolume(t, pods.createdSpecs[0], LinuxGoCachePath); volume.EmptyDir == nil {
		t.Fatalf("created pod's cache volume = %+v, want emptyDir", volume)
	}
}
