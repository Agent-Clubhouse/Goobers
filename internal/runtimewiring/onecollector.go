package runtimewiring

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

// ConfigureOneCollector is independent of public telemetry opt-out and never
// resolves credentials unless the separately provisioned profile opts in.
func ConfigureOneCollector(ctx context.Context, cfg *telemetry.Config, source *instance.OneCollectorConfig, registry *journal.RegistryScrubber, stores credentials.StoreResolver) error {
	if source == nil {
		return nil
	}
	profile, err := onecollector.LoadProfile(source.ProfileFile)
	if err != nil {
		return fmt.Errorf("%w: %w", telemetry.ErrOTLPUnavailable, err)
	}
	if !profile.Enabled() {
		return nil
	}
	return ConfigureOneCollectorProfile(ctx, cfg, *profile, registry, stores)
}

func ConfigureOneCollectorProfile(ctx context.Context, cfg *telemetry.Config, profile onecollector.Profile, registry *journal.RegistryScrubber, stores credentials.StoreResolver) error {
	if !profile.Enabled() {
		return onecollector.ErrDisabled
	}
	ref := func(key onecollector.KeyRef, name string) credentials.TokenRef {
		return credentials.TokenRef{Name: name, Env: key.Env, File: key.File, Store: key.Store, Keychain: key.Keychain}
	}
	routing := ref(profile.RoutingKey, "onecollector.routing")
	instrumentation := ref(profile.InstrumentationKey, "onecollector.instrumentation")
	resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{routing, instrumentation}, stores)
	if err != nil {
		return errors.Join(telemetry.ErrOTLPUnavailable, errors.New("onecollector: invalid credential references"))
	}
	values := make([]string, 2)
	for i, name := range []string{routing.Name, instrumentation.Name} {
		values[i], err = resolver.Resolve(ctx, name)
		if err != nil || values[i] == "" {
			return errors.Join(telemetry.ErrOTLPUnavailable, errors.New("onecollector: credential resolution failed"))
		}
		if registry != nil {
			registry.Register([]byte(values[i]))
		}
	}
	cfg.OneCollector = &telemetry.OneCollectorOptions{Profile: profile, Options: onecollector.Options{
		EnableEnv: profile.EnableEnv, Endpoint: profile.Endpoint, RoutingKey: values[0], InstrumentationKey: values[1],
	}}
	return nil
}
