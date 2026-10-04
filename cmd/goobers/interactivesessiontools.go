package main

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/sessionops"
)

type sessionToolContextKey struct{}
type sessionToolGrant struct {
	run    string
	access *mcpio.SessionOperationAccess
}

func (u *upSession) configureSessionOperations(runtime *daemonSessionRuntime) error {
	if u.setup.SessionBacklogReader == nil && u.setup.SessionBacklogWriter == nil && u.setup.SessionBacklogResolver == nil {
		return nil
	}
	if u.credentialPlane == nil || u.credentialPlane.grants == nil {
		return errors.New("session source operations need a bound daemon endpoint")
	}
	runtime.operations = &sessionops.Bridge{Endpoint: u.credentialPlane.grants.endpoint, Scrubber: journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber()), Secrets: u.setup.SharedRegistry, Now: time.Now}
	u.apiHandlerOpts = append(u.apiHandlerOpts, httpapi.WithSessionOperations(runtime.operations))
	return nil
}
func (g sessionGoober) openOperations(ctx context.Context, env apiv1.InvocationEnvelope, started journal.Event) (context.Context, func(), error) {
	noop := func() {}
	if g.operations == nil {
		return ctx, noop, nil
	}
	runtime, err := interactiveRuntime(ctx)
	if err != nil {
		return ctx, noop, err
	}
	if (g.readers == nil && g.writers == nil && g.resolvers == nil) || g.identity.Session == nil || runtime.execution.source.RunID != g.identity.RunID || g.actor.Issuer == "" || g.actor.Subject == "" {
		return ctx, noop, errors.New("session source operation binding unavailable")
	}
	sourceContext := sessionops.SourceContext{Identity: g.identity, Actor: g.actor, Lease: runtime.lease, RetainedGaggle: *runtime.execution.gaggle.DeepCopy()}
	reader, writer, resolver, err := g.sourceOperations(ctx, sourceContext)
	if err != nil {
		return ctx, noop, err
	}
	if reader == nil && writer == nil && resolver == nil {
		return ctx, noop, nil
	}
	var sources []string
	if config := runtime.execution.gaggle.Spec.Workbench; config != nil {
		for _, source := range config.Sources {
			if source.Kind == "backlog" {
				sources = append(sources, source.Name)
			}
		}
	}
	recorder, ok := g.writer.(sessionops.Recorder)
	if !ok {
		return ctx, noop, errors.New("session operation provenance recorder missing")
	}
	var writeSources, resolveSources []string
	if resolver != nil {
		resolveSources = append([]string(nil), sources...)
	}
	if writer != nil {
		writeSources = append([]string(nil), sources...)
	}
	access, close, err := g.operations.Open(sessionops.Invocation{Identity: g.identity, Actor: g.actor, SourceBindings: sources, WriteBindings: writeSources, Writer: writer, Resolver: resolver, ResolveBindings: resolveSources, StageSequence: started.Seq, Attempt: int(env.Attempt), Lease: runtime.lease, Reader: reader, Recorder: recorder})
	if err != nil {
		return ctx, noop, err
	}
	return context.WithValue(ctx, sessionToolContextKey{}, sessionToolGrant{run: g.identity.RunID, access: access}), close, nil
}
func sessionOperationsFor(ctx context.Context, run string) (*mcpio.SessionOperationAccess, error) {
	grant, ok := ctx.Value(sessionToolContextKey{}).(sessionToolGrant)
	if !ok {
		return nil, nil
	}
	if grant.run != run || grant.access == nil {
		return nil, errors.New("session operation invocation differs")
	}
	copy := *grant.access
	copy.BacklogSources = append([]string(nil), grant.access.BacklogSources...)
	copy.BacklogWriteSources = append([]string(nil), grant.access.BacklogWriteSources...)
	copy.BacklogResolveSources = append([]string(nil), grant.access.BacklogResolveSources...)
	if err := copy.Validate(run); err != nil {
		return nil, err
	}
	return &copy, nil
}

func (g sessionGoober) sourceOperations(ctx context.Context, source sessionops.SourceContext) (sessionops.BacklogReader, sessionops.BacklogWriter, sessionops.BacklogResolver, error) {
	var reader sessionops.BacklogReader
	var writer sessionops.BacklogWriter
	var resolver sessionops.BacklogResolver
	var err error
	if g.readers != nil {
		reader, err = g.readers(ctx, source)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if g.writers != nil {
		writer, err = g.writers(ctx, source)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if g.resolvers != nil {
		resolver, err = g.resolvers(ctx, source)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return reader, writer, resolver, nil
}
