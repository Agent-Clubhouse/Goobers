package dispatcher

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Cancellation must observe disappearance, not only acceptance of DELETE.
// The caller supplies the same bounded context used to request disposal.
// A failed observation reports uncertainty; it never force-deletes a pod or
// overrides Kubernetes termination grace.
func (d *Dispatcher) awaitDisposedPod(ctx context.Context, namespace, name string) error {
	for {
		_, err := d.pods.GetPod(ctx, namespace, name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("dispatcher: observe disposal of pod %s/%s: %w", namespace, name, err)
		}
		if err := d.sleep(ctx, time.Second); err != nil {
			return fmt.Errorf("dispatcher: pod %s/%s disappearance not confirmed: %w", namespace, name, err)
		}
	}
}
