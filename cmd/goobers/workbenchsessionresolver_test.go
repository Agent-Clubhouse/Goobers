package main

import (
	"testing"

	"github.com/goobers/goobers/internal/sessionops"
)

func TestSessionResolverFactoryRefusesAbsentLiveLease(t *testing.T) {
	if resolver, err := workbenchSessionResolver(nil)(t.Context(), sessionops.SourceContext{}); err == nil || resolver != nil {
		t.Fatal(resolver, err)
	}
}
