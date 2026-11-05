package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// Existing command integration fixtures use these adapters to configure the
// extracted services with the same production locks and provider coordination.
func fileClaimLedgerWithResolver(l instance.Layout, resolver claimsclient.SharedClaimResolver, opts ...localscheduler.LedgerOption) (claimsclient.Ledger, error) {
	return claimFiles().FileWithResolver(l, resolver, opts...)
}

func recoverClaimsWithResolver(
	l instance.Layout,
	log *journal.InstanceLog,
	now time.Time,
	interventionActive func(string) bool,
	gate *localscheduler.RecoveryGate,
	resolver claimsclient.SharedClaimResolver,
) ([]localscheduler.ClaimEntry, error) {
	return claimLifecycle().RecoverWithResolver(l, log, now, interventionActive, gate, resolver)
}

func executionGenerationPinsReader(layout instance.Layout) func(context.Context) (map[string]bool, error) {
	return generationService().PinsReader(layout)
}

func retainExecutionGeneration(ctx context.Context, layout instance.Layout, retainer *configgeneration.Retainer) (instance.Layout, string, error) {
	return generationService().Retain(ctx, layout, retainer)
}

func schedulerStateLock(l instance.Layout) func(string, string, func() error) error {
	return schedulerStateFiles().Lock(l)
}
