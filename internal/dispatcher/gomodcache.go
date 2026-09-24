package dispatcher

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The durable Go module cache (#4710) is a PersistentVolumeClaim each gaggle
// namespace must supply. A namespace provisioned before that claim shipped
// does not have it, and a pod that mounts a missing claim is never scheduled:
// every pod-placed stage sat Unschedulable until the unschedulable grace
// expired and burned the attempt (#5595). The dispatcher therefore checks the
// claim before creating a pod and, when it is absent or cannot be read, mounts
// an emptyDir at the same path instead. GOMODCACHE is unchanged either way; the
// only cost of the fallback is a cold module cache.
const (
	// AnnotationGoModCache is stamped on a stage pod whose module cache fell
	// back to an emptyDir, so `kubectl describe` shows why its build is cold.
	AnnotationGoModCache = "goobers.dev/go-mod-cache"
	// GoModCacheEphemeral is AnnotationGoModCache's value on a fallback pod.
	GoModCacheEphemeral = "ephemeral"
	// goModCacheProbeTTL bounds how long one namespace's claim lookup is
	// reused, so the check is not an API call per pod while a claim an
	// operator creates later is picked up within a minute.
	goModCacheProbeTTL = time.Minute
)

type goModCacheProbe struct {
	present bool
	at      time.Time
}

// goModCacheProbes caches the per-namespace claim lookup (#5595).
type goModCacheProbes struct {
	mu          sync.Mutex
	byNamespace map[string]goModCacheProbe
}

// goModCacheClaimPresent reports whether namespace holds the durable module
// cache claim. Any lookup error, NotFound or Forbidden alike, reads as absent:
// an emptyDir pod runs cold, a pod on a missing claim never runs at all. The
// operator warning is logged once per namespace each time the claim goes
// missing, not once per pod.
func (d *Dispatcher) goModCacheClaimPresent(ctx context.Context, namespace string) bool {
	d.goModCache.mu.Lock()
	defer d.goModCache.mu.Unlock()
	now := d.now()
	previous, seen := d.goModCache.byNamespace[namespace]
	if seen && now.Sub(previous.at) < goModCacheProbeTTL {
		return previous.present
	}
	_, err := d.pods.GetPersistentVolumeClaim(ctx, namespace, goBuildCacheClaim)
	present := err == nil
	if !present && (!seen || previous.present) {
		d.logf("dispatcher: gaggle namespace %q has no readable PersistentVolumeClaim %q (%v); "+
			"stage pods there mount an emptyDir Go module cache instead, so every build starts cold "+
			"until the claim exists (deploy/reference/gaggle-namespace, #5595)",
			namespace, goBuildCacheClaim, err)
	}
	if d.goModCache.byNamespace == nil {
		d.goModCache.byNamespace = map[string]goModCacheProbe{}
	}
	d.goModCache.byNamespace[namespace] = goModCacheProbe{present: present, at: now}
	return present
}

// useEphemeralGoModCache swaps the rendered pod's durable module cache claim
// for an emptyDir at the same mount, leaving the mount and GOMODCACHE as
// stampVolumes wrote them, and annotates the pod as running cold.
func useEphemeralGoModCache(pod *corev1.Pod) {
	for i := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[i]
		if volume.Name == goBuildCacheVolume && volume.PersistentVolumeClaim != nil &&
			volume.PersistentVolumeClaim.ClaimName == goBuildCacheClaim {
			volume.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		}
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationGoModCache] = GoModCacheEphemeral
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.cfg.Logf != nil {
		d.cfg.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
