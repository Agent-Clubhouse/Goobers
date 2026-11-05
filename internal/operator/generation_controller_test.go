package operator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/configsync"
)

func TestGenerationPointerReconcileFiltersNamespaceAndGeneration(t *testing.T) {
	pointer := generationPointer("gnew")
	current := publishedGaggleFixture("web", "gnew")
	old := publishedGaggleFixture("web", "gold")
	foreign := publishedGaggleFixture("web", "gnew")
	foreign.Namespace = "other-control-namespace"
	_, c := newReconciler(t, nil, pointer, current, old, foreign)
	events := make(chan event.GenericEvent, 4)
	r := &generationPointerReconciler{Client: c, events: events}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pointer)}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want exactly the published Gaggle", len(events))
	}
	if got := client.ObjectKeyFromObject((<-events).Object); got != client.ObjectKeyFromObject(current) {
		t.Fatalf("event = %v, want %v", got, client.ObjectKeyFromObject(current))
	}
	if !isGenerationPointer(pointer) || isGenerationPointer(&corev1.ConfigMap{}) {
		t.Fatal("watch predicate must select only the generation pointer ConfigMap")
	}
}

func TestGenerationPointerReconcileErrorsRemainRetryable(t *testing.T) {
	pointer := generationPointer("gnew")
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("API unavailable")
	for _, operation := range []string{"get", "list"} {
		t.Run(operation, func(t *testing.T) {
			interceptors := interceptor.Funcs{}
			if operation == "get" {
				interceptors.Get = func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return wantErr
				}
			} else {
				interceptors.List = func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return wantErr
				}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pointer).WithInterceptorFuncs(interceptors).Build()
			r := &generationPointerReconciler{Client: c}
			var logs bytes.Buffer
			ctx := log.IntoContext(t.Context(), logr.FromSlogHandler(slog.NewJSONHandler(&logs, nil)))
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pointer)})
			if !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want API error for controller-runtime to retry", err)
			}
			if operation == "list" {
				for _, want := range []string{"requeuing", "API unavailable", `"namespace":"goobers-system"`, `"generation":"gnew"`} {
					if !strings.Contains(logs.String(), want) {
						t.Errorf("log = %s, want %q", logs.String(), want)
					}
				}
			}
		})
	}
}

func TestGenerationPointerReconcileEmptyOrDeleted(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		pointer := generationPointer("")
		_, c := newReconciler(t, nil, pointer)
		if deleted {
			if err := c.Delete(t.Context(), pointer); err != nil {
				t.Fatal(err)
			}
		}
		// A nil event channel would block if these no-op cases tried to fan out.
		r := &generationPointerReconciler{Client: c}
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pointer)}); err != nil {
			t.Fatalf("deleted=%v: %v", deleted, err)
		}
	}
}

func TestGenerationPointerReconcileCancellationUnblocksFanout(t *testing.T) {
	pointer := generationPointer("gnew")
	_, c := newReconciler(t, nil, pointer, publishedGaggleFixture("web", "gnew"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &generationPointerReconciler{Client: c, events: make(chan event.GenericEvent)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pointer)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation without a channel reader", err)
	}
}

func TestGenerationPointerControllerRecoversAfterExtendedListFailure(t *testing.T) {
	pointer := generationPointer("gnew")
	latest := publishedGaggleFixture("web", "glatest")
	r, base := newReconciler(t, nil, pointer,
		publishedGaggleFixture("web", "gnew"), latest,
		publishedGooberFixture("coder", latest.Name, "glatest"))
	var listCalls atomic.Int32
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.GaggleList); ok {
				call := listCalls.Add(1)
				if call == 3 {
					// No second watch event is sent. A retry must read this new pointer.
					var current corev1.ConfigMap
					if err := c.Get(ctx, client.ObjectKeyFromObject(pointer), &current); err != nil {
						return err
					}
					current.Data[configsync.GenerationConfigMapKey] = "glatest"
					if err := c.Update(ctx, &current); err != nil {
						return err
					}
				}
				if call <= 5 {
					return errors.New("extended API outage")
				}
			}
			return c.List(ctx, list, opts...)
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	events := make(chan event.GenericEvent)
	reconciled := make(chan ctrl.Request, 1)
	startGenerationTestController(t, ctx, "gaggle", reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
		result, err := r.Reconcile(ctx, req)
		if err == nil {
			select {
			case reconciled <- req:
			case <-ctx.Done():
			}
		}
		return result, err
	}), generationGaggleSource(events))
	triggers := make(chan event.GenericEvent, 1)
	startGenerationTestController(t, ctx, "pointer", &generationPointerReconciler{Client: r.Client, events: events},
		source.Channel(triggers, &handler.EnqueueRequestForObject{}))
	triggers <- event.GenericEvent{Object: pointer}
	select {
	case req := <-reconciled:
		if req.NamespacedName != client.ObjectKeyFromObject(latest) {
			t.Fatalf("reconciled %v, want latest generation %v", req, client.ObjectKeyFromObject(latest))
		}
	case <-ctx.Done():
		t.Fatal("one pointer event did not recover through the controller retry queue")
	}
	if calls := listCalls.Load(); calls != 6 {
		t.Fatalf("List calls = %d, want recovery after five failures", calls)
	}
	var dep appsv1.Deployment
	if err := base.Get(ctx, types.NamespacedName{Namespace: "gaggle-web", Name: "goober-coder"}, &dep); err != nil {
		t.Fatalf("published generation did not create its worker: %v", err)
	}
	if dep.Labels[gaggleResourceLabel] != latest.Name {
		t.Fatalf("worker belongs to %q, want %q", dep.Labels[gaggleResourceLabel], latest.Name)
	}
}

func startGenerationTestController(t *testing.T, parent context.Context, name string, reconciler reconcile.Reconciler, events source.Source) {
	t.Helper()
	c, err := controller.NewUnmanaged(t.Name()+"-"+name, controller.Options{
		Reconciler: reconciler, Logger: logr.Discard(), SkipNameValidation: ptr.To(true),
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Millisecond, time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Watch(events); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("controller stopped: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("controller did not stop after cancellation")
		}
	})
}
