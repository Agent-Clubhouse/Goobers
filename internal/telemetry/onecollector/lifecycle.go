package onecollector

import "context"

// AwaitLifecycle prefers an already-completed result over cancellation.
func AwaitLifecycle(ctx context.Context, result <-chan error) error {
	select {
	case err := <-result:
		return err
	default:
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		select {
		case err := <-result:
			return err
		default:
			return ctx.Err()
		}
	}
}
