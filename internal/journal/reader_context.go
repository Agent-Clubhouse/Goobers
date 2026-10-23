package journal

import "context"

// contextRunReader checks the opening request before each file operation.
// Individual operating-system reads cannot be interrupted once in progress.
type contextRunReader struct {
	RunReader
	ctx context.Context
}

func (r contextRunReader) Identity() (RunIdentity, error) {
	if err := r.ctx.Err(); err != nil {
		return RunIdentity{}, err
	}
	return r.RunReader.Identity()
}

func (r contextRunReader) State() (State, error) {
	if err := r.ctx.Err(); err != nil {
		return State{}, err
	}
	return r.RunReader.State()
}

func (r contextRunReader) Phase() (RunPhase, error) {
	if err := r.ctx.Err(); err != nil {
		return "", err
	}
	return r.RunReader.Phase()
}

func (r contextRunReader) Events() ([]Event, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	return r.RunReader.Events()
}

func (r contextRunReader) EventRecords() ([]EventRecord, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	return r.RunReader.EventRecords()
}

func (r contextRunReader) ArtifactBytes(ref Ref) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	return r.RunReader.ArtifactBytes(ref)
}

func (r contextRunReader) ArtifactBytesBounded(ref Ref, limit int64) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	return r.RunReader.ArtifactBytesBounded(ref, limit)
}

func (r contextRunReader) ArtifactByDigest(digest string) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	return r.RunReader.ArtifactByDigest(digest)
}

func (r contextRunReader) SpanBytes(ref Ref) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	return r.RunReader.SpanBytes(ref)
}

func (r contextRunReader) PhaseBounded(ctx context.Context) (RunPhase, error) {
	if err := r.ctx.Err(); err != nil {
		return "", err
	}
	return r.RunReader.PhaseBounded(ctx)
}
