package eventing

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

var consumerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// TargetPins is resolved from a trusted retained configuration generation.
// Configuration and publications cannot supply their own digest overrides.
type TargetPins struct {
	WorkflowDigest, GooberDigest string
}

// CompileConfiguredCatalog resolves every local workflow before publishing an
// immutable subscription generation. The resolver must be scoped to gaggle.
func CompileConfiguredCatalog(gaggle, generation string, policy *apiv1.GaggleEvents, resolve func(string) (TargetPins, error)) (*Catalog, error) {
	if !boundedText(generation, 256) {
		return nil, errors.New("eventing: retained generation is required")
	}
	var definitions []apiv1.EventSubscription
	if policy != nil {
		definitions = policy.Subscriptions
	}
	if len(definitions) > MaxSubscriptions {
		return nil, errors.New("eventing: too many configured subscriptions")
	}
	subscriptions := make([]Subscription, 0, len(definitions))
	for _, definition := range definitions {
		sub, err := configuredSubscription(definition)
		if err != nil {
			return nil, err
		}
		if resolve == nil {
			return nil, errors.New("eventing: target resolver is required")
		}
		pins, err := resolve(definition.Workflow)
		if err != nil {
			return nil, fmt.Errorf("eventing: subscription %q target: %w", definition.Name, err)
		}
		sub.Target = Route{Consumer: definition.Name, Workflow: definition.Workflow, WorkflowDigest: pins.WorkflowDigest, GooberDigest: pins.GooberDigest, ConfigGeneration: generation}
		raw, err := json.Marshal(sub)
		if err != nil {
			return nil, err
		}
		sub.Target.Revision = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
		subscriptions = append(subscriptions, sub)
	}
	return CompileCatalog(gaggle, generation, subscriptions)
}

// ValidateConfiguration checks authoring semantics without resolving digests.
// Local target existence is checked by the calling configuration loader.
func ValidateConfiguration(policy *apiv1.GaggleEvents) error {
	if err := ValidatePublishers(policy); err != nil {
		return err
	}
	_, err := CompileConfiguredCatalog("validation", "validation", policy, func(string) (TargetPins, error) {
		return TargetPins{WorkflowDigest: "validation", GooberDigest: "validation"}, nil
	})
	return err
}

func configuredSubscription(def apiv1.EventSubscription) (Subscription, error) {
	if !consumerName.MatchString(def.Name) || !boundedText(def.Workflow, 128) {
		return Subscription{}, errors.New("eventing: invalid consumer or workflow name")
	}
	filter, err := configuredFilter(def.Filter)
	if err != nil {
		return Subscription{}, err
	}
	debounce, err := configuredDebounce(def.Debounce)
	if err != nil {
		return Subscription{}, err
	}
	return Subscription{Filter: filter, Debounce: debounce}, nil
}

func configuredFilter(def apiv1.EventSubscriptionFilter) (Filter, error) {
	if len(def.All)+len(def.Any) == 0 || len(def.All) > 15 || len(def.Any) > 15 {
		return Filter{}, errors.New("eventing: filter requires bounded all/any conditions")
	}
	conditions := func(matches []apiv1.EventAttributeMatch) []Filter {
		result := make([]Filter, 0, len(matches))
		for _, match := range matches {
			filter := Filter{Attribute: match.Attribute, Equals: match.Equals}
			if match.Not {
				equality := filter
				filter = Filter{Not: &equality}
			}
			result = append(result, filter)
		}
		return result
	}
	all := conditions(def.All)
	if len(def.Any) > 0 {
		all = append(all, Filter{Any: conditions(def.Any)})
	}
	return Filter{All: all}, nil
}

func configuredDebounce(def *apiv1.EventDebounce) (*DebouncePolicy, error) {
	if def == nil {
		return nil, nil
	}
	window, err := configuredDuration(def.Window, 5*time.Second)
	if err != nil {
		return nil, err
	}
	maxWait, err := configuredDuration(def.MaxWait, 30*time.Second)
	if err != nil {
		return nil, err
	}
	count, mode := 100, def.InputMode
	if def.MaxEvents != nil {
		count = int(*def.MaxEvents)
	}
	if mode == "" {
		mode = "all"
	}
	return &DebouncePolicy{KeyAttribute: def.KeyAttribute, ConstantKey: def.ConstantKey, Window: window, MaxWait: maxWait, MaxEvents: count, InputMode: mode}, nil
}

func configuredDuration(value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	if len(value) > 32 {
		return 0, errors.New("eventing: duration exceeds length bound")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, errors.New("eventing: invalid debounce duration")
	}
	return parsed, nil
}
