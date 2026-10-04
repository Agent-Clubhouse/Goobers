package proc

import "errors"

// ErrQuiescenceUnobservable means the host cannot attest workspace writer
// termination. Ordinary process cleanup may succeed, but capture must refuse.
var ErrQuiescenceUnobservable = errors.New("proc: workspace writer termination is unobservable")
