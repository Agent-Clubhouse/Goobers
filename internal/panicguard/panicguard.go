// Package panicguard contains a panic at a boundary where unwinding into the
// caller would be worse than losing the call.
//
// It exists mainly for packages that cannot call the builtin recover at all:
// internal/journal declares its own recover for journal recovery, which shadows
// the builtin package-wide, so containment there has to happen behind a call
// into another package.
package panicguard

// Call invokes fn(arg) and reports whether it panicked.
//
// The panic value is deliberately discarded rather than returned: callers use
// this where the panic is a bug in the callee and the caller's only correct
// response is to count it and carry on. A caller that needs the value, a stack,
// or a re-panic should write its own deferred recover instead.
func Call[T any](fn func(T), arg T) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	fn(arg)
	return false
}
